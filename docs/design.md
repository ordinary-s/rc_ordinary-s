# 系统设计

## 边界和组件

一个 Go 进程通过显式依赖注入运行 `net/http` API、MySQL Outbox 发布循环和 RocketMQ PushConsumer。MySQL 保存任务事实与发布责任；RocketMQ 保存分发队列及执行异常的原生重试。外部 HTTP 调用在事务外。项目按单实例设计，不提供多实例租约/限流协调。

## 处理流程

```text
调用方
  | Bearer 鉴权、请求/目标校验、Idempotency-Key
  v
API -- begin tx --> notification(task + 请求指纹) + outbox(event_id=task_id)
  |                          | 同一个事务提交成功后返回 202
  |                          v
  |                 Publisher 按 idx_outbox_pending_due 分页扫描
  |                          | SendSync 返回 SendOK 后标记 PUBLISHED
  |                          v
  |                 RocketMQ notification_delivery (单条消息)
  |                          v
  |                 task 锁 + DB 终态/过期/机构开关检查
  |                          v
  |                 每次先记录 STARTED，再 HTTP(S)
  |                          v
  +<------查询状态/尝试------ 保存 DELIVERED / FAILED / FAST_FAILED /
                              EXPIRED / UNCERTAIN
```

Outbox 网络发送不处于数据库事务中。事件体只带 event/task/trace 标识。发送状态不覆盖业务投递状态。确认不明时保留 Outbox 发布责任，同一 event_id 可安全再次发送。

## 状态和确认

```text
PENDING --过期--> EXPIRED
PENDING --机构 fast_fail_enabled--> FAST_FAILED
PENDING --写入第 n 条 STARTED 并发 HTTP--> PROCESSING
PROCESSING --2xx--> DELIVERED
PROCESSING --非重试 4xx--> FAILED
PROCESSING --第 3 次仍为 429/5xx--> FAILED
PROCESSING --有预算的 429/5xx--> PROCESSING（完成当前 attempt，退避后继续）
PROCESSING --远端结果不明/中断恢复--> UNCERTAIN
任意已保存终态 --重复 MQ 消息--> 直接确认，不发 HTTP
```

HTTP 终态和快速失败/过期是已完成业务结果，返回 `ConsumeSuccess`。MySQL 读写失败、处理上下文取消、结果无法可靠保存返回 `ConsumeRetryLater`，由选定 RocketMQ SDK/Broker 机制重投，耗尽后进入 `%DLQ%notification_delivery_workers`。HTTP 业务次数在通知行和 `delivery_attempt` 保存，不随 MQ 重投清零。处理发现旧 STARTED 时先记 UNKNOWN/UNCERTAIN，不盲目重发。

HTTP 已成功但结果事务暂时失败时，`completeAttempt` 在同一次消费中最多保存 3 次（首次加 50ms、100ms 两段可取消等待），只重试数据库写入，不重发 HTTP。仍未保存才返回 MQ 重试；重投发现 STARTED 后转 UNCERTAIN 并确认消费。相同 attempt 结果若数据库提交成功但客户端未收到确认，Store 会读回相同已保存结果并按幂等成功处理。真实 MySQL 集成分别验证了暂时失败后第三次保存成功，以及三次失败后 MQ 重投且外部 HTTP 调用仍为一次。

每次传输使用稳定 task ID 的下游 `Idempotency-Key`。只有下游实际实现去重时它才有效；即便如此，本系统仍不声称外部 exactly-once。Go `net/http` 对部分幂等请求可能内部重试，因此“最多三次”是业务层尝试计数，不等同于网络包或供应商实际执行次数。

## HTTP 处理策略

- 业务最大尝试数固定为 3：首次 + 两次重试。默认两段 10s/20s 指数等待并加有界抖动；Docker 验收使用更短的配置，但走相同状态转换。
- 429/5xx 可重试；永久 4xx 立即 FAILED。有效 `Retry-After` 不被提前绕过；若等待会超过剩余时效，保存终止失败。
- DNS、连接断开、HTTP timeout 等可能无法判断远端是否执行，保守保存 UNCERTAIN，不进行重复有害请求。
- 目标是初始化时固定的 HTTP(S) 主机/端口/协议白名单；关闭自动重定向，TLS 校验默认开启，不使用环境代理绕过目标限制。
- 请求体要求 UTF-8 原始文本，当前最大 256 KiB；请求头最多 64 个且总计 16 KiB。不支持 multipart/大文件。

## 最小数据模型

| 表 | 重要字段/约束 | 查询目的 |
|---|---|---|
| `destination` | 目标 URL、fast-fail、审计人/原因/UTC 时间 | 受控目标和管理员开关 |
| `notification` | task、客户端幂等键及二进制比较、指纹、单份请求头/Body、状态、尝试数、过期和处置结论 | 幂等、客户端分页、状态机 |
| `delivery_attempt` | `(task_id, attempt_no)` 主键、STARTED/结果/UTC 时间 | 每次实际启动的业务 HTTP 尝试 |
| `outbox` | event/task 唯一键、publish 状态、下次发送时间/次数 | `idx_outbox_pending_due` 扫描待发事件 |

全部是 InnoDB；utf8mb4 和明确 collation；ID/幂等键按大小写敏感约定比较；Body 原文只保存在 notification 一份；时间使用 UTC `DATETIME(6)` 与 `UTC_TIMESTAMP(6)`。目标以 migration seed，运行时使用限权用户，迁移另用 root/init 用户。迁移 DDL 会隐式提交，失败恢复靠可重跑的 `IF NOT EXISTS` 和 schema migration 版本表，不宣称全量原子回滚。

## 管理和运行边界

`/healthz` 只报告进程存活；`/readyz` 检查 MySQL 连接和 RocketMQ client/NameServer/Broker 可达。Broker 暂时不可用时 API 可继续向 MySQL 可靠受理，Outbox 保持待发并退避；readyz 返回 503。若单实例进程本身退出，当然不能接收新请求。

失败/待核对任务通过客户端隔离查询；管理员将核对结论写回任务。DLQ 是 Broker 原生死信，需要运维负责人使用 RocketMQ 管理工具查看/导出；本 MVP 不自动搬运为数据库工单，也不自动重放。

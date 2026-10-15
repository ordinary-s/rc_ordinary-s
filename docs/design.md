# 设计说明：上游直入 RocketMQ 的通知服务

## 1. 目标与边界

上游服务在自己的业务事务中决定是否通知，并负责把完整通知事件写入自己的 Outbox。它将事件发布到 RocketMQ。本服务消费该事件，校验协议及目标，将通知数据先保存到 MySQL，再调用受控 HTTP(S) 目标并记录最终结果。

本服务不提供 HTTP 通知提交入口，也不保存新的 Outbox 发布责任。HTTPS API 仅用于查询、快速失败配置和人工结论。当前部署按单 Go 实例、单 MySQL、单 Broker 的 MVP 设计，不宣称多实例全局限流或高可用。

## 2. 端到端流程

    上游业务事务
      └─ 同事务写上游 Outbox
           └─ 发布完整 event_id 事件到 notification_ingress_v1
                └─ 单条 PushConsumer 回调（有界并发）
                     ├─ 解码 schema_version=1 并校验 UTC 截止时间
                     ├─ 按配置绑定 client_id；验证 destination 与请求约束
                     ├─ MySQL 事务：插入 notification（唯一 client_id + source_event_id）
                     │    ├─ 同 ID 同指纹：复用已存在 task
                     │    └─ 同 ID 异指纹：协议冲突，拒绝覆盖和发送
                     ├─ 检查终态、有效期、机构 fast_fail_enabled
                     ├─ 最多 3 次 HTTP 业务尝试，记录每次尝试
                     └─ 终态已可靠保存 -> ConsumeSuccess
                          执行异常/结果未可靠保存 -> ConsumeRetryLater
                               -> RocketMQ 原生重试 -> 耗尽进入 DLQ

    HTTPS 查询/管理员 API ───────────────────────> MySQL

## 3. MQ 协议

Topic 为 notification_ingress_v1，消费组为 notification_ingress_workers_v1。消费采用 RocketMQ 官方 rocketmq-client-go/v2 v2.1.2 PushConsumer 的集群模式、单消息回调、单次拉取 1 条和有界 goroutine 数。Broker/NameServer 固定为 RocketMQ 5.5.0；容器内 Broker 通告地址可由应用和验收容器访问。Topic/Group 由初始化步骤显式创建。

事件 JSON 至少包含：

    {
      "schema_version": 1,
      "event_id": "stable-upstream-event-id",
      "trace_id": "trace-identifier",
      "destination_id": "mock-target",
      "method": "POST",
      "headers": {"Content-Type": "application/json"},
      "body": "{\"example\":true}",
      "expires_at": "2026-10-15T16:00:00Z"
    }

event_id 必须由上游稳定生成并在重发时保持不变；client_id 不在消息体中，由 MQ_TRUSTED_CLIENT_ID 配置绑定。事件体有字节上限，只允许 UTF-8，拒绝未知字段、额外 JSON 值或不支持的版本。Body 仅为受限大小的 UTF-8 文本，不把 Header/Body 写入日志。

幂等指纹覆盖 destination_id、规范化 HTTP method、规范化 Header、原始 UTF-8 Body 和 UTC 绝对 expires_at，不覆盖 trace_id。数据库唯一键 (client_id, source_event_id) 并保存 SHA-256 指纹。同键同指纹复用原任务；同键异指纹是协议冲突，不覆盖数据库或发 HTTP，进入原生异常重试和 DLQ。坏版本事件同样由原生异常策略处理。

## 4. 事务与 ACK 边界

服务先把校验通过的事件写入 MySQL notification 记录。数据库唯一键冲突时读取原任务并比较指纹。任务插入成功或复用同指纹原任务后，才进入 Delivery Engine。Consumer 只在 Engine 返回时已得到可靠持久终态后返回 ConsumeSuccess。读取、写入、取消或执行错误导致处理未完成时返回 ConsumeRetryLater，由 Broker 管理重投和 DLQ。

消息已到 Broker、数据库未写入时，重投事件并重新执行入口落库。任务已落库、HTTP 尚未开始的进程中断，状态保留 PENDING 并按持久预算恢复。持久化 STARTED 尝试说明 HTTP 可能已经产生副作用；恢复时保守标记为 UNKNOWN/UNCERTAIN，不盲目再发。HTTP 结果已知但 MySQL 暂时失败时，优先有限重试保存已有结果，不重复调用 HTTP。远端成功到本地 commit 之间仍不可跨系统原子化。

早期 migration 创建的 outbox 表保留历史兼容；新服务无 Outbox publisher，不插入或扫描该表。

## 5. 状态和处理规则

| 当前条件/状态 | 动作 | 结果/ACK |
|---|---|---|
| DELIVERED、FAILED、FAST_FAILED、EXPIRED、UNCERTAIN | 终态不再调用 HTTP、不改回处理中 | ACK |
| 新事件或同指纹重复事件 | 按持久状态继续执行 | 保存结果后 ACK |
| 已到 expires_at | 保存 EXPIRED，不启动新 HTTP | ACK |
| 机构开关开启 | 保存 FAST_FAILED，不启动 HTTP | ACK；关闭开关不会复活此任务 |
| HTTP 2xx | 保存尝试结果和 DELIVERED | ACK |
| 可重试 429/5xx | 最多三次总尝试；第三次仍失败保存 FAILED | ACK |
| 明确不可重试响应 | 保存 FAILED 和受限摘要 | ACK |
| 远端是否执行无法确认 | 保存 UNCERTAIN，停止自动发送 | ACK |
| DB/协议执行错误且无可靠终态 | 回调返回失败 | RocketMQ 原生重试，耗尽进 DLQ |

HTTP 次数是服务启动的业务尝试数，不等同 Broker 重投次数。默认最大次数为 3；默认两段退避为 10 秒和 20 秒并加有限抖动。每次请求超时、总处理预算和任务 TTL 可配置；有效 Retry-After 不被提前绕过。context 取消可终止等待。Go net/http 使用复用有界连接，不跳过 TLS 校验，不自动跟随重定向。Transport 在部分网络失败下可能内部重试，因此不把业务计数描述成所有故障下绝对物理发送上限。

## 6. 最小数据模型

| 表 | 主要内容 | 唯一/索引 |
|---|---|---|
| destination | 目标 URL、快速失败开关及最后一次管理员原因/时间 | 主键 destination_id |
| notification | trusted client、source event ID、指纹、目标、method、headers、单份原文 body、trace、状态、次数、有效期、核对结论 | 主键 task_id；唯一 (client_id, source_event_id)；客户端分页/状态索引 |
| delivery_attempt | 每次 HTTP 启动序号、状态、HTTP code、短摘要、UTC 起止时间 | 主键 (task_id, attempt_no) |
| outbox | 旧版受理/发布记录 | 新版运行时不写不读 |

表、列及索引采用小写 snake_case、utf8mb4 和明确 collation 是本项目约定。入口去重标识按大小写敏感比较。时刻使用 UTC DATETIME(6)，SQL 参数化，分页有稳定排序。migration 追加执行；MySQL DDL 有隐式提交行为，不假定整份迁移可事务回滚。

## 7. HTTPS API 安全与管理边界

对外 API 最低 TLS 版本为 1.3。除健康探针外，/api/* 要求 Bearer token 和 RFC 9421 HMAC-SHA-256 请求签名；Content-Digest 使用 RFC 9530，签名含 method、authority、目标 URI、path、Authorization、摘要、有效期和 nonce。nonce 写入 MySQL 并由唯一键防重放。客户端与管理员 key ID/token 分开配置。HMAC 不加密内容，不额外发明请求体加密。HTTPS 可在受控网关终止，但网关到应用仍需加密且入口受限；应用不信任任意转发 URI Header。

Fast-fail 修改需要管理员身份和非空原因，存储操作人、原因、时间。resolve 只接受指定终态并保存结论，不改变业务状态、不清除尝试、不重发。/readyz 检查 MySQL、消费者启动状态和 Broker；依赖不可用时返回 503。/healthz 只检查进程存活。

## 8. 切换与人工恢复

旧版使用 ID-only Topic/Group 与本地 Outbox；新版使用独立 Topic/Group 及完整 schema。升级前应停止旧版接收新请求，让其发布并处理旧任务，然后核对旧 Group 积压和 outbox.publish_state='PENDING' 记录。不要删除旧 outbox 表、历史消息、卷或改写事件格式。未完成记录由旧版本继续完成，或由负责人审查后按新协议显式导出；不要自动复活旧终态任务。migration 004_source_event_id.sql 只增加可空列和唯一索引以兼容历史数据。

失败/待核对任务可通过客户端查询，管理员可写核对结论。DLQ 用 Broker mqadmin consumerStatus 和 mqadmin printMsg 单独查阅；DLQ 不会自动写入通知表或人工工单。DLQ 可能包含没有有效 task_id 的坏协议事件，须由负责人在受控流程登记处置。第一版不自动搬运、审批或重放 DLQ。

## 9. 扩展方向（未实现）

未来按上游 client、机构/共享账号设置入口配额、出口速率和在途并发，首次与重试共用同一额度。按机构风险、响应时间和优先级分组 Worker/队列；只拆队列但共享执行资源不构成隔离。瓶颈被验证后再拆分 API/消费者、协调全局额度、增加自动熔断或归档。当前没有 Redis、集群锁、管理后台、自动熔断、自动 DLQ 工单或高可用配置。

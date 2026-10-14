# API 通知系统：完整设计与实现边界

系统通过 MySQL 事务可靠受理通知，再由 RocketMQ 驱动异步 HTTP 投递。核心设计是将消息发布、HTTP 尝试和结果保存分开管理：允许消息重复，限制业务尝试次数，并在外部结果不明时停止自动发送。以下说明处理流程、故障恢复和当前能力边界；验证结果见[验收记录](verification.md)。

## 1. 业务背景与系统定位

内部业务系统在注册、付款、购买等事件后，需要向广告、CRM、库存等外部机构发送 HTTP(S) 通知。机构要求的地址、Header 和 Body 不同，调用方不希望在业务请求中等待外部接口，也不希望重复实现持久化、失败记录和有限重试。

本系统接收调用方构造好的请求，持久化为任务，再异步投递。它是受控 HTTP 通知执行器，不负责理解每家供应商的业务响应。`202 Accepted` 表示任务与发布责任已落库；`DELIVERED` 表示收到 HTTP 2xx 并成功保存结果。两者都不能代替供应商对最终业务完成的确认，尤其外部 202 仍可能只是受理。

MVP 将 API、Outbox 发布器和消费者放在一个 Go 进程。模块职责分开，部署保持单实例；数据库和队列是两个外部依赖。

## 2. 解决与不解决的问题

| 本系统负责 | 不负责及原因 |
|---|---|
| 鉴权、输入/目标校验、入口幂等、任务与 Outbox 原子保存 | 上游业务事务与提交 API 的跨系统原子性；上游未提交的事件无法由本系统恢复，上游仍需可靠交接机制 |
| 发布补偿、异步分发、有限 HTTP 尝试和可查询结果 | 任意故障下最终必达；次数耗尽、过期和快速失败都可能不送达 |
| 重复消息识别、已用次数持久化、结果不明时停止自动发送 | 外部副作用恰好一次；本地唯一键不能代替供应商去重 |
| 机构人工快速失败、失败结论记录、原生 DLQ 排查 | 自动熔断、自动恢复、终态自动重放、审批平台；它们改变故障与预算边界 |
| 原始 UTF-8 文本请求，包括 JSON、XML、表单和纯文本 | 二进制/multipart 大文件、通用供应商适配、动态签名和 OAuth 自动刷新 |
| 单实例有界消费、连接复用、轻量日志和真实依赖验收 | 严格业务顺序、多实例全局配额、跨机房容灾和未经压测的吞吐承诺 |

当前身份配置只映射一个客户端 token 到一个 `CLIENT_ID`，一个管理员 token 到一个 `ADMIN_ACTOR`。查询有调用方隔离条件，但没有多租户身份管理和按调用方分配机构权限的平台。原始请求可能含敏感信息，持久化存储及备份须受控。

## 3. 技术架构与选型依据

| 组件 | 当前作用 | 选择理由与代价 |
|---|---|---|
| Go，标准库 net/http、context、log/slog | API、定时发布、消费与 HTTP 执行 | 显式依赖和取消链路容易核对；需要自行写清事务与错误分支 |
| MySQL/InnoDB | 任务事实、幂等唯一约束、尝试、机构配置、Outbox | 本地事务覆盖可靠受理；增加索引、归档、备份和迁移维护责任 |
| Apache RocketMQ | 分发、消费进度、执行异常原生重试/DLQ | 保留成熟队列恢复能力；需管理 NameServer、Broker、SDK 和路由兼容 |
| Transactional Outbox | 发布责任与任务同事务提交，后台补发 | 解决数据库与 MQ 双写窗口；允许重复消息，增加扫描与状态更新成本 |

当前使用 Go 1.27.2、MySQL 8.4.8、RocketMQ 5.5.0 和 `rocketmq-client-go/v2 v2.1.2`。客户端采用 Remoting 协议，通过 NameServer 发现 Broker；当前部署不需要 gRPC Proxy。版本与镜像 digest 固定在 [Dockerfile](../Dockerfile)、[Compose 配置](../compose.yaml) 和 [go.mod](../go.mod)，运行环境见[验收记录](verification.md)。选型过程与替代方案见[设计决策](decisions.md)和[题目答复](assignment-answers.md)。

## 4. 完整处理流程

```text
上游业务方
    |
    v
API：Bearer -> 客户端身份 -> 输入/目标校验 -> Idempotency-Key
    |
    v
T1 本地事务：notification(PENDING, 次数0) + outbox(PENDING)
    | 失败：不报告受理成功；同键冲突：比较指纹，异内容409
    | 提交成功：202 + task_id/trace_id；同内容复用原任务
    v
Go Publisher：扫描到期的 PENDING Outbox，每批最多100条
    |
    +-- SendSync --> RocketMQ（Broker 接收后即可分发）
    |
    +-- SendOK 且标记成功 --> PUBLISHED
    |
    +-- SendSync 未确认 / PUBLISHED 落库失败 --> 保留发布责任，退避补发
```

消费可以在本地标记 PUBLISHED 之前开始。Broker 已接收消息、但发送确认或本地标记尚未完成时，发布器仍可能补发，因此消费侧需要处理重复消息。

```text
          RocketMQ 单条消费回调、有界并发
                 |
          单实例 task 锁 -> 读取任务/尝试记录
                 |
                 +-- 已终态 -----------------------------> 完成消费
                 +-- 遗留 STARTED -> UNKNOWN/UNCERTAIN落库 -> 完成消费
                 +-- 过期 -> EXPIRED落库 ----------------> 完成消费
                 +-- 命中机构开关 -> FAST_FAILED落库 ----> 完成消费
                 |
                 v
          等待持久化的 HTTP 重试到期时间（可取消）
                 | 到期后重新读取状态、时效和机构开关
                 v
          T2：预留第n次机会 + delivery_attempt(STARTED)
                 |
          事务外 HTTP，n最多为3
                 |
                 +-- 2xx -------------> T3保存 DELIVERED -> 完成消费
                 +-- 不可重试/次数耗尽 -> T3保存 FAILED ---> 完成消费
                 +-- 传输结果不明 ----> T3保存 UNCERTAIN -> 完成消费
                 +-- 429/5xx且有预算 -> T3保存失败和下次到期时间
                                                     | 回到等待/检查
读取、执行或结果保存未可靠完成
    -> ConsumeRetryLater -> RocketMQ原生重投 -> 耗尽进入原生DLQ
```

所有“完成消费”都以对应结果可靠保存为前提。发布成功和业务投递成功是不同事实，Outbox 状态不替代 notification 状态。

## 5. 接收接口与入口幂等

`POST /api/v1/notifications` 接收 JSON 信封：`destination_id`、`method`、`headers`、`body`。body 是原始文本字符串，解码后按原字节发送，不重新解析其中 JSON/XML/表单。允许 GET、POST、PUT、PATCH、DELETE、HEAD、OPTIONS。方法统一大写、Header 名规范化并拒绝规范化后的重名；禁止调用方控制 Host、Content-Length、Connection 等传输头。

客户端身份来自 Bearer 配置，不能在请求体中自报。`Idempotency-Key` 必填、TrimSpace 后最多 128 字节。数据库使用 `(client_id, idempotency_key)` 非空唯一约束和大小写敏感排序规则，避免“先查询再插入”的竞争。`request_fingerprint` 是目标 ID、规范化方法/Header 和解码后 Body 的 SHA-256，trace 不参与。Body 内空白仍影响指纹，系统不判断两份文本是否具有相同业务含义。

同键同内容返回原任务及原 trace，仍为 202，`created=false`；同键异内容返回 409。响应丢失后重提不会新建任务或 Outbox。指纹只包含 destination_id，不包含数据库中的实际目标 URL，因此目标配置变化不产生新的入口幂等身份。

三层去重责任分开：入口唯一键防重复受理；任务终态/尝试记录防重复消息重新给予预算；下游 `Idempotency-Key: <task_id>` 只有在对方确实实现去重时才约束副作用。换一个入口键会得到新任务，不能视为无副作用的“恢复”。

## 6. MySQL 与 Outbox 的事务边界

| 边界 | 同事务内容 | 明确在事务外的动作 |
|---|---|---|
| T1 Store.Accept | notification 与唯一 Outbox；提交前失败整体回滚 | API 响应、MQ 发布 |
| T2 Store.StartAttempt | 条件更新次数与 PROCESSING；插入 STARTED | 外部 HTTP |
| T3 Store.CompleteAttempt | 完成本次尝试、更新任务结果及 notification.next_attempt_at | HTTP 已结束，不持有事务等待外部系统 |
| MarkInFlightUncertain | STARTED 转 UNKNOWN；任务转 UNCERTAIN | 人工核对外部结果 |
| 发布确认/退避 | 更新单条 Outbox 发布状态或补发时间 | SendSync |

Publisher 默认每秒扫描一次，按 `idx_outbox_pending_due` 获取 `publish_state='PENDING' AND next_attempt_at<=UTC_TIMESTAMP(6)`，最多 100 条，逐条发送。它不扫描 FAILED 任务，不为业务重试增加 Outbox。事件只带 `event_id/task_id/trace_id`，同一任务的 event_id 稳定且等于 task_id。

发送超时可能意味着 Broker 已收到消息。本地 PUBLISHED 更新失败也可能在 Broker 接收之后发生。两种情况都保留发布责任，并尝试持久化 1、2、4…最高 60 秒的退避；若连退避更新也失败，下次扫描仍可能重发。因此，逐条退避依赖数据库能够保存下次发布时间，消费者也必须容忍重复事件。

Outbox 在依赖恢复且发布循环持续运行时补偿未完成发布，不保证灾难性丢库、永久停机或超过 Broker 保留策略后仍必达。当前没有 Outbox 归档任务和多实例发布领取协议。

## 7. RocketMQ 发布、消费确认与异常重试

发布使用 SendSync，只有非空结果且 `Status==SendOK` 才尝试标记 PUBLISHED；Producer 的 SDK 额外发送重试设为 0，由 Outbox 统一承担应用发布补偿。发送 context 为 7 秒，SDK 发送 timeout 为 5 秒。

消费者为 Clustering PushConsumer；`WithConsumeMessageBatchMaxSize(1)`、`WithPullBatchSize(1)`，消费 goroutine 默认 4，可配置 1–64。回调额外检查消息数为 1。每条消息独立返回处理结果，执行异常统一交给 RocketMQ 原生重试，避免批次内部分成功的协调成本。

| 处理结果 | Engine 返回 | SDK 回调 |
|---|---|---|
| DELIVERED、FAILED、FAST_FAILED、EXPIRED、UNCERTAIN 已保存 | nil | ConsumeSuccess |
| 已终态的重复消息 | nil | ConsumeSuccess，不发 HTTP |
| MySQL 读写异常、未完成的上下文取消 | error | ConsumeRetryLater |
| 消息损坏、缺少必要 ID、回调批量违约 | 在 MQ 边界返回 error | ConsumeRetryLater |

这里的消费确认通过上述 SDK 返回值完成。`MQ_MAX_RECONSUME` 默认 8，与 HTTP 三次上限独立；验收配置设为 2，实际重消费次数与 DLQ 结果见[验收记录](verification.md)。这组结果适用于当前锁定的 Broker/SDK 组合。原生 DLQ 为 `%DLQ%notification_delivery_workers`。

[RocketMQ 消费重试文档](https://rocketmq.apache.org/docs/featureBehavior/10consumerretrypolicy/)说明了异常重投与死信的职责。原生重试用于恢复未完成处理；流量控制需要单独的限流机制。项目的具体配置对应 [Go v2.1.2 SDK](https://github.com/apache/rocketmq-client-go/blob/v2.1.2/consumer/option.go)。

## 8. 每个任务最多三次 HTTP 业务尝试

```text
第1次 --可重试失败--> 等待10s + 0..250ms抖动
第2次 --可重试失败--> 等待20s + 0..250ms抖动
第3次 --仍失败------> FAILED，保存后完成消费
任何一次2xx---------> DELIVERED，保存后完成消费
```

这是上限，不是所有错误都必须耗尽三次。默认两段退避呈指数增长；HTTP_BACKOFFS 可自定义，代码只校验恰有两项且非负，不强制任意配置保持倍增。429 或 5xx 可重试；其余非 2xx（包括禁止跟随的重定向）终止为 FAILED。有效 Retry-After（非负整数秒或未来 HTTP-date）与本地退避取较大值。

继续前检查等待加剩余尝试预算是否容纳于有效期和处理预算；容纳不了则保存 FAILED，原因是 `RETRY_DELAY_EXCEEDS_TASK_WINDOW` 或 `RETRY_DELAY_EXCEEDS_PROCESSING_BUDGET`。不提前忽略 Retry-After，不新建业务延迟队列。

次数在 T2 预留并持久化。HTTP next_attempt_at 与当前失败在 T3 提交，MQ 重投或恢复仍等原到期时间；开始下次时清空。数据库“结果保存”的 3 次尝试是另一件事：间隔 50ms/100ms，只重试数据库事务，不再调用 HTTP、不增加业务次数。

| 参数 | 当前默认值与约束 |
|---|---|
| HTTP_TIMEOUT | 10s；每次请求上限 |
| HTTP_BACKOFFS | 10s,20s；两段等待，各加最多 250ms 抖动 |
| DELIVERY_BUDGET | 90s；覆盖三次 HTTP、两段退避、落库等待和余量 |
| MQ_CONSUME_TIMEOUT | 2m；启动检查至少比处理预算长 5s |
| TASK_TTL | 30m；受理时确定，不随重投、恢复开关或查询延长 |

基础预算为 `3×10s+10s+20s=60s`。启动校验另加 950ms 与 5s 余量；数据库耗时仍受 context 约束，不保证任意故障下正常完成。处理预算在取得 task 锁之后开始，锁等待不计入。等待使用可取消 timer，SIGTERM 会取消执行。

**三次上限针对应用层 `client.Do` 调用和持久化尝试计数。** 请求带有稳定幂等头和可重放 Body，Go Transport 在部分连接故障下可能内部重试。因此，当前实现不保证物理 HTTP 请求次数最多为三次；若业务要求这一硬上限，需要补充底层重放控制与故障测试。参见 [Go Transport 文档](https://pkg.go.dev/net/http#Transport)与[已知限制](implementation-review.md)。

## 9. 机构快速失败与终态规则

管理员通过 `PATCH /api/v1/admin/destinations/{id}/fast-fail` 设置 enabled 和原因。表中保存 updated_by/update_reason/updated_at；身份来自 ADMIN_ACTOR，不采信自报操作人。当前只保存最新修改信息，不是独立、不可改写的审计流水。

每轮执行（包括退避后）读取机构配置。观察到 fast_fail_enabled=true，不启动下次 HTTP、不增加未发生的次数，保存 FAST_FAILED 后完成消费。单任务三次失败不会自动开启机构开关。配置按 destination 隔离，不改变其他机构开关；当前共享 Worker 仍可能产生资源干扰。

关闭开关只允许后续尚未终态的任务执行，旧 FAILED/FAST_FAILED 不复活。打开开关不能撤销已发出的 HTTP。读开关与发出之间没有同一同步屏障，管理员修改与发送竞争时，仍可能有越过检查点的请求继续发出；“配置提交瞬间严格零新请求”未实现，见[已知限制](implementation-review.md)。

快速失败期间没有业务探测流量，管理员需结合机构通知、既有监控或安全人工验证判断恢复。查看频率和负责人属于运维约定，代码没有自动探测、半开或自动恢复。

## 10. 重复消息与执行异常恢复

同进程按 task ID 加内存锁并维护引用计数，等待者全部释放后移除锁项；DB 条件更新和尝试主键提供持久校验。锁只在单实例有效，不是跨实例互斥协议。

| 故障窗口 | 持久状态/实际恢复 |
|---|---|
| T1 提交前失败 | 无完整受理；可用同键重提 |
| T1 提交后响应或进程丢失 | 原任务/Outbox 保留；同键复用，Publisher 补发 |
| Broker 收到事件，但确认或发布标记丢失 | 同 event 可再发布；终态/已用次数不重置 |
| STARTED 提交，HTTP 尚未发出或结果未保存时中断 | 无法可靠区分两种情况；一律 UNKNOWN/UNCERTAIN，保守消耗机会并停止自动发送 |
| HTTP 成功，结果事务暂时失败 | 本次消费最多 3 次结果保存，不转换成新 HTTP |
| 结果事务已提交但客户端未收到 DB 确认 | 再保存时读回尝试与任务结果，内容一致视为已保存 |
| 失败及下次时间已提交，退避中断 | 无 STARTED 遗留时，用剩余次数并遵守原到期时间 |
| 终态已落库、MQ 确认前中断 | 重投查终态直接完成，不发 HTTP |

PROCESSING 任务会根据尝试记录恢复：已完成的失败尝试可以在剩余预算内继续；遗留 STARTED 则表示无法确认上次调用是否执行，转为 UNKNOWN/UNCERTAIN 并停止自动发送。这一保守策略同样适用于支持幂等键的下游，代价是部分原本可以安全重试的任务仍需人工核对。

## 11. 数据表与状态设计

| 表 | 关键约束/字段 | 职责 |
|---|---|---|
| destination | destination_id 主键，target_url、fast_fail_enabled、最新管理员信息 | 受控目标和开关；没有 QPS、凭证刷新、每机构执行池配置 |
| notification | task_id 主键；客户端/幂等键唯一；指纹、单份 Header/Body、trace、状态、次数、expires_at、next_attempt_at、处置字段 | 受理事实、结果和恢复依据 |
| delivery_attempt | task_id/attempt_no 主键；state、http_status、error_summary、开始/结束时间 | 已启动尝试；耗时可由时间计算，无独立 cost_ms 列 |
| outbox | event_id/task_id 各自唯一；publish_state、publish_attempts、next_attempt_at、last_error | 首次事件发布责任 |
| schema_migrations | version 主键、applied_at | 迁移登记，不能让 DDL 自动获得事务回滚能力 |

notification.next_attempt_at 记录下次 HTTP 尝试时间，outbox.next_attempt_at 记录下次消息发布时间，两者分别管理投递和发布的退避。Body 只保存一份，MQ 事件和查询响应不带原始正文/Header。

| 当前状态 | 条件 | 下一状态/行为 |
|---|---|---|
| PENDING/PROCESSING | 新尝试预留成功 | PROCESSING，次数+1，记录 STARTED |
| PENDING/PROCESSING | 过期且无优先处理的遗留 STARTED | EXPIRED |
| PENDING/PROCESSING | 观察到开关且无遗留 STARTED | FAST_FAILED |
| PROCESSING | 2xx 且结果保存成功 | DELIVERED；尝试 SUCCEEDED |
| PROCESSING | 429/5xx，尚有次数和时间 | PROCESSING；本次尝试 FAILED，保存下次时间 |
| PROCESSING | 非重试响应、次数耗尽、等待预算不足 | FAILED；本次尝试 FAILED |
| PROCESSING | 传输结果不明或遗留 STARTED | UNCERTAIN；尝试 UNKNOWN |
| 任意终态 | 重复消息或关闭开关 | 保持终态，零新业务尝试 |
| FAILED/FAST_FAILED/EXPIRED/UNCERTAIN | 人工 resolve | 只更新 resolution/resolved_by/resolved_at，不改状态 |

UNCERTAIN 表示投递结果不明。人工核对结论以自由文本保存在独立处置字段中，不改变任务的投递状态，也不会使任务重新获得发送机会。

表为 InnoDB，表列小写 snake_case，明确 utf8mb4/排序规则；task_id、event_id、trace_id 等 ASCII 字段用 ascii_bin，client_id、destination_id、幂等键用 utf8mb4_0900_bin。时间是 UTC DATETIME(6)，写 UTC_TIMESTAMP(6)，Go DSN 用 parseTime/UTC。SQL 参数化、短事务，列表有客户端/状态/时间索引。

迁移见 [001](../migrations/001_initial.sql) 和 [002](../migrations/002_notification_retry_due.sql)。MySQL [DDL 隐式提交](https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html)，不能假设回滚整批迁移。002 的 ALTER 成功、登记失败后，重跑会重复加列；须核对 information_schema 中类型、精度、可空性和位置，对照 SQL/schema_migrations。仅在 schema 完全符合预期时补登记版本；否则先制定修复方案，不能盲目跳过或删列。新库验收不覆盖该中断窗口。

## 12. HTTP 错误分类与结果不明

| 观察结果 | 实际策略 | 说明 |
|---|---|---|
| 2xx，读取响应未报错 | DELIVERED | 不分析正文中的供应商业务状态 |
| 429、5xx | 剩余预算内重试，否则 FAILED | 尊重 Retry-After；最多三次业务尝试 |
| 其余非 2xx，包括 3xx/普通 4xx | FAILED | 不跟随跳转，不触发业务失败重投 |
| 请求构造/已存 Header 无效 | FAILED，不启动 HTTP | 记录分类，不记录原始 Header/URL |
| client.Do 报错，处理 context 有效 | UNKNOWN/UNCERTAIN | DNS、超时、断连统一保守处理，即使部分情况可能证明尚未发送 |
| 父 context 取消或响应读取出错 | 执行异常，保留 STARTED，MQ 重试 | 恢复后 UNKNOWN/UNCERTAIN，不直接再发 |
| 结果无法保存 | 有限重试保存，仍失败则 MQ 重试 | HTTP 成功与持久化成功分开 |

响应最多读取并丢弃 32 KiB，随后关闭，不保存或打印正文。大响应不单独视为业务错误，未读完时连接复用可能受影响。UNCERTAIN 是无法证明结果，不等于失败或成功；人工须按 task ID 和外部事实核对。

## 13. 现有人工能力与异常 DLQ

接口、鉴权和示例见 [README](../README.md)。详情与列表只允许已鉴权调用方查询自己的任务；列表默认 20、有效 page_size 最大 100，详情带尝试记录，不返回原 Header/Body。管理员有机构开关和 resolve，没有重放接口。

人工流程：检查失败/UNCERTAIN 或原生 DLQ → 按 task_id 查任务和尝试 → 向机构核实 → 记录事实、结论、操作人。对 FAILED、FAST_FAILED、EXPIRED、UNCERTAIN，可 resolve 保存结论；重复处置更新字段，没有不可变历史流水。人工结论与实际 HTTP 结果分别保留，不将任务改为 DELIVERED，也不新增 HTTP 预算。

DLQ 是消费执行异常耗尽，任务可能仍为 PENDING/PROCESSING；普通三次 HTTP 失败已保存终态并完成消费。**PENDING/PROCESSING 的 DLQ 任务无法使用 resolve 结案（409）**，只能在受控外部记录中保存核对结果，这是现有人工闭环缺口。没有自动 DLQ 工单、强制终态或自动重放。

值班负责人应在告警后及约定巡检时查看 DLQ，并结合失败/UNCERTAIN 查询处理；这是建议运行规程，仓库未实现自动告警和巡检。命令见 README，先核对镜像 mqadmin help。Broker 当前 fileReservedTime=48，死信不能作为无限期工单；必要 ID、时间和结论应及时保存到受控记录，不导出不必要正文/凭证。

## 14. 轻量日志与异步关联

入口校验 X-Trace-ID（最多 64 位字母、数字、点、下划线、连字符），无效则生成 UUID。首次 trace 写入 notification/Outbox，再进入 MQ 小事件，重投保留。成功路径从任务读取 trace，读任务失败时使用事件 trace/task。外发 Header 带 X-Trace-ID、X-Notification-Task-ID 和稳定 Idempotency-Key。

当前 trace 表示入口关联标识，不为每次重消费另建 trace，也不保证供应商内部链路可见。task_id 是任务主标识；http_attempts 区分已用业务机会，mq_reconsume 区分原生异常重消费。

主要日志包括受理、消费结束或未完成、Outbox 异常和管理员操作。结果字段为 `trace_id/task_id/destination_id/http_attempts/mq_reconsume/result/code/cost_ms`，其中 http_attempts 表示已用的业务尝试次数。审计补充 actor/enabled，发布异常补充 event_id/退避时间；不输出完整 Header、Body、Authorization、Cookie、带 Token 的 URL 或外部响应。

共享 slog logger 只接收每次调用的局部字段，不将当前 task/trace 写入共享可变 logger。详细尝试放 DB，不逐层重复 INFO；Outbox 扫描错误每 30 秒最多记一次。没有独立日志队列/服务或全量 Trace 平台，Compose 输出有轮转限制。

部分查询/Outbox 异常日志仍缺 trace，启动、迁移、SDK 路径尚未全面验证脱敏；有限 canary 扫描不证明所有故障路径安全。位置与建议见[已知限制](implementation-review.md)。

## 15. 安全、并发与资源控制

目标由 migration seed 配置，入口按 ALLOWED_TARGET 校验协议、主机、端口；调用方不能提供任意 URL。禁自动重定向和环境代理，保留 TLS 校验。当前防护依赖受信任的目标配置：执行时重读 destination，却没有重新校验 allowlist，也没有检查 DNS 解析后的地址范围。因此，受理后目标被修改和 DNS 地址变化仍是 SSRF 防护缺口。

Body 最大 256 KiB，业务 Header 最多 64 个、合计 16 KiB；JSON 信封另有传输读取限制，接近上限且大量转义的文本未必能受理。管理员输入有大小限制；API 不返回原请求。运行 DB 用户为 DML 权限，迁移用独立初始化身份。没有应用层加密、凭证轮换平台或数据自动清理。

资源上限有消费并发默认 4、单主机连接 16、空闲连接总数 64/每主机 8、DB 连接池 24/空闲 8，以及 Compose 内存/日志限制；当前没有显式 CPU 配额。HTTP 拨号 3s、TLS 握手 5s，每次总超时/处理预算有界。pull/batch=1 不等于 SDK 只缓存一条消息，尚未专门压测总缓存。

并发/连接数限制不等于 QPS 限流：响应很快时仍可高 QPS。没有入口/机构令牌桶或机构独立在途限制；机构共享 Worker，慢调用和退避都会占用容量。API 有 ReadHeaderTimeout，但没有完整请求体读取时长治理，公开暴露前需单独加固。

healthz 表示进程存活，readyz 在 2s context 内检查 MySQL、MQ 客户端状态及 NameServer/Broker TCP，不是探针每次真实发送消息。已启动进程在 Broker 故障但 MySQL 可用时仍可受理并写 Outbox；未完成 MQ 初始化或进程已退出则无法受理。

## 16. 流量增长后的渐进演进（未实现）

先区分保护对象：入口按上游业务方限制受理量和待处理规模，防一个调用方耗尽数据库；出口按机构或共享账号真实额度限制发送，防触发对方 429。两者不能互相替代。

令牌桶控制平均 QPS 和有限突发，同时用信号量/执行池限制在途数。首次和两次重试共用出口配额；尚未获准发送不计业务次数，但等待计入 TTL/处理预算。多个 URL 共用供应商账号时应共享额度，不能每接口得到完整额度。超过预算仍按终态规则处理，不借原生重投制造限流队列。

普通机构先共享资源；出现干扰后按机构、业务重要性或共享账号分组，为关键、高流量、慢或频繁故障机构划分 Topic/队列和 Worker 配额。队列隔离还需执行池、连接、在途数和 DB 访问预算，改队列名不消除资源竞争。RocketMQ 路由/消费组方案需独立设计，防误用不同组导致全量重复消费。

人工快速失败可在指标成熟后演进为错误率阈值、开启时长和受控半开探测，先定义样本、429 与真实故障的区别和恢复判据。自动化不能默默把 FAST_FAILED 改成等待恢复或复活旧任务。

API、Publisher、Consumer 出现不同瓶颈后，再分别拆部署扩容。增加实例前需解决跨实例执行权和机构合计额度，可先固定归属或静态分配，确需动态协调再评估共享设施。例如某机构总配额为 100 QPS，多个实例必须共享这 100 QPS，不能各自使用完整额度。

| 应观察的指标（后续建设） | 支持的判断 |
|---|---|
| Outbox 待发数/最老年龄、发布耗时/错误 | 发布端、DB 扫描或 MQ 交接是否瓶颈 |
| MQ 积压/最老任务年龄、TTL 剩余 | 是否增消费能力，还是任务已失去时效 |
| 按机构 QPS、在途数、p95/p99、429/超时/失败率 | 下游配额满时扩 Worker 无助，应削峰、协商时效或申请配额 |
| CPU、内存、goroutine、连接池等待、DB 锁/执行计划 | 下游有余量且本系统资源饱和时扩对应资源 |
| 原生重试/DLQ、UNCERTAIN 与人工积压 | 优先修执行故障/处置能力，不将异常量当业务容量需求 |

数据增长先按执行计划优化索引、扫描批量与归档。清理 Body 与保留幂等依据分开，不能删正文时取消去重承诺。日志平台、CDC、Redis、分片和 HA 只在相应约束被证明后评估。详见[题目答复第四、五部分](assignment-answers.md)。

## 17. 验收场景与已知限制

验收脚本 [verify-docker.ps1](../scripts/verify-docker.ps1) 在 Dockerfile 锁定的 Go 镜像中执行格式检查、vet、普通测试、race 和集成测试。集成环境包括 MySQL、NameServer、Broker、应用和可计数 mock target，通过任务状态、尝试记录和实际请求次数验证业务行为。

| 场景 | 验证重点 |
|---|---|
| 并发幂等、异内容、大小写不同键 | task/Outbox 行数、409、原 task 复用 |
| 503 后成功、连续三次 503、重复终态消息 | 尝试、mock 次数、终态；观察窗口内不再发 |
| 快速失败开启/关闭后的新旧任务 | 开启零调用，新任务恢复，旧终态保持 |
| 外部成功但结果保存失败 | 只重试保存；耗尽后通过 RocketMQ 重投，HTTP 不重复 |
| 等待中断 | 到期时间跨处理保留，未到期不开始下次 |
| DB 异常/原生 DLQ | Broker 重消费和死信；普通 FAILED 完成消费 |
| Broker 中断/恢复、发布标记失败 | 持久 Outbox 补发，重复事件不增加 HTTP |
| SIGTERM、原始文本、日志/canary | 可取消、不提前确认、恢复 UNCERTAIN、task/trace 配对 |

各场景的运行记录、命令、退出码和覆盖范围见[验收文档](verification.md)。

当前部署为单应用、单 MySQL、单 Broker，SYNC_FLUSH 和命名卷不能替代高可用与备份。现有验证覆盖功能与指定故障场景，尚未覆盖生产容量、完整安全路径、多实例协作和 DDL 中断恢复。物理 HTTP 次数硬上限、快速失败严格切换、DLQ 中间态处置、发送时目标复核及异常日志覆盖仍需完善，具体影响见[已知限制](implementation-review.md)。

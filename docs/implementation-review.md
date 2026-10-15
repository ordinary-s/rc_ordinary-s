# 已知限制与改进方向

这个项目实现了单实例通知服务的主要链路：事务受理、Outbox 发布、有限次数的 HTTP 投递、异常恢复，以及人工核对。当前的取舍是先保证任务状态和重试语义清楚，再按业务规模补充权限、资源隔离和故障处理能力。

下面说明哪些边界在接入真实机构或扩大部署规模前需要重点处理。测试覆盖与源码入口放在文末，完整测试结果见[测试与验收报告](verification.md)。

## 1. 投递与异常处理

### R01：三次业务尝试不等于任何故障下最多三个物理 HTTP 请求

应用最多调用三次 `client.Do`，每次调用前先持久化尝试次数。集成测试中，目标连续返回 503 时实际收到三次请求，终态任务再次消费也没有产生第四次调用。

这里还有一层底层行为：请求使用可重放的 `bytes.Reader`，并携带 `Idempotency-Key`，Go 的 `net/http.Transport` 在满足条件的连接故障下可能透明重放。应用记录的是业务尝试次数，没有单独统计 Transport 内部的每次发送。因此，下游如果要求严格的物理请求次数上限，还需要核查所用 Go 版本的重放条件，并补充复用连接断开、写后断连等测试。外部机构也需要明确是否支持幂等键，以控制重复业务处理。

源码入口：[Engine.Process 和 makeRequest](../internal/delivery/engine.go)。

### R02：快速失败生效存在读配置到发送之间的竞态窗口

消费者每轮读取机构配置后判断快速失败开关。读到开启时，任务保存为 `FAST_FAILED`，不再发起 HTTP；关闭开关后，旧终态任务仍不会恢复。已经发出的请求会继续执行。

管理员修改配置和消费者发送请求之间没有共同的同步机制。如果消费者先读到关闭，随后管理员开启开关，消费者仍可能按之前的读取结果发出请求。因此，开关适合停止后续读取到新配置的任务，无法保证 PATCH 返回后所有消费者立即停止发送。需要严格切换语义时，应增加机构级调度协调和并发切换测试。

源码入口：[Engine.Process](../internal/delivery/engine.go)、[Store.UpdateFastFail](../internal/store/store.go)。

### R03：DLQ 消息不一定能通过现有 resolve 接口闭环

RocketMQ 将原生重试耗尽的消息放入 DLQ，但这个动作不会同步更新数据库中的任务状态。现有 `resolve` 接口只接受 `FAILED`、`FAST_FAILED`、`EXPIRED`、`UNCERTAIN`，用于记录人工核对结论；它不会改写投递状态或重新发送消息。

如果持续读库失败，任务可能停在 `PENDING`；如果结果保存和恢复持续失败，可能停在 `PROCESSING`。这两种状态调用 `resolve` 会返回 409，没有对应任务的毒消息则返回 404。当前 DLQ 测试使用缺失任务的消息，验证的是 Broker 的死信行为。

处理这些异常时，需要结合 DLQ、任务、尝试记录、Outbox 和机构侧结果核对，并在外部工单中记录无法写入任务的结论。后续可增加带权限和审计的异常处置入口，同时保持终态任务不自动复活、DLQ 不自动重放的语义。

源码入口：[Store.Resolve](../internal/store/store.go)、[DLQ 集成测试](../tests/integration/e2e_test.go)。

## 2. 安全与运维

### TLS 与 HMAC 的安全边界

服务端目前最低接受 TLS 1.3；`/api/*` 在 Bearer 身份之外还校验 RFC 9421 HMAC 签名、内容摘要、时间窗和 MySQL nonce。真实 Docker E2E 应验证 TLS1.3 协商、TLS1.2 拒绝、无签名/篡改/过期/重放拒绝。

Compose 自签名 CA 和 key 仅用于本地演示。部署需提供可信证书、分别保存 client/admin HMAC 密钥并制定轮换流程；当前没有自动轮换平台或每客户端独立凭证库。HMAC 只校验来源与完整性，不保密；Body 仍以原始文本写入 MySQL 和备份。可由可信网关终止 TLS，但当前签名校验根据应用收到的请求 URI 计算，没有可信代理头支持，必须限制应用网络入口，且不能直接信任任意 `Forwarded` 头。

源码入口：[TLS server](../cmd/server/main.go)、[签名策略](../internal/signature/signature.go)、[nonce 表](../migrations/003_api_signature_nonce.sql)、[Compose 演示证书](../cmd/devcert/main.go)。验收结果及当前运行 ID见[测试报告](verification.md)。

### R04：日志关联已实现，但不是每条日志字段齐全、所有错误均已白名单化

任务的 trace 会写入数据库并随异步事件传递，消费日志通过局部参数传入关联字段，没有共享可变的请求日志状态。归档测试中的并发 task/trace 配对和敏感信息扫描均通过。

不同环节的日志字段还不完全一致：业务尝试字段使用 `http_attempts`，部分接收、管理员操作和 Outbox 日志缺少投递字段；读库失败时也可能拿不到机构和尝试次数。消费异常记录 `error_type`，启动与迁移路径仍输出原始 error，RocketMQ SDK 还有独立的日志行为。这些路径需要补充无效 DSN、连接失败、迁移失败等脱敏测试。

改进重点是按各环节定义必要字段，并限制自由错误文本，继续避免记录请求正文、响应正文、凭证和带 Token 的 URL。现有测试覆盖正常验收运行中的日志样本。

源码入口：[Engine.executionError 和 logResult](../internal/delivery/engine.go)、[服务启动](../cmd/server/main.go)、[迁移程序](../cmd/migrate/main.go)、[Outbox 发布器](../internal/outbox/publisher.go)。

### R05：目标 allowlist 在受理时验证，执行时未重新验证

调用方提交 `destination_id`，目标 URL 由机构配置提供。受理时会校验目标的 scheme、host 和有效端口是否符合 `ALLOWED_TARGET`。执行时会重新读取机构 URL，但只检查基本 HTTP(S) 格式、Host 和无 userinfo，没有再次检查允许的 origin。

因此，受理后修改机构配置可能改变任务的实际发送目标；迁移程序中的种子更新也能修改已有机构的 URL。当前适用于固定、受控的机构目标，尚未提供 DNS 解析后 IP 范围检查等通用 Webhook 防护。

后续可以在发送前重新校验允许的 origin，或为任务保存经过审核的目标快照。两种方案都需要明确“机构配置更新是否影响已受理任务”，并限制修改 URL 的权限。

源码入口：[Service.Accept 和 validTarget](../internal/notification/accept.go)、[Engine.makeRequest](../internal/delivery/engine.go)、[机构种子配置](../cmd/migrate/main.go)。

### R06：migration 002 的 DDL 成功但版本记录失败窗口无法直接重跑

迁移程序先执行 DDL，再写入 `schema_migrations`。MySQL DDL 会隐式提交，而 migration 002 使用无条件的 `ALTER TABLE ... ADD COLUMN next_attempt_at`。如果加列成功后进程中断，或版本记录没有写入，下次运行会因重复加列而失败。

全新数据库初始化已经通过验收，但这个中断窗口尚无恢复测试。遇到此情况，需要先核对实际表结构和版本表，再制定恢复步骤。后续可以增加迁移前的结构检查与中断恢复测试，避免依赖反复重跑。

源码入口：[migration 002](../migrations/002_notification_retry_due.sql)、[迁移执行与版本登记](../cmd/migrate/main.go)。

## 3. 扩展部署的前提

### R07：调用方与机构权限、限流和隔离属于明确的 MVP 范围限制

当前通过一个 client token 映射一个 `client_id`，通过一个 admin token 映射 `ADMIN_ACTOR`，查询按 `client_id` 隔离。目标限制支持一个 origin，尚无“调用方可以访问哪些机构”的 ACL 和管理员角色分级。

资源控制包括全局消费并发、每主机连接数、数据库连接池和容器资源限额。机构共用 worker，慢机构仍可能占用其他任务的执行资源；这些上限也无法替代入口 QPS、每机构 QPS 或每机构在途数限制。

扩展到多调用方、多机构时，应先补齐调用方到机构的权限，再根据流量和延迟指标增加令牌桶、在途数限制、分组 worker 与独立扩容。多实例还需要任务领取和共享配额设计。当前保持单实例，不依赖 Redis 锁或自动熔断。

源码入口：[API 鉴权与查询](../internal/api/server.go)、[通知受理](../internal/notification/accept.go)、[消费配置](../internal/mq/rocket.go)、[资源与超时配置](../internal/config/config.go)。

### R08：处理超时不包含等待同 task 内存锁的时间

同一个 task 在进程内通过互斥锁串行处理，引用计数归零后会回收锁。`DeliveryBudget` 在获得锁之后才开始计时，而等待锁本身不响应 context 取消。

因此，大量同 task 的重复消息可能占用消费者 goroutine，等待时间也不包含在默认 90 秒的处理预算中。现有 `DeliveryBudget + 5s <= MQConsumeTimeout` 校验覆盖的是获得锁后的预算。后续需要补充重复消息密集、取消、慢请求和锁回收的组合测试，并将锁等待纳入可取消的调度过程。扩展到多实例时，还需要数据库领取或租约机制。

源码入口：[Engine.Process 和 lockTask](../internal/delivery/engine.go)、[超时配置校验](../internal/config/config.go)。

## 附录：主要能力的实现与测试覆盖

下面的源码对应提交 [`9ed109d`](https://github.com/ordinary-s/rc_ordinary-s/commit/9ed109dce1a7e3373e052d06c7d343c1ba63c652)。测试采用 2026-10-14 已归档的通过记录，运行编号为 `20261014T063922Z-6c5cb01c`。运行环境、命令和结果详见[测试与验收报告](verification.md)。

| 能力 | 实现入口 | 已有验证及边界 |
|---|---|---|
| 事务受理 | [Store.Accept](../internal/store/store.go)、[API](../internal/api/server.go) | 同一事务创建 1 个任务和 1 条 Outbox，提交后返回受理。 |
| 入口幂等 | [唯一约束](../migrations/001_initial.sql)、[请求指纹](../internal/notification/accept.go) | 并发同键复用任务，同键异内容返回 409，键值大小写敏感；键先 TrimSpace，按 client_id 隔离。 |
| Outbox 发布恢复 | [Publisher.publishBatch](../internal/outbox/publisher.go)、[Client.Publish](../internal/mq/rocket.go) | 覆盖 Broker 中断和发送后本地标记失败；未注入真实网络确认包丢失。 |
| 单条消费与有界并发 | [MQ 客户端](../internal/mq/rocket.go) | 单条回调，样本最大并发为 4，8 个任务的 task/trace 配对正确。 |
| 有限业务重试 | [Engine.Process](../internal/delivery/engine.go) | 503 后第二次成功、三次 503 终止均通过；终态重复消息不增加 HTTP。底层重放边界见 R01。 |
| 业务终态与异常重投 | [Engine](../internal/delivery/engine.go)、[消费结果映射](../internal/mq/rocket.go) | 终态结果保存后结束消费；数据库执行异常触发 RocketMQ 原生重投，没有新增业务失败队列。 |
| 快速失败 | [Engine.Process](../internal/delivery/engine.go)、[Store.UpdateFastFail](../internal/store/store.go) | 命中开关时零 HTTP，关闭后旧终态不复活，新任务可执行；并发切换边界见 R02。 |
| 外部成功后保存结果 | [Engine.completeAttempt](../internal/delivery/engine.go)、[Store.CompleteAttempt](../internal/store/store.go) | 优先重试写库，写库重试耗尽后重投转为 UNCERTAIN；目标均只收到 1 次 HTTP。 |
| 重试进度恢复 | [Engine.Process](../internal/delivery/engine.go)、[重试到期字段](../migrations/002_notification_retry_due.sql) | 保存次数和 next_attempt_at；取消恢复场景直接调用 Engine，SIGTERM 场景覆盖真实进程中断。 |
| 过期与退避 | [Engine.Process 和 responseResult](../internal/delivery/engine.go) | 过期任务零 HTTP；重投沿用到期时间。极长 Retry-After 与取消时序的完整组合尚未覆盖。 |
| 原生 DLQ 与人工核对 | [MQ 客户端](../internal/mq/rocket.go)、[Store.Resolve](../internal/store/store.go) | 覆盖真实 Broker 死信与终态任务核对记录；DLQ 全部异常的处置入口见 R03。 |
| 请求格式与结果判断 | [输入校验](../internal/notification/accept.go)、[HTTP 执行](../internal/delivery/engine.go) | XML、表单内容保持；2xx 且响应处理成功才记为 DELIVERED，响应读取失败进入异常恢复。 |
| 接口与目标限制 | [API](../internal/api/server.go)、[目标校验](../internal/notification/accept.go) | 具备 token 鉴权、查询隔离、受理时 origin 校验、大小限制和禁止重定向；扩展边界见 R05、R07。 |
| 日志关联 | [投递日志](../internal/delivery/engine.go)、[Outbox 日志](../internal/outbox/publisher.go) | 并发配对与归档运行的敏感信息扫描通过；异常路径覆盖见 R04。 |
| 单任务互斥与处理预算 | [Engine.lockTask](../internal/delivery/engine.go)、[配置校验](../internal/config/config.go) | 具备进程内互斥、引用计数回收与处理预算；密集重复消息和锁等待边界见 R08。 |
| 代码检查与真实环境测试 | [容器内检查脚本](../scripts/verify-in-container.sh)、[集成测试](../tests/integration/e2e_test.go) | 格式、vet、普通测试、race 与真实 MySQL/RocketMQ 集成测试均有退出码记录；race 覆盖普通测试，未覆盖带 integration 标签的集成测试。 |

原始记录：[集成场景](evidence/20261014T063922Z-6c5cb01c/integration.json)、[检查阶段](evidence/20261014T063922Z-6c5cb01c/checks-summary.txt)、[Broker 中断与过期](evidence/20261014T063922Z-6c5cb01c/outage.json)、[SIGTERM 恢复](evidence/20261014T063922Z-6c5cb01c/sigterm.json)、[日志配对](evidence/20261014T063922Z-6c5cb01c/log-context.tsv)。

# 实现、设计与证据核对

## 1. 审查基线和证据范围

本页记录 2026-10-14 的源码审查。最终业务规则以本次用户要求及 [AGENTS.md](../AGENTS.md) 为准，详细设计见 [design.md](design.md)。源码基线为 [`9ed109dce1a7e3373e052d06c7d343c1ba63c652`](https://github.com/ordinary-s/rc_ordinary-s/commit/9ed109dce1a7e3373e052d06c7d343c1ba63c652)。以下行号均指该基线。

审查开始时，仓库位于 `58dc791`，并存在预先修改的代码、配置、脚本、migration 和文档。审查期间已有工作进入独立提交 `9ed109d`；本次文档修订以该提交重新核对，不把这些业务修改计为本次文档工作的产出。本页没有修改业务代码或数据库结构。

本页的“有验证”只指下列已归档报告所覆盖的场景，不把源码推理或未运行测试写成 PASS。用户指定的实现任务已完成运行 `20261014T063922Z-6c5cb01c`；本次重新核对该验收结论与原始证据，命令、退出码及范围见 [verification.md](verification.md)。

| 证据代号 | 已读取的实际证据 | 能证明的范围 |
|---|---|---|
| E1 | [既有集成场景](evidence/20261014T063922Z-6c5cb01c/integration.json) | 16 个场景的实际结果、任务状态和 mock 调用计数；对应测试代码见 [e2e_test.go](../tests/integration/e2e_test.go)。 |
| E2 | [既有检查阶段](evidence/20261014T063922Z-6c5cb01c/checks-summary.txt)、[运行摘要](evidence/20261014T063922Z-6c5cb01c/summary.txt) | 该次 gofmt/vet/test/race、集成检查、脚本及清理退出码；运行时间为 `2026-10-14T06:39:22Z` 至 `06:44:17Z`。 |
| E3 | [Broker 中断及过期](evidence/20261014T063922Z-6c5cb01c/outage.json)、[SIGTERM 恢复](evidence/20261014T063922Z-6c5cb01c/sigterm.json) | Broker 恢复后发布、过期零请求、在途进程中断后 UNCERTAIN 且没有再次调用 HTTP。 |
| E4 | [并发日志配对](evidence/20261014T063922Z-6c5cb01c/log-context.tsv)、E2 中的日志扫描结果 | 该次样本的 task/trace 配对、并发量和敏感内容扫描。不是全部启动、驱动、SDK 错误路径的形式化证明。 |

既有报告记录了运行 ID、镜像 ID 和测试结果，但摘要没有记录 Git SHA。它们随 `9ed109d` 归档，不能仅凭归档位置断言报告带有完整的源码哈希证明。新的验收记录应补充实际基线与测试时工作区状态。

## 2. 核对矩阵

状态含义：“已实现且有验证”是代码可见且有对应场景证据；“已实现待验证”是代码可见但指定边界没有直接测试；“部分实现”明确存在覆盖缺口；“未实现”表示本次完整检查相关代码后没有该能力；“未核对”表示尚未完成对应检查。范围限制和残余差异详见下一节。

| ID | 规则或问题 | 代码依据 | 测试或报告依据、适用边界 | 状态 |
|---|---|---|---|---|
| V01 | 通知任务和 Outbox 同事务提交，成功后才返回受理。 | [Store.Accept](../internal/store/store.go) 68–104 行；[API.createNotification](../internal/api/server.go) 108–127 行。 | E1 `acceptance_transaction_and_idempotency`：1 个任务、1 条 Outbox。 | 已实现且有验证 |
| V02 | 并发同键建一个任务，同键异内容冲突，比较规则明确。 | [唯一约束](../migrations/001_initial.sql) 13–15、32 行；[fingerprint](../internal/notification/accept.go) 85–97 行；Store.Accept 78–89 行。幂等键先 TrimSpace，按 client_id 隔离。 | E1 `case_sensitive_and_parallel_idempotency`；大小写敏感，同内容复用、异内容 409。 | 已实现且有验证 |
| V03 | 发布器只补发未完成发布，发送未确认不标成功。 | [PendingOutbox](../internal/store/store.go) 362–399 行；[Publisher.publishBatch](../internal/outbox/publisher.go) 51–82 行；[Client.Publish](../internal/mq/rocket.go) 129–146 行。 | E1 `outbox_send_mark_failure_duplicate_safe`、E3；验证了本地标记故障，未注入真实网络确认包丢失。 | 已实现且有验证 |
| V04 | Broker/SDK 固定版本、消费模式与超时配置接入真实链路。 | [go.mod](../go.mod)、[compose.yaml](../compose.yaml)；[mq.New](../internal/mq/rocket.go) 51–59 行；[config.Load](../internal/config/config.go) 107–112 行。 | E1 真实 Broker 发布、异常重投和 DLQ；配置超时接入可见，不代表每种超时边界均已测试。 | 已实现且有验证 |
| V05 | 单条消费、有界并发，没有批次部分结果第二路径。 | mq.New 配置 batch=1、pull=1、goroutine 数；Client.Start 94–115 行拒绝非单条回调。 | E1 `concurrent_tasks_keep_trace_and_task_context_paired`；E4 样本在途上限 4。 | 已实现且有验证 |
| V06 | 总共最多三次 HTTP、仅两段退避。 | [Engine.Process](../internal/delivery/engine.go) 137–235 行；config.Load 96–102 行。应用最多三次 `client.Do`。 | E1 `three_503_terminal_duplicate_delivery` 恰为 3 次；Transport 透明重试另见 R01，不能扩张为任意网络故障下的物理请求硬上限。 | 部分实现 |
| V07 | 三次终止失败保存后结束消费，不新建业务重试 Outbox。 | Engine.Process 204–210 行；Client.Start 112–115 行；全仓业务 Outbox 插入点只有 Store.Accept。 | E1 `three_503_terminal_duplicate_delivery`；终态重复消息未增加 HTTP。 | 已实现且有验证 |
| V08 | 已完成业务失败与执行异常分开，只有未完成异常触发原生重投。 | Engine.Process/completeAttempt 返回 nil 或 error；Client.Start 映射消费结果。 | E1 `database_exception_native_redelivery`、结果写库失败的两个场景。 | 已实现且有验证 |
| V09 | 命中快速失败零新 HTTP、保存终态；关闭不复活旧任务。 | Engine.Process 85–123 行；[Store.UpdateFastFail](../internal/store/store.go) 316–329 行仅更新机构配置。 | E1 `fast_fail_terminal_manual_recovery`；开关切换与发请求之间仍有竞态窗口，见 R02。 | 部分实现 |
| V10 | 同 task_id 并发互斥，内存锁可回收。 | Engine.lockTask 327–343 行：引用计数管理 map；Process 71–76 行。仅进程内。 | 重复消息已有 E1；同 task 高并发、锁回收和等待锁超时没有独立场景证据，见 R08。 | 已实现待验证 |
| V11 | 已终态消息重投不增加 HTTP，业务次数不归零。 | [domain.IsTerminal](../internal/domain/model.go) 46–53 行；Engine.Process 85–87、137 行；[StartAttempt](../internal/store/store.go) 190–215 行条件更新次数。 | E1 受理幂等、三次失败重复消费、快速失败关闭后旧任务场景。 | 已实现且有验证 |
| V12 | 外部成功后优先重试保存结果，不能直接重发 HTTP。 | Engine.completeAttempt 239–259 行；Store.CompleteAttempt 219–271 行幂等核对结果。 | E1 `external_success_result_write_retried_without_repeating_http`、`external_success_result_write_exhaustion_no_http_repeat`；耗尽后 UNCERTAIN、mock 仍为 1 次。 | 已实现且有验证 |
| V13 | PROCESSING 恢复有明确路径，只沿用剩余业务预算。 | Engine.Process 89–103 行将残留 STARTED 变 UNKNOWN/UNCERTAIN；125–144 行沿用 `http_attempts`/`next_attempt_at`。 | E1 有取消恢复通过记录，E3 覆盖 SIGTERM；这些成绩仅对应指定归档运行，不保证任意时序下恢复成功。 | 已实现且有归档验证 |
| V14 | 等待可取消，处理预算与 MQ 超时匹配，退出不提前确认。 | Engine.Process 75、125–134 行；completeAttempt 249–257 行；Client.Start 106–115 行关联进程 context；config.Load 107–112 行。 | E3 SIGTERM、E1 取消恢复；等待同 task 锁不在 DeliveryBudget 内，见 R08。 | 部分实现 |
| V15 | 有效期、Retry-After 不因重投或开关重置。 | Engine.Process 105–110、212–235 行；responseResult 303–324 行；[migration 002](../migrations/002_notification_retry_due.sql) 保存重试到期时间。 | E1 取消恢复与 E3 过期场景；极长 Retry-After、时间边界和取消时序的完整组合尚未验证。 | 已实现且有归档验证 |
| V16 | 原生异常重投/DLQ 可观察，并有人工核对入口。 | Client.Start；[管理员 resolve](../internal/api/server.go) 216–244 行；Store.Resolve 332–352 行。 | E1 `rocketmq_native_retries_reach_dlq` 使用缺失 task 的毒消息。DLQ 无自动数据库状态同步；PENDING/PROCESSING 或缺失任务不能 resolve，见 R03。 | 部分实现 |
| V17 | 不自动搬运 DLQ，不自行发布另一条执行异常队列。 | 已检查 cmd/internal 和 MQ 初始化脚本：运行应用只订阅业务 Topic，异常走 SDK 返回值。 | E1 原生重试/DLQ；测试中的 DLQ observer 仅用于验收，不是运行应用能力。 | 已实现且有验证 |
| V18 | 支持明确请求格式，按 HTTP 契约判断结果。 | [validateInput](../internal/notification/accept.go) 161–191 行、Engine.makeRequest/responseResult；body 是 JSON 字段解码后的 UTF-8 内容。 | E1 `raw_body_0`、`raw_body_1` 验证 XML/表单内容保持；2xx 且响应处理成功为 DELIVERED，响应读取失败仍走未完成/不确定恢复。 | 已实现且有验证 |
| V19 | 调用方隔离、管理员鉴权、目标/跳转限制、大小限制。 | API.authenticateClient/getNotification/listNotifications；accept.validTarget；Engine.New 禁止重定向；API/validateInput 限制大小。 | 代码具有单 client 隔离和配置 origin 限制；没有按调用方到机构 ACL、执行时重新验证 allowlist，见 R05/R07。 | 部分实现 |
| V20 | 日志轻量、关联不串，不记录请求响应正文或凭证。 | API 99–127 行持久化 trace；mq.Event 携带关联；Engine.executionError/logResult 显式传局部字段；Publisher.logScanError 限频。 | E4 的样本已通过；不是每条日志均有全部字段，启动/迁移原始 error 路径未统一白名单化，见 R04。 | 部分实现 |
| V21 | 并发和连接资源有上限，未实现的配额不宣称已有。 | Engine.New 51–65 行连接池；Store.Open 24–35 行数据库池；mq.New/config.Load 限制 worker；Compose 资源限额。 | E1 并发样本；没有入口/机构 QPS、机构在途上限或多实例全局配额，见 R07。 | 部分实现 |
| V22 | 验收运行真实 MySQL、RocketMQ、应用、mock，而非只有启动检查。 | [verify-docker.ps1](../scripts/verify-docker.ps1)、[verify-in-container.sh](../scripts/verify-in-container.sh)、[集成测试](../tests/integration/e2e_test.go)。 | E1–E3 是用户指定已完成验收的真实链路证据；具体运行范围见 verification.md。 | 已实现且有验证 |
| V23 | gofmt/vet/test/race 有真实命令和退出码，并区分证据版本。 | verify-in-container.sh 25–39 行执行格式、静态检查、普通/race 测试、集成编译/E2E。 | E2 记录各阶段退出码 0；既有摘要没有 Git SHA，本次版本归属另见 verification.md。 | 已实现且有验证 |
| V24 | README/设计/题目答复/AI 使用说明一致，历史、目标、已实现分开。 | 文档导航与 [decisions.md](decisions.md)、[assignment-answers.md](assignment-answers.md)、[AI_USAGE.md](../AI_USAGE.md)。 | Markdown 链接、格式、旧规则和敏感信息检查已完成，来源与表述经过人工复核；记录见 verification.md。 | 已核对 |

## 3. 残余差异与建议

以下是本次审查发现的实现边界。它们不通过修改文档而变成“已修复”；本次不改业务代码。高风险表示若作为硬承诺可能直接违背业务契约，中风险表示故障或竞态窗口需要人工介入或额外保护，低风险表示当前范围内的审计/可观测性缺口。没有复现的故障不写成已发生事件。

### R01：三次业务尝试不等于任何故障下最多三个物理 HTTP 请求

- **目标**：通知包含首次总共最多三次 HTTP 调用。
- **实际**：Engine.Process 在 137–210 行限制最多三次 `client.Do`，每次调用前持久化次数；503 集成场景确实观察到三次。makeRequest 在 281 行使用 `bytes.Reader` 构造可重放 body，并在 297 行设置 `Idempotency-Key`；默认 `net/http.Transport` 对满足条件的可重放请求可能内部重试。应用没有逐次计数其内部网络发送。
- **差异与风险**：**中；若合同要求下游实际接收次数硬上限，则高。** 现证据支持三次业务尝试，不能把它写成任何连接故障下物理 HTTP 发送或供应商业务执行次数必然不超过三次。也不能将此边界作为扩大业务重试次数的许可。
- **依据**：[engine.go](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/internal/delivery/engine.go#L137)，尤其 166、281、297 行；既有 E1 只覆盖明确返回 503 的计数。
- **建议**：单独审查所锁定 Go 版本的 Transport 重放条件，加入复用连接断开、写后断连等故障注入，再决定是否需要阻止透明重放或在更低层计数。保留任务幂等键，并由外部机构明确其幂等契约。

### R02：快速失败生效存在读配置到发送之间的竞态窗口

- **目标**：开启机构快速失败后不发起新 HTTP；命中任务保存 FAST_FAILED 后结束消费，关闭不复活旧终态。
- **实际**：Engine.Process 每轮先查询 destination，再判断 `FastFailEnabled`，之后预留尝试并调用 HTTP。管理员更新配置和这组动作没有共同的机构同步屏障；已经开始的请求也不会被开关中止。终态检查在最前，关闭开关不会复活旧任务。
- **差异与风险**：**中。** “处理读取到开关开启时零新 HTTP”已验证；“PATCH 成功返回的一刻，所有消费者此后绝无新请求”未被保证。读到关闭、随后管理员开启、最后执行发送的交错仍可能发生。
- **依据**：[engine.go](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/internal/delivery/engine.go#L113) 113–166 行；Store.UpdateFastFail 316–329 行。
- **建议**：当前运维说明明确该窗口和在途请求边界。如果需要严格切换语义，另行设计机构级调度协调并增加并发切换测试；不能仅靠再读一次配置宣称竞态完全消失。

### R03：DLQ 消息不一定能通过现有 resolve 接口闭环

- **目标**：原生异常重试耗尽后可查询、核对并留下人工处置结论，不自动重发业务失败。
- **实际**：Broker 将消息放入原生 DLQ，不同步修改 notification。resolve 只允许 FAILED、FAST_FAILED、EXPIRED、UNCERTAIN，且只写结论，不修改业务状态、不发送消息。若持续读库失败耗尽重试，任务可能仍为 PENDING；若结果保存与恢复持续失败，任务可能仍为 PROCESSING；毒消息还可能根本没有任务记录。
- **差异与风险**：**中。** 这些 DLQ 项不能直接通过现有 resolve 留结论：中间态返回 409，缺失任务返回 404。当前“DLQ 查询 + resolve”并非覆盖所有异常的完整人工闭环。已有 DLQ 测试恰使用 `missing-task-for-dlq`，只证明 Broker 死信行为。
- **依据**：[Store.Resolve](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/internal/store/store.go#L332)、API.resolveNotification 229–240 行、[testNativeDLQ](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/tests/integration/e2e_test.go#L874)。
- **建议**：先由负责人检索 DLQ、核对 task/attempt/Outbox 和机构侧结果，在受控外部记录中保存无法写入任务的结论；不得把关闭快速失败、修改次数、直接搬运 DLQ 当作已有恢复接口。未来可以增加带授权与审计的异常处置入口，但不能无条件复活终态或自动重放。

### R04：日志关联已实现，但不是每条日志字段齐全、所有错误均已白名单化

- **目标**：关键链路使用轻量 trace/task/destination/attempt/result/code/cost 信息，不记录正文、Header、Token、凭证或含 Token 的 URL，不共享可变的请求日志状态。
- **实际**：业务消费日志通过参数显式传入关联字段，异步事件和数据库保存 trace；使用 `http_attempts` 而非统一的 `attempt` 字段。接收、管理员操作和 Outbox 部分日志没有全部字段；数据库读取失败时 destination/次数可能为 `unknown`。Engine.executionError 输出 `error_type`，但启动与迁移路径仍输出原始 error。SDK 自身也有独立日志行为。
- **差异与风险**：**低，启动故障脱敏覆盖待加固。** 既有样本扫描和并发配对通过，不代表所有异常文本均经过统一字段白名单。源码审查没有证明已泄露真实凭证，不据此声称发生泄密。
- **依据**：[Engine.executionError/logResult](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/internal/delivery/engine.go#L346)；[cmd/server/main.go](../cmd/server/main.go) 28、34、43、48 行；[cmd/migrate/main.go](../cmd/migrate/main.go) 26、30、67、71 行；Publisher.publishBatch 的异常日志。
- **建议**：按各环节实际可用字段定义最小日志契约，不为了字段齐全记录正文或自由文本。后续覆盖无效 DSN、启动连接失败、迁移失败和 SDK 异常的脱敏测试；无需引入独立日志平台。

### R05：目标 allowlist 在受理时验证，执行时未重新验证

- **目标**：外部目标受管理员配置和允许 origin 约束，调用方不能任意指定请求地址。
- **实际**：Service.Accept 校验目标的 scheme/host/有效端口；请求仅携带 destination_id。执行时重新读取 destination URL，makeRequest 只校验基本 HTTP(S) URL、Host 和无 userinfo，不再次校验 `ALLOWED_TARGET`。迁移种子逻辑能够更新已存在 destination 的 URL。
- **差异与风险**：**中，触发条件是受理后目标配置被修改。** 已受理任务可能使用后来改变的目标；当前机制不等于发送时完整的 SSRF 防护，也没有 DNS 解析后 IP 范围检查。固定受控目标的 MVP 范围应明确，不能宣称通用多租户 Webhook 安全平台。
- **依据**：[accept.go](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/internal/notification/accept.go#L50) 55–60、121–130 行；Engine.Process 113 行、makeRequest 275–279 行；cmd/migrate/main.go 75–79 行。
- **建议**：限制谁能修改机构 URL；后续可在发送前重复校验受控 origin，或为任务保存经过审核的目标快照，并明确配置更新对已受理任务的语义。额外网络访问策略属于独立加固工作。

### R06：migration 002 的 DDL 成功但版本记录失败窗口无法直接重跑

- **目标**：使用版本 migration，认识 MySQL DDL 隐式提交，不假设 DDL 能随事务回滚。
- **实际**：迁移器先执行 DDL，再写 schema_migrations；002 是无条件 `ALTER TABLE ... ADD COLUMN next_attempt_at`。若加列成功后进程中断、或版本记录失败，下次因版本未登记再次加列会报重复列。
- **差异与风险**：**中。** 已有全新数据库验收通过，不证明此中断窗口可自动恢复。不能笼统描述所有 DDL 都可以安全重复执行；001 的 `IF NOT EXISTS` 不能代表 002 同样幂等。
- **依据**：[migration 002](../migrations/002_notification_retry_due.sql)；[迁移执行与登记](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/cmd/migrate/main.go#L61) 61–71 行。
- **建议**：遇到此失败先核对真实表结构和版本表，由有权限的维护者制定恢复步骤，不盲目回滚 DDL或反复重跑。后续增加结构前置核对和迁移中断恢复测试。本次不修改迁移文件。

### R07：调用方与机构权限、限流和隔离属于明确的 MVP 范围限制

- **目标**：当前保持单实例最小实现；未来按调用方、机构和重要性限制资源并隔离故障。
- **实际**：一个配置的 client token 映射一个 client_id，一个 admin token 映射 `ADMIN_ACTOR`；查询按 client_id 隔离。没有“哪个调用方可调用哪个 destination”的 ACL，没有管理员角色分级；allowlist 为一个 origin。全局消费并发、每主机连接数和数据库连接池有上限，但没有入口令牌桶、机构 QPS/在途限额、独立机构 worker 或多实例全局限流。
- **差异与风险**：**范围限制；若对外作为多租户生产服务使用则需重新评估。** 全局并发上限不是每机构配额，也不等于 QPS 上限。机构共用 worker 时，慢机构能够占用消费资源。
- **依据**：[API 鉴权](../internal/api/server.go) 247–260 行；[notification.Service](../internal/notification/accept.go) 29–47 行；Engine.New、Store.Open、mq.New；[config.Load](../internal/config/config.go)。
- **建议**：当前文档只声明已有能力。扩展到多调用方、多外部机构时，再按实际指标引入调用方到机构 ACL、令牌桶、在途数限制、分组 worker 和独立扩容；自动熔断、Redis 或全局协调不能作为当前已实现功能。

### R08：处理超时不包含等待同 task 内存锁的时间

- **目标**：单任务互斥，整体处理有界，取消和退出不误确认。
- **实际**：Process 先阻塞获取 taskLock，再创建 DeliveryBudget context；锁等待本身不响应 context。锁引用数归零后会删除，但互斥只覆盖本进程，不能用来宣称多实例互斥。
- **差异与风险**：**中，重复消息密集时。** 同 task 的等待者可能占用消费者 goroutine；其等待时间不计入声明的 90 秒处理预算，因此不能仅凭 `DeliveryBudget + 5s <= MQConsumeTimeout` 宣称所有回调全程均在预算内。现有测试未专门覆盖此压力场景。
- **依据**：[Engine.Process](https://github.com/ordinary-s/rc_ordinary-s/blob/9ed109dce1a7e3373e052d06c7d343c1ba63c652/internal/delivery/engine.go#L70) 71–76 行、lockTask 327–343 行；config.Load 107–112 行。
- **建议**：后续验证同任务并发重复消息、取消、锁回收和慢请求组合；若要完整回调预算，需将等待纳入可取消的调度边界。多实例部署另行设计数据库领取或租约，不把本地锁视作分布式锁。

## 4. 已确认不应混入的旧方案

当前源码没有 RabbitMQ、HTTP 失败重发队列、批次部分失败协调、Redis 锁或自动熔断。它们只能出现在有来源说明的历史取舍或未来演进中。

已受理不等于已送达；RocketMQ 至少一次交付不等于外部业务恰好一次。默认退避是 10 秒、20 秒加少量抖动，配置只强制两段非负等待，不强制任意自定义值仍满足指数关系。HTTP 429/5xx 可在剩余预算内重试；其他确定性失败、快速失败、过期或结果不明可能少于三次就结束。三次业务上限不因 MQ 重投、管理员关闭快速失败或重启而重新获得。

管理员 resolve 保存核对结论，不把状态改成 DELIVERED，不创建新任务，不恢复旧终态。没有自动 DLQ 工单和自动重放能力；人工处置的实际边界见 R03。

## 5. 本次提交前的复核要求

本次只提交审查相关文档和实际验收证据。不得为了通过验收删测试断言，不修改业务代码、消费语义或数据库结构。最终核对文档相对链接、敏感信息、状态和规则表述；[verification.md](verification.md) 引用用户指定任务已归档的完整 Docker 验收。

交付分别报告文件修改、文档提交、GitHub 推送、远端回读与验证情况。指定运行 PASS、源码已实现、测试已覆盖及残余风险分别说明，不合并成“所有边界均已验证”。

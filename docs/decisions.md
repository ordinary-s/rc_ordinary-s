# 设计决策与取舍

本表记录需求讨论后采用的最终方案，以及早期建议被替换或暂缓的内容。实现证据链接到当前源码/Compose；真实 Docker 场景证据和运行 SHA 见[验收记录](verification.md)。

| 编号 / 议题 | 最终决定 | 被替代或推迟的做法 | 理由与代价 | 实现或验证证据 |
|---|---|---|---|---|
| D01 Java/Spring 与 Go | 使用 Go 标准库、显式依赖与函数组合 | 不采用 AOP 注解式事务/幂等层 | 状态、取消和错误边界直观；代价是需要显式编排。 | [server](../cmd/server/main.go)、[internal](../internal/)；Docker 中 gofmt/vet/test/race |
| D02 PostgreSQL 与 MySQL | 使用 MySQL/InnoDB 保存通知、尝试、目标和核对结果 | 不为 PostgreSQL 专属能力引入额外切换 | 当前没有 PostgreSQL 专属需求；仍需自行验证 collation、UTC 和迁移。 | [migration](../migrations/001_initial.sql)、[Store](../internal/store/store.go)、真实 MySQL E2E |
| D03 事件路径 / Spring 事件 / RabbitMQ / RocketMQ | 新版由上游直接向 RocketMQ 发布完整事件，本服务只有消费者 | 移除本服务 HTTP 通知提交及内部 Outbox Publisher；RabbitMQ/Spring 事件路线不保留 | 责任边界移到上游业务 Outbox；代价是本服务不能替上游修复业务库到 MQ 的双写。 | [MQ consumer](../internal/mq/rocket.go)、[server callback](../cmd/server/main.go)、[Compose](../compose.yaml) |
| D04 发布可靠交接 | 上游用自己的事务 Outbox 或等价持久交接；本服务消费已发布事件并先落库 | 本服务定时扫描自己的 Outbox 并向 MQ 发布 | 避免重复建设发送路径；上游必须确保稳定 event_id 与业务提交一致。 | [event ingest](../internal/notification/accept.go)、[Store.Ingest](../internal/store/store.go)、直接 MQ E2E |
| D05 批量确认与单条回调 | PushConsumer 单条回调、单条拉取、有界并发 | 放弃整批确认后自行重发部分失败消息的协调逻辑 | 责任确认对应一条消息；代价是没有批处理吞吐优化。 | [MQ client](../internal/mq/rocket.go)、并发重复投递 E2E |
| D06 HTTP 次数口径 | 最多三次业务尝试，首次加两次重试 | 纠正首次加三次重试（总共四次） | 业务预算有唯一口径；Transport 底层对部分网络错误可能内部重试。 | [Delivery Engine](../internal/delivery/engine.go)、503 mock 计数 E2E |
| D07 业务终态与执行异常 | 已保存 FAILED/FAST_FAILED/EXPIRED/UNCERTAIN/DELIVERED 均 ACK；仅执行未完成交 MQ | 不因 HTTP 最终失败再发新业务消息；不把任意非空 error 等同需重发 HTTP | 避免无界业务重复；代价是业务失败需查询或人工判断。 | [callback](../internal/mq/rocket.go)、[Engine](../internal/delivery/engine.go)、3x503/DB retry E2E |
| D08 机构快速失败 | 管理员开启开关后，任务保存 FAST_FAILED 并完成；关闭只影响新任务 | 不把任务暂停排队等恢复，不自动复活旧终态 | 状态和责任清楚；代价是旧失败须人工处置且依赖人工恢复判断。 | [admin API](../internal/api/server.go)、Store、fast-fail E2E |
| D09 MQ 异常重试 | 使用 RocketMQ 原生有限重试和 DLQ | 移除自定义 Properties 次数驱动的二次手工发布 | 只保留一套执行异常恢复；时间/上限受 Broker 与 SDK 约束。 | [MQ callback](../internal/mq/rocket.go)、真实 Broker 重投/DLQ E2E |
| D10 幂等责任 | 唯一 (trusted client,event_id) + 指纹；消费检查任务终态；下游按稳定 task 标识去重（若下游支持） | 不用 trace_id/MQ MsgID 替代业务键，也不承诺外部恰好一次 | 三层分别解决不同重复来源；下游未实现幂等时，远端结果未知仍有重复风险。 | [migration 004](../migrations/004_source_event_id.sql)、[ingest](../internal/notification/accept.go)、冲突/重复 E2E |
| D11 日志 | 使用 slog 短字段、trace/task 关联和审计日志 | 不输出正文/凭证，不加无界日志队列、逐层完整堆栈或日志平台 | 降低敏感数据和日志成本；复杂排查需结合数据库。 | [server](../cmd/server/main.go)、[Engine](../internal/delivery/engine.go)、并发上下文/敏感 canary 扫描 |
| D12 人工闭环 | 提供失败查询、结论记录和 Broker DLQ 查询命令 | 不自动搬运 DLQ、不自动批量重放、不建审批系统 | 足以记录第一版核对事实；DLQ 不是数据库待办，坏事件仍需外部登记。 | [resolve API](../internal/api/server.go)、[README 运维入口](../README.md)、DLQ E2E |
| D13 未来治理 | 未来按机构/共享账号配额、分组、隔离后独立扩容 | 当前不加入 Redis、集群协调、全局限流和自动熔断 | 先按观测瓶颈演进；代价是当前共享 Worker 下机构间资源隔离有限。 | 当前仅全局并发配置 [config](../internal/config/config.go)；后续设计见题目答复 |
| D14 工程交付 | 固定 D 盘路径、固定版本、真实 Docker MySQL/RocketMQ 和完整验收 | 不用 Mock MQ、容器 running 或未执行测试宣称通过 | 需要等待真实 Broker 重试/死信，验收较慢；得到可复核证据。 | [verify scripts](../scripts/verify-docker.ps1)、[Compose](../compose.yaml)、[最新报告](verification.md) |
| D15 API 加密与完整性 | TLS 1.3 保护传输；Bearer + RFC 9421 HMAC 签名（按讨论加入） | 不发明额外请求体加密；不将 HMAC 当加密 | TLS 保密并认证服务端；HMAC 校验调用方持钥与完整性，不保密。代价是密钥/nonce 管理。 | [TLS startup](../cmd/server/main.go)、[signature](../internal/signature/signature.go)、nonce migration 与 TLS/HMAC E2E |

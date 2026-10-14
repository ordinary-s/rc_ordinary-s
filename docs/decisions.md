# 讨论决策表

| 议题 | 最终决定 | 被替代或推迟的做法 | 理由及代价 | 实现/验证证据 |
|---|---|---|---|---|
| D01 Java/Spring 与 Go | Go；使用标准库、显式依赖、函数组合 | Java/Spring AOP 注解体系 | 需求不需要复杂框架；代价是显式编排 | `cmd/server`, `internal/*`; Docker build、vet |
| D02 PostgreSQL 与 MySQL | MySQL 8.4.8 / InnoDB | PostgreSQL 早期候选 | 只依赖常规事务/约束/索引，无专属能力要求；不宣称 MySQL 天然更可靠 | `migrations/001_initial.sql`; UTC/EXPLAIN 集成 |
| D03 Spring 事件、RabbitMQ、RocketMQ | RocketMQ 5.5.0 + 官方 `rocketmq-client-go/v2` 2.1.2 | Spring 事件/RabbitMQ；不保留两套发送系统 | 与用户最终技术约束一致；需管理 Broker/NameServer/SDK 兼容 | `internal/mq/rocket.go`, Compose 真 Broker 测试 |
| D04 生产者可靠交接 | Go 循环扫描 MySQL Outbox；发送确认后标记，标记更新失败也按退避时间补发 | 先 DB 后 MQ 双写；扫描所有 HTTP 失败任务重发；标记失败后每轮立即重发 | 原子持久化发布责任；重复事件由消费者处理，退避避免数据库写入故障时快速发布循环 | `internal/outbox/publisher.go`; Broker outage 和真实 MySQL trigger 标记失败后重复发布 E2E，间隔实测 1110ms |
| D05 批量确认/单条回调 | 单条处理、SDK batch=1、有界并发 | 整批协调及失败消息手工重发 | 确认边界易解释；减少复杂部分成功状态 | `internal/mq/rocket.go`; RocketMQ 集成与 DLQ |
| D06 HTTP 次数口径 | 最多三次总尝试（首次 + 两次重试） | 首次 + 三次重试 | 用户冻结最终口径；业务预算独立于 MQ 重投 | `internal/delivery/engine.go`; exact 3x503 计数 |
| D07 两类失败 | 已保存业务终态确认；执行异常返回 RocketMQ 重试；HTTP 成功后的结果写库最多重试 3 次且不重发 HTTP；HTTP 重试到期时间与尝试结果同事务持久化 | 任意非空 error 都重发 HTTP；进程重投后忽略已保存退避时间 | 避免把正常业务失败当执行故障或超前重试，也避免数据库短暂错误重复产生外部副作用 | `internal/delivery/engine.go`, `internal/store/store.go`, migration 002；MySQL trigger 和取消/立即重入场景真实验证 |
| D08 快速失败 | MySQL 机构开关、单个配置管理员身份审计、人工恢复，不复活旧终态 | 暂存待恢复、调用方自报审计人、自动复活或自动熔断 | 明确可审计的终止语义；数据库处置失败需与业务冲突分开返回 | fast-fail/resolve API、`ADMIN_ACTOR`、SQL 审计字段；trigger 验证 resolve DB 错误返回 500，恢复后写入配置 actor |
| D09 MQ 异常重试 | SDK/Broker 原生有限重试和 DLQ | Properties 自定义次数 + 应用手工重发 | 只保留一套异常恢复路径 | SDK 2.1.2 配置；实际 retry topic/DLQ 测试 |
| D10 幂等边界 | 入口唯一键、消费状态校验、下游幂等键分别负责 | 声称外部恰好一次 | 各层只解决自身重复；下游需真正去重 | unique key/Fingerprint/attempt table; duplicate E2E |
| D11 日志 | 简短 key=value，关联 trace/task，审计操作，不记正文/凭证 | 无界日志队列、逐层堆栈和独立日志平台 | 降低泄密与运行复杂度；细节放尝试表 | `log/slog`; Docker 脱敏日志检查 |
| D12 人工闭环 | 失败/待核对查询、结论记录、原生 DLQ 检索 | 自动搬运/重放、审批系统 | 保持最小运营闭环；负责人需人工看 DLQ | GET/resolve API、README mqadmin 指令 |
| D13 未来治理 | 按机构限流/分组/故障隔离后再独立扩容 | 把未来平台能力塞入 MVP | 先观测瓶颈；代价是当前单实例/本地配额 | 后续设计在 `assignment-answers.md`，未实现 |
| D14 工程交付 | 固定 D 盘、锁版本、真实 Docker 验收和精简证据 | 只生成代码、Mock MQ、latest 镜像或只给 push 命令 | 保证可复现且可核对；依赖 Docker 环境 | 最新完整验收 `20261014T063922Z-6c5cb01c`，`scripts/verify-docker.*`, `docs/verification.md` |

PDF 没有规定语言、数据库、MQ、公开仓库、CI、覆盖率或性能指标；上述技术栈来自用户补充约束。没有把未执行的人工评审或性能数据当成决策依据。

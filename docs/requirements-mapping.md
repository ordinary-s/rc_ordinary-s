# 要求映射

PDF 来源只覆盖题目原文的交付形式和问题；Go、MySQL、RocketMQ、固定 D 盘、三次 HTTP 业务尝试及真实 Docker 验收均来自用户补充约束。证据目录固定为本轮最新完整验收 [`20261013T214902Z-747fc6e8`](evidence/20261013T214902Z-747fc6e8/)；旧调试输出不作为最终 PASS 依据。

| 来源 | 要求 | 文档章节 | 代码或配置 | 测试证据 | 状态 |
|---|---|---|---|---|---|
| PDF 第 1 页 | README/设计说明问题理解、架构、核心设计和取舍 | [README](../README.md)、[设计](design.md)、[决策](decisions.md) | `cmd/`, `internal/`, `migrations/`, `compose.yaml` | Docker build + E2E；[验收报告](verification.md) | PASS |
| PDF 第 1 页 | 提交 MVP 代码 | README | `cmd/`, `internal/`, `migrations/` | `go test`、应用镜像构建及 API E2E | PASS |
| PDF 第 1 页 | 说明 AI 帮助、未采纳建议和本人决定 | [AI_USAGE](../AI_USAGE.md) | 结合最终实现与讨论记录 | 交付内容核对；不声称人工评审 | PASS |
| PDF 第 2 页 | 系统边界 | [作业答复](assignment-answers.md) §1 | API、notification、delivery | API 受理、终态和人工处置 E2E | PASS |
| PDF 第 2 页 | 投递语义 | [作业答复](assignment-answers.md) §2、[设计](design.md) | Outbox、单条 RocketMQ 消费 | transaction、重复消息、业务三次上限、真实 retry/DLQ | PASS |
| PDF 第 2 页 | 外部临时失败和长期不可用 | [作业答复](assignment-answers.md) §2 | `internal/delivery`, fast-fail 管理 API | 503 计数、fast-fail、过期及人工恢复 | PASS |
| PDF 第 2 页 | 过度设计和未采纳方案 | [作业答复](assignment-answers.md) §3、[决策表](decisions.md)、[AI_USAGE](../AI_USAGE.md) | MVP 未引入额外平台 | Compose/依赖清单与真实测试 | PASS |
| PDF 第 2 页 | 流量或复杂度增长的演进 | [作业答复](assignment-answers.md) §3 | 当前无机构全局配额或队列分组 | 未实现项明确为后续设计 | 后续设计 |
| PDF 第 2 页 | 中间件选择和不用时的替代 | [作业答复](assignment-answers.md) §4 | Go、MySQL、RocketMQ、Outbox | migration、真实依赖 E2E | PASS |
| 用户补充 | `(client_id, idempotency_key)` 原子入口幂等、异内容 409、区分大小写 | [设计](design.md) | `internal/notification`, `internal/store`, migration | 并发 8 请求、1 任务/1 outbox、409 与 case-distinct | PASS |
| 用户补充 | HTTP 最多 3 次、业务失败不重新发布 | [设计](design.md)、[规范](standards.md) | `internal/delivery/engine.go` | 2 次后成功；3x503 mock 恰好 3 次；重复窗口不再发 | PASS |
| 用户补充 | 结果保存错误不重复 HTTP | [设计](design.md) §状态和确认 | `completeAttempt`, `CompleteAttempt` | MySQL trigger：短暂错误第 3 次成功；全部失败后真实 MQ 重投、HTTP 仍 1 次 | PASS |
| 用户补充 | Outbox 发送补偿和重复安全 | [设计](design.md)、[验收报告](verification.md) | `internal/outbox/publisher.go` | Broker 中断恢复；本地 PUBLISHED 标记失败 2 次后重复发布仍只 HTTP 一次 | PASS |
| 用户补充 | RocketMQ 单条回调、有界并发、原生异常重试和 DLQ | [规范](standards.md)、[决策](decisions.md) | `internal/mq/rocket.go`, Topic 初始化脚本 | 真 Broker 重投 `reconsume=1`；真实 DLQ `reconsume_times=3` | PASS |
| 用户补充 | fast-fail 是终止结果，开关关闭不复活旧任务 | [作业答复](assignment-answers.md) | destination 管理 API、条件终态更新 | 0 次 HTTP、关闭后新任务可送、旧任务在观察窗口内不调用 | PASS |
| 用户补充 | Broker 中断、任务过期、SIGTERM 可恢复 | [验收报告](verification.md) | `scripts/verify-docker.ps1`, 应用 shutdown | Broker stop/start + Outbox、过期零调用、SIGTERM 重启后 UNCERTAIN | PASS |
| 用户补充 | UTC、索引、EXPLAIN、幂等键排序规则 | [规范](standards.md) | migration、UTC DSN、Store 查询 | `+00:00`、`idx_outbox_pending_due` range、大小写敏感 E2E | PASS |
| 用户补充 | trace/task 不串号，日志不含敏感内容 | [规范](standards.md) | request-scoped slog 字段 | 8 组 task/trace 行匹配；canary/凭证扫描退出码 0 | PASS |
| 用户补充 | Docker 内 gofmt/vet/unit/race/真实依赖端到端 | [验收报告](verification.md) | `verify-docker.ps1`, Compose verify profile | 最新 run 所有阶段退出码为 0 | PASS |
| 用户补充 | Windows 与 WSL 都有验收入口，固定 D 盘 | README、[验收报告](verification.md) | PowerShell/Bash 脚本 | Windows 完整验证 PASS；WSL D 盘挂载和 `bash -n` PASS，但 WSL 内 `docker info` 因未启用 Desktop 集成退出 1 | WSL Docker entry: BLOCKED；Windows gate: PASS |
| 用户补充 | 按机构限流/分组隔离/独立扩容 | [作业答复](assignment-answers.md) §3 | MVP 未实现 | 无测试；清楚标注未来工作 | 后续设计 |
| 用户补充 | 外部恰好一次、自动熔断、DLQ 自动工单/重放 | README、design、assignment answers | 明确不实现或不承诺 | 只验证实际存在边界，不作为 PASS 功能 | 明确排除 |

注意：本地更新 PUBLISHED 失败的真实窗口已用 MySQL trigger 注入；本次没有通过网络代理丢弃“Broker 已持久化但 SendSync 应答丢失”的 TCP 响应包。重复 Outbox 发送和消费者幂等由独立真实 Broker 用例验证，不把数据库标记故障说成网络层故障注入。

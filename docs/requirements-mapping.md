# 要求映射

本表将交付要求、最终业务规则和工程约束映射到文档与实现。原题 PDF 的名称、页码及问题组织来自候选包的来源登记，本轮未重新读取原 PDF；它只覆盖原题的交付形式和问题。Go、MySQL、RocketMQ、固定 D 盘、三次 HTTP 业务尝试及真实 Docker 验收来自用户补充约束。来源边界和章节覆盖见[参考对齐说明](reference-alignment.md)。

实现核对基线为 `9ed109dce1a7e3373e052d06c7d343c1ba63c652`。运行依据采用用户指定已完成任务的归档验收：**2026-10-14 UTC，`20261014T063922Z-6c5cb01c`，完整脚本退出码 0**，证据随该提交归档。表中的“有归档证据”仅指该运行覆盖列明场景，不表示本轮文档修订重新通过，也不代表所有故障窗口均有验证。原摘要未嵌入 Git SHA；命令、覆盖和适用范围统一见[验收记录](verification.md)。代码与目标的差异单独见[实现核对表](implementation-review.md)，不得用本表状态掩盖。

| 来源 | 要求 | 文档位置 | 代码或配置依据 | 验证范围 | 覆盖状态 |
| --- | --- | --- | --- | --- | --- |
| 原题第 1 页（候选转述） | README/设计说明问题理解、架构、核心设计和取舍 | [README](../README.md)、[设计](design.md) 第 1–17 节、[决策](decisions.md) | `cmd/`、`internal/`、`migrations/`、`compose.yaml` | 文档与源码对照；运行证据见[验收记录](verification.md) | 已提供文档；运行结论独立记录 |
| 原题第 1 页（候选转述） | 提交 MVP 代码 | [README](../README.md) | `cmd/`、`internal/`、`migrations/` | 编译、单元测试、应用镜像及 API E2E，见[验收记录](verification.md) | 代码存在；有归档证据 |
| 原题第 1 页（候选转述） | 说明 AI 帮助、未采纳建议和本人决定 | [AI_USAGE](../AI_USAGE.md)、[决策](decisions.md) | 候选记录、用户本轮规则、Git 历史 | 来源核对，不以测试替代人工评审 | 已提供说明；不虚构人工经历 |
| 原题第 2 页（候选转述） | 系统边界 | [作业答复](assignment-answers.md) 第一部分、[设计](design.md) 第 1–2 节 | API、notification、delivery | API 受理、终态和人工处置，见[验收记录](verification.md) | 已说明实现范围与排除项 |
| 原题第 2 页（候选转述） | 投递语义 | [作业答复](assignment-answers.md) 第二部分、[设计](design.md) 第 6–10 节 | Outbox、单条消费、持久化尝试 | 事务、重复消息、三次上限、原生重试/DLQ，见[验收记录](verification.md) | 已实现；有归档场景证据 |
| 原题第 2 页（候选转述） | 外部临时失败与长期不可用 | [作业答复](assignment-answers.md) 第二部分、[设计](design.md) 第 8–9、12 节 | delivery、机构管理 API | 503 计数、快速失败、过期和人工恢复，见[验收记录](verification.md) | 已实现有界尝试与人工开关 |
| 原题第 2 页（候选转述） | 过度设计和未采纳方案 | [作业答复](assignment-answers.md) 第三部分、[决策](decisions.md)、[AI_USAGE](../AI_USAGE.md) | 当前组件和依赖清单 | 文档与依赖核对，不由测试反推讨论过程 | 已记录取舍与代价 |
| 原题第 2 页（候选转述） | 流量或复杂度增长的演进 | [作业答复](assignment-answers.md) 第四部分、[设计](design.md) 第 16 节 | 当前无机构全局配额或独立投递组 | 无对应上线/容量验证 | 后续设计；未实现 |
| 原题第 2 页（候选转述） | 中间件选择和不用时的替代 | [作业答复](assignment-answers.md) 第五部分、[设计](design.md) 第 3 节 | Go、MySQL、RocketMQ、Outbox | 当前依赖运行见[验收记录](verification.md)；替代方案未实施 | 已说明现行选择与替代成本 |
| 用户补充 | 原子入口幂等、同键异内容 409、区分大小写 | [设计](design.md) 第 5–6 节 | `internal/notification/accept.go`、`Store.Accept`、migration 001 唯一约束 | `acceptance_transaction_and_idempotency`、`case_sensitive_and_parallel_idempotency`，见[验收记录](verification.md) | 已实现；有归档证据 |
| 用户补充 | HTTP 最多三次，终止业务失败不重新发布 | [设计](design.md) 第 7–8 节、[规范](standards.md) | `internal/delivery/engine.go`、`internal/mq/rocket.go` | 503 后成功、连续三次 503、终态重复投递，见[验收记录](verification.md) | 业务上限已实现；物理调用上限缺口见实现核对表 R01 |
| 实现加固 / 工程决策 | 重试到期时间跨重投/中断持久保存，不提前请求 | [设计](design.md) 第 8、10–11 节、[规范](standards.md) | `notification.next_attempt_at`、migration 002、delivery/store | `persisted_retry_due_survives_context_cancellation`，见[验收记录](verification.md) | 已实现；有取消后恢复的归档证据 |
| 用户补充 | 结果保存错误不直接重复 HTTP | [设计](design.md) 第 10、12 节 | `Engine.completeAttempt`、`Store.CompleteAttempt` | 暂时写库失败后保存成功；持续失败后重投转 UNCERTAIN，HTTP 仍一次，见[验收记录](verification.md) | 已实现；有故障注入证据 |
| 用户补充 | Outbox 补偿、标记失败退避和重复安全 | [设计](design.md) 第 6–7 节 | `internal/outbox/publisher.go`、Store 待发布查询 | `outbox_send_mark_failure_duplicate_safe`；MySQL trigger 注入，见[验收记录](verification.md) | 已实现；未覆盖 Broker 确认包网络丢失注入 |
| 用户补充 | 单条回调、有界并发、原生异常重试和 DLQ | [设计](design.md) 第 7、13 节、[规范](standards.md) | `internal/mq/rocket.go`、Topic/Group 初始化脚本 | 真实重投 `reconsume=1`、DLQ `reconsume_times=3`，见[验收记录](verification.md) | 已实现；证据适用于锁定 Broker/SDK 组合 |
| 用户补充 | 快速失败为终态，关闭开关不复活旧任务 | [设计](design.md) 第 9 节、[作业答复](assignment-answers.md) 第二部分 | destination 管理 API、条件终态更新 | 开启时零 HTTP，关闭后新任务可送、旧任务保持终态，见[验收记录](verification.md) | 已实现检查与终态；严格切换缺口见实现核对表 R02 |
| 实现加固 / 工程决策 | 管理员身份可信；处置 DB 故障 500，业务冲突 409 | [设计](design.md) 第 13、15 节、[规范](standards.md) | `ADMIN_ACTOR`、API、`Store.Resolve` | 受控 DB 错误及恢复、配置审计身份，见[验收记录](verification.md) | 已实现；有归档证据 |
| 用户补充 | 任务查询、人工结论与异常 DLQ 核对 | [设计](design.md) 第 13 节、[README](../README.md) | 客户端查询、管理员 resolve、Broker 原生 DLQ | resolve 只覆盖四类失败终态；原生 DLQ 场景不等于所有任务均可结案 | 部分实现；中间态/缺失任务缺口见实现核对表 R03 |
| 用户补充 | Broker 中断、任务过期、SIGTERM 可恢复 | [设计](design.md) 第 10、17 节 | `scripts/verify-docker.ps1`、应用 shutdown | Broker 停启与 Outbox、过期零调用、SIGTERM 后 UNCERTAIN，见[验收记录](verification.md) | 有归档场景证据；不等于任意故障均可恢复 |
| 用户补充 | UTC、索引、EXPLAIN、幂等键排序规则 | [设计](design.md) 第 11 节、[规范](standards.md) | migrations、UTC DSN、Store 参数查询 | `+00:00`、待发索引 range、大小写 E2E，见[验收记录](verification.md) | 已实现；有归档证据 |
| 用户补充 | trace/task 不串号，日志不含正文及凭证 | [设计](design.md) 第 14 节、[规范](standards.md) | 显式 slog 字段、任务与 MQ event 的 ID | 8 组 task/trace 匹配与 canary/凭证扫描，见[验收记录](verification.md) | 部分覆盖；字段与错误脱敏缺口见实现核对表 R04 |
| 用户补充 | Docker 内 gofmt/vet/unit/race/真实依赖 E2E | [验收记录](verification.md) | PowerShell 脚本、Compose verify profile | 2026-10-14 UTC 归档运行 `20261014T063922Z-6c5cb01c` 的各阶段退出码 | 已归档通过；范围见[验收记录](verification.md) |
| 用户补充 | Windows/WSL 入口，固定 D 盘 | [README](../README.md)、[验收记录](verification.md) | PowerShell/Bash 脚本 | 指定归档运行使用 Windows PowerShell | Windows 已归档通过；WSL 不在该运行验收范围 |
| 用户补充 | 入口限流、机构出口 QPS/并发限制、分组与独立扩容 | [作业答复](assignment-answers.md) 第四部分、[设计](design.md) 第 16 节 | MVP 只有当前全局有界并发和资源限制 | 未实施，无功能或容量验收 | 后续设计 |
| 用户补充 | 外部恰好一次、完整自动熔断、DLQ 自动工单/重放 | [README](../README.md)、[设计](design.md)、[作业答复](assignment-answers.md) | 当前不提供这些承诺或功能 | 无对应通过声明 | 明确排除；需要时另行设计 |

Outbox 的归档注入场景是“Broker 已接收后，本地 PUBLISHED 更新失败”，并非通过网络代理丢弃 SendSync 应答。原生重投、重复发布和消费者幂等虽有独立场景证据，仍不能据此宣称所有消息丢失/重复故障窗口都已验证。历史报告不因本轮文档修订而更改原始时间、版本或结果。

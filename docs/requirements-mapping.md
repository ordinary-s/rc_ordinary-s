# 作业与用户要求映射

状态约定：PASS 只用于在本次最新 Docker run 中实际验证的要求；NOT_RUN 表示没有执行；FAIL 表示运行实际失败；BLOCKED 表示环境/依赖阻塞；后续设计表示明确未进入 MVP。最新 run 与逐项结果见[验收记录](verification.md)。

| 来源 | 要求 | 文档章节 | 代码或配置 | 测试证据 | 状态 |
|---|---|---|---|---|---|
| 作业 PDF 第 1 页摘录 | 说明问题理解、架构、核心设计和取舍 | README、design、decisions | 项目源码、Compose | requirements/链接审阅；Docker run 环境证据 | PASS（交付文档） |
| 作业 PDF 第 1 页摘录 | 提供可读、可复现 MVP | README、design、verification | cmd、internal、migrations、Dockerfile、Compose、scripts | 最新 reports 与 docs/evidence run；以 verification.md 退出码为准 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 作业 PDF 第 1 页、第 2 页摘录 | 说明 AI 帮助、未采纳建议及本人决策 | AI_USAGE、assignment-answers、decisions | — | 对照最终消息协议与源码 | PASS（交付文档） |
| 作业 PDF 第 2 页摘录 | 逐项回答边界、投递、外部失败、AI 取舍、演进和基础设施选择 | assignment-answers | 当前代码及配置作为事实依据 | 文档逐题检查 | PASS（交付文档） |
| 后续 MQ 接入提示词 | 上游直接发完整版本化 MQ 事件；服务消费前先持久化，不保留本地 HTTP submit/Publisher | README、design、D03/D04 | internal/mq、internal/notification、internal/store、cmd/server | 真实 Broker 事件发布/消费 E2E | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 后续 MQ 接入提示词 | schema 版本、事件键、trace、目标、method/header/raw UTF-8 body、UTC expires_at 明确 | design §MQ 协议 | internal/mq/rocket.go、config、migration 004 | MQ decoder 单测、真实 XML/form/raw body 与过期场景 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 后续 MQ 接入提示词 | 身份由配置绑定；同事件同内容复用；同 ID 不同内容不覆盖、不调用 HTTP | design §MQ 协议 | MQ_TRUSTED_CLIENT_ID、notification.Ingest、source_event_id unique key | 真实 MySQL 并发 duplicate/conflict + mock 计数 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：三次业务尝试 | HTTP 总共最多三次；正常失败保存并 ACK；不生成业务失败重试 | assignment-answers、design 状态表 | internal/delivery/engine.go、internal/mq/rocket.go | 真实 Broker + 503 mock 三次断言和重复消息窗口 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：执行异常 | 只有未可靠完成时返回 RocketMQ 原生重试；达到上限进入原生 DLQ | design §事务与 ACK | internal/mq/rocket.go、MQ_MAX_RECONSUME | DB 故障、坏协议事件/冲突事件的实际 Broker 重投/DLQ | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：失败恢复窗口 | HTTP 前未落尝试可恢复；STARTED/远端结果未知不能盲目重发；SIGTERM 不提前 ACK | design §事务与 ACK | delivery.Engine、attempt 状态、cmd/server shutdown | MySQL 故障注入和宿主机 SIGTERM 脚本 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：快速失败/到期 | FAST_FAILED 是终止；关闭开关不复活旧任务；expires_at 是绝对 UTC | assignment-answers、design 状态表 | API、destination 表、store、delivery | 真实 MySQL/API/mock 次数与观察窗口 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：TLS/HMAC | TLS 1.3 保护传输；Bearer 与 HMAC 签名/nonce 校验；HMAC 不加密 | README、design、standards、D15 | cmd/server、internal/signature、migration 003 | 真实 HTTPS 1.3/拒绝 TLS1.2/签名篡改和重放 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：目标安全和原始文本 | 仅受控 HTTP(S) target、限制 Body/Header、禁重定向、支持 XML/form 文本 | README、design §边界 | notification validation、delivery Transport、destination seed | XML/form mock 透传及目标 allowlist 场景 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：工程规范 | 固定 Go/MySQL/RocketMQ；Go 格式、vet、test、race；版本 migration | AGENTS、standards | Dockerfile、go.mod/sum、migrations、scripts | 最新 verify log 的每个 exit code | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：Docker E2E | 真实 Go 应用、MySQL、Broker、NameServer、mock target；API/消费者生产路径可复现 | README、verification | compose.yaml、compose.verify.yaml、verify scripts | 最新隔离 project 的 integration.json、checks.log、compose 状态 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：旧版切换 | 不混用旧 ID-only 事件；保留旧 outbox 表/卷；新 Topic/Group；先排空/核对 | README §迁移、design §切换 | migration 004、新 Topic/Group 配置 | 当前集成测试使用新 schema；旧数据升级专用故障演练未运行 | 文档完成；旧库升级演练 NOT_RUN |
| 用户补充：短日志与隐私 | trace/task 关联，简短日志不含完整正文、凭证和敏感响应 | AGENTS、standards | slog、Compose 日志上限 | 并发 task/trace 配对与 canary 扫描 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：人工边界 | 查询失败/待核对，保存结论，MQ DLQ 独立查询；不自动待办/重放 | README、design、D12 | query/resolve API、RocketMQ admin 入口 | API resolution 和真实 DLQ 场景；DLQ 自动化工单未实现 | PASS（最新 run 20261015T150311Z-bf5a516b） |
| 用户补充：安全交付 | 不提交 .env、凭证、题目 PDF、完整聊天或无关数据 | README、AI_USAGE | .gitignore、.dockerignore、仅精确暂存 | commit 前 secret/diff/staged 审核 | 提交前检查 |
| 用户补充：未来流量演进 | client/机构限流、机构分组、故障隔离和拆分扩容 | assignment-answers §演进、D13 | MVP 不实现 | 无性能压测要求 | 后续设计 |
| PDF 未要求项 | 不要求公开仓库、CI、覆盖率门槛或性能指标 | 本仓库不将其列为门槛 | 无 | 未额外执行/建立 | NOT_RUN / 非验收项 |

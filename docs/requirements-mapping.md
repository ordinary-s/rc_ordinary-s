# 要求映射

状态含义：`PASS` 表示最新代码在有记录的 Docker 验收中实际通过；`FAIL` 表示实际失败；`NOT_RUN` 表示未执行；`后续设计` 表示文档提出、MVP 尚未实现。当前通过依据为 `20261015T074216Z-479b6b19`，完整记录见 `docs/verification.md`。

| 来源 | 要求 | 文档章节 | 代码或配置 | 测试证据 | 状态 |
|---|---|---|---|---|---|
| 作业 PDF 第 1 页摘录 | README/设计说明阐明问题、架构、核心设计与取舍 | README、`docs/design.md`、`docs/decisions.md` | 项目源码与 Compose | `docs/verification.md` | PASS（文档存在，验收以最终报告为准） |
| 作业 PDF 第 1 页摘录 | MVP 代码实现，可独立阅读和复现 | README、`docs/design.md` | `cmd/`、`internal/`、`migrations/`、`compose.yaml` | `20261015T074216Z-479b6b19`，17 个真实 E2E 场景通过 |
| 作业 PDF 第 1 页及第 2 页摘录 | AI 帮助、未采纳建议、本人关键判断；逐项答复题目 | `AI_USAGE.md`、`docs/assignment-answers.md`、`docs/decisions.md` | — | 文档审阅及相对链接检查 | PASS（文档核对） |
| 用户补充 | D 盘项目路径、Go/MySQL/RocketMQ 工程规则与单实例边界 | `AGENTS.md`、`docs/standards.md`、`docs/design.md` | migrations、Store、MQ、Dockerfile、Compose | 完整 Docker 验收脚本 | PASS |
| 用户补充：TLS/HMAC | API 使用 TLS 1.3；HMAC 验证请求完整性并带时效/nonce 防重放；不另造 Body 加密 | README、`docs/design.md`、`docs/decisions.md`、`AI_USAGE.md`、`docs/standards.md` | `cmd/server/main.go`、`internal/signature/`、`internal/api/server.go`、migration 003、Compose cert volume | TLS1.3 协商/TLS1.2 拒绝；签名缺失、正文/Authorization 篡改、过期、重放拒绝；客户端 key 访问管理员接口拒绝 | PASS |
| 用户补充 | 至多三次 HTTP 业务尝试；终态失败消费完成，不因业务失败重新入队 | `docs/design.md`、`docs/assignment-answers.md` | `internal/delivery/`、`internal/mq/` | 3 次 503、重复消息、真实 MQ 异常重投 | PASS |
| 用户补充 | MySQL Outbox 与通知同事务；真实 Broker 投递与重复安全 | `docs/design.md` | `internal/store/`、`internal/outbox/`、`internal/mq/` | MySQL/RocketMQ E2E、Broker 中断恢复 | PASS |
| 用户补充 | Docker 内 gofmt、vet、普通测试、race、真实依赖 E2E，全部阶段退出码准确 | `docs/verification.md` | `scripts/verify-docker.ps1`、`.sh`、Compose override | `reports/20261015T074216Z-479b6b19/` 与 `docs/evidence/20261015T074216Z-479b6b19/` | PASS |
| 用户补充 | 单条消费、原生异常重试与 DLQ；不增加业务失败重放路径 | `docs/design.md`、`docs/decisions.md` | `internal/mq/rocket.go` | 数据库异常重投、DLQ 到达条件 | PASS |
| 用户补充 | 配置快速失败、恢复后不自动复活终态；有人工核对记录 | README、`docs/design.md` | API、Store、Compose seed | 快速失败/关闭恢复/旧任务不复活场景 | PASS |
| 用户补充 | 不提交凭证、完整请求/响应或无关附件；保留既有仓库可见性 | `README.md`、`docs/verification.md` | `.gitignore`、`.dockerignore` | 暂存区安全扫描；`ordinary-s/rc_ordinary-s` 的公开状态、`main` 分支 SHA 与核心文件通过 GitHub API 回读 | PASS |
| 用户补充：增长路线 | 调用方限流、机构配额、分组隔离、独立扩容与自动熔断 | `docs/assignment-answers.md`、D13 | — | 未作为 MVP 验收门槛 | 后续设计 |
| 用户补充：边界 | 多实例全局限流、HA、自动 DLQ 工单、终态自动重放 | `docs/design.md`、`docs/implementation-review.md` | 当前未实现 | 不适用 | 后续设计 |

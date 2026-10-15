# 工程规范落实

这里记录本项目采用的规范依据、具体约定和可核验位置。表内命名、时间写法和限额属于项目约定，不表示对应技术的唯一官方做法。

| 规则 | 本项目落实位置 | 自动检查或真实验证 |
|---|---|---|
| Go 使用短职责包、显式依赖、context 传播、错误返回；导出 API 说明关键行为 | `cmd/`、`internal/`；I/O 入口显式接收 context；服务生命周期在 `cmd/server/main.go` 管理 | `scripts/verify-docker.ps1` / `.sh` 在固定 Go 容器中运行 gofmt、`go vet ./...`、`go test -count=1 ./...`、`go test -race -count=1 ./...` |
| Go 格式与依赖可复现 | `go.mod`、`go.sum`；Dockerfile 固定带 digest 的 Go 镜像 | 验收脚本检查 gofmt 无差异并构建应用/test-runner；输出分别记录退出码 |
| TLS 仅使用 TLS 1.3 或更高版本；依赖 Go 标准库默认安全密码套件，不硬编码 cipher 列表 | `cmd/server/main.go` 设置 `tls.Config.MinVersion=tls.VersionTLS13`，证书来自 `TLS_CERT_FILE` / `TLS_KEY_FILE`；健康检查校验本地 CA | 真实 HTTPS E2E 检查协商版本为 TLS 1.3，并用 TLS1.2-only 客户端确认握手被拒绝；Docker run 证据包含应用握手日志 |
| HTTP API 的 HMAC 签名验证身份/完整性，不能代替 TLS；摘要按 RFC 9530，签名按 RFC 9421 | `internal/signature/signature.go` 使用 `hmac-sha256`，覆盖 `@method`、`@authority`、`@target-uri`、`@path`、`authorization`、`content-digest`；通知提交再覆盖 `idempotency-key` | 单元和真实 API E2E 检查无签名、篡改正文、过期签名均被拒绝；标准定义并不替代本项目测试 |
| 请求签名要防重放，密钥角色最小区分 | `internal/api/server.go` 强制 client/admin key ID 与 Bearer token 配对；`migrations/003_api_signature_nonce.sql` 以二进制 `(key_id, nonce)` 唯一键持久化 nonce，并索引到期清理 | 真实 MySQL E2E 重放同一签名得到 401；重复键唯一冲突按 replay 分类；nonce 写库故障返回 503 |
| MySQL 主键、非空关联、唯一约束与索引对应实际查询 | `migrations/001_initial.sql` 至 `003_api_signature_nonce.sql`；表/列/索引使用小写 snake_case（项目约定）；幂等键、nonce 使用区分大小写比较 | Docker E2E 验证并发幂等、区分大小写、Outbox `EXPLAIN` 与 nonce 重放；migration 在隔离空库执行 |
| InnoDB 事务边界短且明确；notification 与 Outbox 原子提交，网络调用不在事务内；DDL 隐式提交按迁移处理 | `internal/store/store.go`；迁移脚本按序执行；运行应用账号只需 DML，迁移入口使用受限于本地初始化的账户 | 真实 MySQL 检查 notification/outbox 同时存在；MySQL 注入场景覆盖更新故障与恢复；`EXPLAIN` 保存于 E2E 报告 |
| UTC `DATETIME(6)` 与 UTF-8；用户 SQL 参数化、显式列名、分页有界稳定 | MySQL Compose 启动参数、DSN `parseTime=true&loc=UTC&charset=utf8mb4`、migration 和 Store 查询 | Docker E2E 查询连接时区 `+00:00`、查询 Outbox 索引计划并验证请求正文保真 |
| RocketMQ 固定 Broker/Go SDK 版本；单条确认、有限并发；Outbox 仅在确认 `SendOK` 后标记发布；异常使用原生重试/DLQ | RocketMQ `5.5.0` 镜像；`rocketmq-client-go/v2 v2.1.2`；`internal/mq/rocket.go`、`internal/outbox/publisher.go`、Compose Topic/Group init | 真实 Broker E2E 覆盖发布/消费、重复消息、数据库异常重投、原生重试进入 DLQ；Broker 中断恢复与 SIGTERM 由宿主脚本驱动 |
| 不记录凭证和完整通知内容；每请求日志上下文不共享可变 task/trace | `internal/api/`、`internal/delivery/`、`internal/outbox/` 使用局部结构化字段；Compose 日志轮转 | E2E 检查并发 task/trace 配对、正文 canary /凭证扫描；该扫描是有限覆盖，不代表全部异常路径形式化脱敏 |
| 测试环境隔离，依赖就绪按 healthcheck 等待，失败返回非零 | `compose.yaml`、`compose.verify.yaml`、`scripts/verify-docker.ps1`、`scripts/verify-docker.sh` | 验收构建应用和工具链镜像、跑 MySQL/RocketMQ/mock target 实测；使用隔离 project 与命名卷，导出 `reports/<run_id>/` |

## 官方资料

- [Effective Go](https://go.dev/doc/effective_go)、[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)、[Go Race Detector](https://go.dev/doc/articles/race_detector)
- [Go `crypto/tls` for Go 1.27.2](https://pkg.go.dev/crypto/tls@go1.27.2)
- [TLS 1.3: RFC 9846](https://www.rfc-editor.org/rfc/rfc9846.html)；版本修订保留 TLS 1.3 语义
- [HTTP Message Signatures: RFC 9421](https://www.rfc-editor.org/rfc/rfc9421.html)；[Content-Digest: RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html)
- [MySQL 8.4 InnoDB commit/rollback](https://dev.mysql.com/doc/refman/8.4/en/innodb-autocommit-commit-rollback.html)、[primary key](https://dev.mysql.com/doc/refman/8.4/en/constraint-primary-key.html)、[implicit commit](https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html)
- [RocketMQ consumer retry policy](https://rocketmq.apache.org/docs/featureBehavior/10consumerretrypolicy/)、[best practices](https://rocketmq.apache.org/docs/bestPractice/01bestpractice/)、[rocketmq-client-go v2.1.2](https://github.com/apache/rocketmq-client-go/tree/v2.1.2)
- [Docker Compose startup order](https://docs.docker.com/compose/how-tos/startup-order/)、[networking](https://docs.docker.com/compose/how-tos/networking/)、[compose run](https://docs.docker.com/reference/cli/docker/compose/run/)

MySQL 的 snake_case、UTC `DATETIME(6)` 与二进制比较是项目一致性约定，依据本项目查询和幂等契约选择，并非 MySQL 官方要求的唯一命名或时间方案。

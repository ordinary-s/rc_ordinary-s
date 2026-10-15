# 工程规范依据与落实位置

本表把工程规则、代码/配置位置与实际自动检查连接起来。MySQL 命名、UTC 时间表示、限额等是项目约定，不是相关产品唯一官方做法。

| 规则 | 代码或配置落实 | 自动检查 / 真实验证 |
|---|---|---|
| Go 使用短职责包、MixedCaps 导出名、显式依赖；I/O 接收 context；普通错误返回并包装；管理 goroutine 退出 | cmd/server、internal/api、notification、store、delivery、mq | Docker 测试镜像 gofmt 检查、go vet ./…、go test -count=1 ./…、go test -race -count=1 ./… |
| Go 源码格式、依赖和工具链可复现 | go.mod/go.sum、Dockerfile 固定 Go 镜像摘要 | verify-docker 在固定 Linux Go 工具镜像执行 gofmt 差异检查和 go mod tidy -diff；镜像构建记录 |
| RocketMQ 版本/API 配套 | RocketMQ 5.5.0；github.com/apache/rocketmq-client-go/v2 v2.1.2；internal/mq/rocket.go | 集成测试使用真实 5.5.0 NameServer/Broker；实际发布、单条消费、执行失败重投与 DLQ |
| RocketMQ 消费确认遵守 Go PushConsumer API | callback 验证消息恰为一条；已保存终态返回 ConsumeSuccess；未完成返回 ConsumeRetryLater + error；WithMaxReconsumeTimes 有界 | 真实 Broker E2E 核验 DB 异常重投和达到上限的 DLQ；ProtocolError 不跳过原生处理 |
| Topic/Group 显式创建且地址可路由 | notification_ingress_v1 / notification_ingress_workers_v1；Compose mq-init；服务解析 NameServer DNS，Broker advertised address 为容器可达地址 | 隔离环境初始化后检查 readyz；跨容器测试实际发送/消费，不把容器 running 作为成功 |
| 上游 MQ 事件版本、大小、编码明确 | internal/mq/rocket.go 的 DecodeEvent；schema_version=1、限制字节数、拒绝未知字段/尾随 JSON/非 UTF-8 | rocket_test.go 覆盖未知版本/字段、UTC、大小、非法 UTF-8、尾随 JSON；真实 E2E 覆盖协议错误 DLQ |
| 新入口先持久化再做外部 I/O | internal/notification/accept.go 校验和指纹；internal/store/store.go 用唯一键保存 notification；delivery 在落库后调用 | MySQL 注入任务写入失败时确认无 HTTP，再观察原生 MQ 恢复；同 event_id 并发重复只保留一个任务 |
| 事件幂等与大小写行为明确 | migration 004 为 (client_id,source_event_id) 添加唯一键；二进制区分大小写 collation；trace_id 不纳入指纹 | 真实 MySQL 验证并发重复、相同键异内容冲突、重投不增加 HTTP |
| 旧 outbox 兼容不代表新版仍发布 | migration 001 的 outbox 表保留；新版 Store 不创建、不更新、不扫描 outbox；无 internal/outbox Publisher | 代码审阅和真实 E2E 直接从 MQ 驱动；不以旧 Outbox 测试描述新流程 |
| MySQL/InnoDB 事务、主键、约束、索引与 DDL 行为 | migrations/*.sql；internal/store/store.go；运行 DML 账号与迁移入口分离 | 新隔离库顺序 migration；真实 MySQL 验证唯一约束、UTC 连接与查询状态；migration 用新增文件演进 |
| 表/列/索引小写 snake_case、utf8mb4、明确 collation；UTC DATETIME(6) | migrations/001_initial.sql、migration 004、DSN parseTime/loc=UTC/charset=utf8mb4 | 集成测试查询 MySQL 会话 UTC、幂等标识大小写和关键索引；这些格式是项目约定 |
| SQL 参数化、显式列、稳定分页；连接池和 I/O 有限 | internal/store/store.go；internal/config/config.go；http.Transport 与 DB pool 配置 | 集成 API 分页/客户端隔离；docker logs 和测试约束；README 明示 Body/Header/消息体限额 |
| HTTP 尝试、MQ 重投分离；副作用不明时停止自动重发 | internal/delivery/engine.go、delivery_attempt 状态更新和恢复逻辑 | 503 mock 精确断言三次应用尝试和三次 mock 请求；DB 异常真 Broker 重投；SIGTERM 后 UNCERTAIN 且无第二次 HTTP |
| API TLS 版本与标准签名 | cmd/server/main.go 设置 MinVersion TLS 1.3；internal/signature 使用 RFC 9421 HMAC-SHA-256 和 RFC 9530 Content-Digest | 真实 HTTPS 测 TLS1.3 协商、TLS1.2 拒绝、无签名/篡改/过期签名失败 |
| API nonce 防重放、身份分权 | migration 003 唯一保存 key_id+nonce；internal/api/server.go 区分 client/admin bearer 与签名 key | 真实 MySQL 重放返回拒绝；客户端 key 访问管理员接口失败；HMAC 不被描述成加密 |
| SSRF、HTTP 请求大小及隐私控制 | destination host/scheme/port allowlist、header/body 限额、redirect 禁用、TLS 校验保留；日志不写全文 | mock target 只在 Compose 网络 allowlist；原始 XML/表单 E2E；日志敏感 canary 扫描 |
| Compose 依赖健康、隔离与报告 | compose.yaml、compose.verify.yaml、verify-docker.ps1/.sh | 脚本检查 Docker/Compose、compose config --quiet、健康等待、build、测试、退出码、脱敏报告和仅清理本 project |

## 官方依据

- Go： [Effective Go](https://go.dev/doc/effective_go)、[Code Review Comments](https://go.dev/wiki/CodeReviewComments)、[Race detector](https://go.dev/doc/articles/race_detector)、[net/http Go 1.27.2](https://pkg.go.dev/net/http@go1.27.2)。
- MySQL 8.4： [InnoDB commit/rollback](https://dev.mysql.com/doc/refman/8.4/en/innodb-autocommit-commit-rollback.html)、[primary key](https://dev.mysql.com/doc/refman/8.4/en/constraint-primary-key.html)、[implicit commit](https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html)。
- RocketMQ： [Consumer retry policy](https://rocketmq.apache.org/docs/featureBehavior/10consumerretrypolicy/)、[Best practices](https://rocketmq.apache.org/docs/bestPractice/01bestpractice/)、[rocketmq-client-go v2.1.2 source/examples](https://github.com/apache/rocketmq-client-go/tree/v2.1.2)。
- HTTP 安全： [TLS 1.3 RFC 9846](https://www.rfc-editor.org/rfc/rfc9846.html)、[HTTP Message Signatures RFC 9421](https://www.rfc-editor.org/rfc/rfc9421.html)、[Content-Digest RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html)。
- Docker： [Compose startup order](https://docs.docker.com/compose/how-tos/startup-order/)、[Compose networking](https://docs.docker.com/compose/how-tos/networking/)、[docker compose run](https://docs.docker.com/reference/cli/docker/compose/run/)。

MySQL 小写 snake_case、UTC DATETIME(6)、特定索引名和二进制 collation 是本项目为可读性、时区一致性和事件键语义选择的约定，不是 MySQL 官方唯一的命名或存储方式。RocketMQ 重试实际时间和 DLQ 行为以固定 Broker、SDK 和本仓库真实 Docker 验收为准；不根据 Java 配置名推导 Go SDK 能力。

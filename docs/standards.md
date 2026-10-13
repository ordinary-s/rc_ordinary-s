# 工程规范与落实位置

下表把官方文档中的适用原则与本项目约定分开。表名/列名采用 snake_case、统一 UTC DATETIME 和本地报告结构属于项目工程约定，不是 MySQL 官方唯一命名规则。

| 领域/规则 | 本项目代码或配置 | 自动检查/真实验证 |
|---|---|---|
| Go：标准库优先、MixedCaps、短职责包、显式依赖/context、错误包装；参考 [Effective Go](https://go.dev/doc/effective_go)、[Code Review Comments](https://go.dev/wiki/CodeReviewComments) | `cmd/*`, `internal/{api,config,delivery,mq,notification,outbox,store}` | pinned Go test-runner 执行 gofmt 差异检查、`go vet ./...`、`go test -count=1 ./...`、`go test -race -count=1 ./...`；race 镜像基于 Debian CGO 工具链 |
| Go HTTP：复用 Client/Transport、超时、TLS 校验、禁重定向；参考 [`net/http`](https://pkg.go.dev/net/http) | `internal/delivery/engine.go` | API E2E 计数 503；明确记录 Transport 内部重试边界，未声称网络发送绝不超过三次 |
| RocketMQ：版本匹配、单条 PushConsumer、有界并发、返回消费结果、发送确认、原生异常重试/DLQ；参考 [RocketMQ Go SDK](https://rocketmq.apache.org/docs/sdk/05go/)、[SDK 概览](https://rocketmq.apache.org/docs/sdk/01overview/)、[消费重试](https://rocketmq.apache.org/docs/featureBehavior/10consumerretrypolicy/) | `internal/mq/rocket.go`; Topic/Group 在 `scripts/init-rocketmq.sh`; `compose.yaml` 明确 broker route `broker:10911` | 真 Broker E2E：Outbox 发布/消费、重复事件、DB 异常 retry topic、DLQ；`SendSync` 只在 `SendOK` 标记成功 |
| SDK 版本/API：使用 `rocketmq-client-go/v2 v2.1.2` Remoting SDK，NameServer + Broker，不使用 Proxy；回调 `ConsumeResult,error`；消费 timeout 是 `time.Duration`；发送返回 SendResult | `go.mod`, `internal/mq/rocket.go` | Docker 源码构建与实测；SDK resolver 校验 NameServer literal IP，因此 Compose service DNS 先解析为 IP，而 Broker 路由保持容器内可达 DNS |
| MySQL 事务/主键/DDL 隐式提交；参考 [事务提交](https://dev.mysql.com/doc/refman/8.4/en/innodb-autocommit-commit-rollback.html)、[主键约束](https://dev.mysql.com/doc/refman/8.4/en/constraint-primary-key.html)、[隐式提交](https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html) | `migrations/001_initial.sql`, `cmd/migrate`, `internal/store/store.go` | MySQL 8.4.8 真容器执行 migration；同键并发/不同键 409；transaction/outbox 行计数 |
| MySQL 命名和比较规则：snake_case/utf8mb4；幂等键大小写敏感；索引按实际查询创建 | `migrations/001_initial.sql` 明确 `utf8mb4_0900_bin`、`ascii_bin`、唯一约束、`idx_outbox_pending_due`、调用方查询索引 | case-distinct key E2E；真实 `EXPLAIN` 记录 type/range、rows、key |
| 时间：UTC DATETIME(6)，读写使用 UTC | schema `DATETIME(6)`；写入使用 `UTC_TIMESTAMP(6)`；Go DSN `parseTime=true&loc=UTC` | 集成读回 UTC 字段，test-runner 验证 MySQL session time zone；Compose MySQL 默认时区 `+00:00` |
| SQL：显式列名、参数化、受限分页、短事务、状态条件更新 | `internal/store/store.go`, `internal/api/server.go` | go vet、真实 API 分页/状态；条件状态/次数检查在 SQL 更新语句里 |
| 外部 HTTP 以外的结果保存恢复：HTTP 已成功后只重试结果事务，边界有限且可取消；仍失败才交给 MQ，不能重发 HTTP | `internal/delivery/engine.go:completeAttempt`, `internal/store/store.go:CompleteAttempt` | MySQL trigger 注入两次暂错后第三次保存成功；再注入三次失败后观察真实 RocketMQ 重投，任务为 UNCERTAIN 且 mock 请求仍为 1 |
| Runtime DB least privilege，迁移与运行时用户分开；连接池复用 | `scripts/mysql-runtime-user.sh`, Compose migration/runtime DSN, `store.Open` | 初始化日志和 E2E 使用 MySQL 容器；应用连接使用 DML 用户，迁移容器单独使用启动 root |
| Compose：健康依赖、服务 DNS、命名卷、资源/日志限额；参考 [启动顺序](https://docs.docker.com/compose/how-tos/startup-order/)、[网络](https://docs.docker.com/compose/how-tos/networking/)、[compose run](https://docs.docker.com/reference/cli/docker/compose/run/) | `compose.yaml`, `compose.verify.yaml`, `scripts/verify-docker.ps1`, `.sh` | `docker compose config --quiet`；依赖 healthcheck；Broker 实际停启、应用 API、MQ、DB、mock target 全链路 |

## 固定版本和相容性

| 组件 | 固定版本/镜像 |
|---|---|
| Go | 1.27.2；`golang:1.27.2-bookworm` digest 锁定 |
| MySQL | 8.4.8；镜像 digest 锁定 |
| Apache RocketMQ | 5.5.0 NameServer/Broker；镜像 digest 锁定 |
| Go SDK | `github.com/apache/rocketmq-client-go/v2 v2.1.2` |
| Docker 验收宿主工具 | Docker Desktop `4.69.0`, Engine `29.4.0`, Compose `5.1.1` |
| Compose | 本机验收时记录 Docker Compose CLI 实际版本 |

选定 SDK 是 RocketMQ 4.x Remoting 协议的官方 Go v2 API，可连接本项目 RocketMQ 5.x Broker 的兼容 Remoting listener；不使用 `rocketmq-clients/golang` gRPC API，因此本拓扑不需要 Proxy。SDK `NamesrvAddr` 校验要求 IP 格式，代码将 NameServer 服务名解析为 IP，再由 NameServer 返回 Compose Broker route `broker:10911`。是否可达最终以 E2E 为准。

MySQL 8.4.8 使用 utf8mb4 与 InnoDB。表/列命名、`DATETIME(6)` UTC 使用和严格二进制比较是本项目约定；MySQL DDL 隐式提交，因此 migration 失败恢复依靠版本表和可重跑 DDL，不假设整组 DDL 回滚。

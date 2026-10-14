# Docker 验收记录

## 最新完整验收

**结果：PASS。** 2026-10-14 UTC 在 Windows PowerShell 中对最新代码运行 `scripts/verify-docker.ps1`，脚本退出码为 `0`。格式检查、静态检查、普通测试、race、真实 MySQL/RocketMQ 集成/E2E、Broker 停启、SIGTERM、日志扫描和本次隔离资源清理均通过。

| 项目 | 实际值 |
|---|---|
| 固定项目目录 | `D:\rc_notification` |
| WSL 映射 | `/mnt/d/rc_notification`；之前已由 `findmnt` 核实为 D 盘可写挂载 |
| 运行 ID / Compose project | `20261014T063922Z-6c5cb01c` / `rcnotif-verify-20261014t063922z-6c5cb01c` |
| 验收开始 / 结束 | `2026-10-14T06:39:22Z` / `2026-10-14T06:44:17Z` |
| Docker Desktop / Engine / Compose | `4.69.0` / `29.4.0` Linux `amd64` / `5.1.1` |
| Go / MySQL / RocketMQ / Go SDK | Go `1.27.2` / MySQL `8.4.8` / Apache RocketMQ `5.5.0` / `rocketmq-client-go/v2 v2.1.2` |
| 应用镜像 | `rc-notification:0.1.0`, `sha256:b15d4454ab0c1d8e5fd3476a21be238071ec1e8475e86d2d1bb50c35b8b2c432` |
| 测试镜像 | `rc-notification-test:0.1.0`, `sha256:2793748d12a55109ced879c6f9b6d01ebba14446ca2d5f9be3eefef334b15e2c` |
| 镜像来源 | Go、MySQL、RocketMQ 基础镜像均使用 Dockerfile/Compose 锁定的 digest；版本和 digest 可在 `verify.log` 与配置中核对 |

复现命令：

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

脚本实际执行 Docker Engine/Compose 检查、`docker compose config --quiet`、应用与 test-runner 镜像构建、隔离 Compose 栈健康检查、Broker 中断恢复和 SIGTERM 场景，再执行 Docker 内的完整检查。test-runner 阶段时间为 `2026-10-14T06:42:20Z` 至 `2026-10-14T06:44:06Z`。

此前启动演示环境的 `scripts/dev-up.ps1` 已验证 `/healthz` 和 `/readyz`；现有 `rcnotif-dev` 演示环境没有被本次验收清理或重启。

| 阶段 | 命令/检查 | 退出码 |
|---|---|---:|
| Docker Engine、Compose 和配置检查 | `docker info`、`docker version`、`docker compose version`、`docker compose config --quiet` | 0 |
| 应用和 test-runner 镜像构建 | `docker compose build app test-runner` | 0 |
| 隔离依赖启动和健康检查 | MySQL、NameServer、Broker、mock-target、migration、Topic/Group init、应用 | 0 |
| gofmt 差异检查 | test-runner | 0 |
| `go vet ./...` | test-runner | 0 |
| `go test -count=1 ./...` | test-runner | 0 |
| `go test -race -count=1 ./...` | test-runner（含 CGO 工具链） | 0 |
| integration build-tag 编译检查 | test-runner | 0 |
| 真实 MySQL/RocketMQ API E2E | test-runner | 0 |
| 宿主机暂停并恢复 Broker | Docker Compose stop/start + 集成检查 | 0 |
| HTTP 在途时 SIGTERM 并重启 | Docker Compose stop/start + 集成检查 | 0 |
| 日志敏感内容 / task-trace 配对扫描 | 验收脚本 | 0 / 0 |
| 隔离 Compose 资源清理 | 仅本次 project、网络、容器和命名卷 | 0 |
| 完整验收脚本 | `scripts/verify-docker.ps1` | **0** |

## 场景证据

精简证据目录 [`docs/evidence/20261014T063922Z-6c5cb01c/`](evidence/20261014T063922Z-6c5cb01c/) 保存环境/镜像版本、阶段输出、真实集成 JSON、限量日志、容器状态及脚本摘要。完整本地日志位于 `reports/20261014T063922Z-6c5cb01c/`，并由 `.gitignore` 排除。

| 要求场景 | 实际验证结果 | 证据 |
|---|---|---|
| 入口事务与幂等 | 1 个 notification、1 条 Outbox；同内容重复提交复用任务，重复消费未多发 HTTP | `integration.json`: `acceptance_transaction_and_idempotency` |
| 并发幂等和键大小写 | 并发请求只生成 1 个任务/Outbox；大小写不同的键可区分 | `integration.json`: `case_sensitive_and_parallel_idempotency` |
| HTTP 503 后成功 | 两次业务尝试、两次 mock 请求后 DELIVERED | `integration.json`: `http_503_then_success` |
| 连续 503 到上限 | 恰好 3 次业务尝试和 3 次 mock 请求；保存 FAILED，重复投递观察窗口内仍为 3 次 | `integration.json`: `three_503_terminal_duplicate_delivery` |
| 重试到期时间跨消费取消保存 | 首次处理持久化 `next_attempt_at` 后取消；立即再次处理，在到期前 mock 仍只有 1 次请求，之后第二次尝试成功 | `integration.json`: `persisted_retry_due_survives_context_cancellation` |
| 快速失败与人工恢复 | 开关开启时 HTTP 为 0；关闭后新任务发送，旧 FAST_FAILED 任务保持终态 | `integration.json`: `fast_fail_terminal_manual_recovery` |
| 外部成功后的结果写库故障 | 两次暂时写入失败后第 3 次保存成功；连续三次失败则 MQ 原生重投并标记 UNCERTAIN，HTTP 仍仅 1 次 | `integration.json`: `external_success_result_write_retried_without_repeating_http`, `external_success_result_write_exhaustion_no_http_repeat` |
| 管理员处置数据库错误和审计身份 | 受控 MySQL 错误映射为 HTTP 500；恢复后返回 200，数据库审计身份为配置的 `integration-test` | `integration.json`: `resolution_database_failure_maps_to_500_then_recovers` |
| Outbox 标记失败补偿 | Broker 已接收但 MySQL PUBLISHED 更新失败两次；重试间隔实测 1110ms，最终 PUBLISHED，目标 HTTP 仍仅调用 1 次 | `integration.json`: `outbox_send_mark_failure_duplicate_safe`。该场景通过 MySQL trigger 注入本地标记失败，不代表网络确认包丢失注入 |
| 原始请求体、UTC 和查询索引 | XML 与表单透传；runner 时区 `+00:00`；Outbox EXPLAIN 选用 `idx_outbox_pending_due` range | `integration.json`: `raw_body_0`, `raw_body_1`, `outbox_explain_utc` |
| 数据库执行异常及 RocketMQ 原生重试 | 真实 MySQL 故障触发重投，观察到 `mq_reconsume=1`，HTTP 请求没有重复 | `integration.json`: `database_exception_native_redelivery` |
| 原生重试耗尽进入 DLQ | 固定 Broker/SDK 实测 `reconsume_times=3` 后进入 `%DLQ%notification_delivery_workers` | `integration.json`: `rocketmq_native_retries_reach_dlq` |
| 并发日志与敏感内容 | 8 组 task/trace 配对；在途数不超过配置 4；敏感内容扫描通过 | `log-context.tsv`, `summary.txt` |
| Broker 中断及 Outbox 恢复 | Broker 停止期间待发责任留在 MySQL，Broker 恢复后可发布和消费 | `outage.json` |
| SIGTERM 及处理中断 | HTTP 在途时停止应用，消息没有提前确认；应用恢复后任务转 UNCERTAIN，未重复 HTTP | `sigterm.json` |

## WSL 入口状态

`/mnt/d/rc_notification` 已确认是 D 盘真实可写挂载，`bash -n scripts/verify-docker.sh` 退出码为 0。WSL 内 `docker info` 退出码为 1，错误为 Docker 命令未映射到该 WSL 发行版。没有更改全局 WSL/Docker 配置；Windows PowerShell Docker 验收完整通过，WSL 内完整验收为 BLOCKED。

## 限制与边界

这是单应用、单 MySQL、单 RocketMQ Broker 的开发验收拓扑，不是 HA 或性能证明。业务 HTTP 尝试最多三次，但 Go `net/http` 对部分幂等请求可能内部重试，因此不据此断言底层网络发送或供应商执行次数绝不超过三次。`UNCERTAIN`、`FAST_FAILED`、过期和 HTTP 终止失败均是可查询结果，不代表已送达。DLQ 由负责人使用 README 中的 `mqadmin` 命令查询，再通过管理员 resolve API 留下结论；没有自动 DLQ 工单、重放、机构级分布式限流、自动熔断或高可用。

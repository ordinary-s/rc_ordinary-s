# Docker 验收记录

## 最新完整验收

**结果：PASS。** 最新代码（含故障注入集成用例）在 2026-10-13 UTC 的 Windows PowerShell 验收中通过。验收入口退出码为 `0`；单测、race、真实 MySQL/RocketMQ E2E、Broker 停启、SIGTERM、脱敏扫描和本次资源清理均为 `0`。

| 项目 | 实际值 |
|---|---|
| 固定项目目录 | `D:\rc_notification` |
| WSL 映射 | `/mnt/d/rc_notification`；`findmnt` 证明 `/mnt/d` 来源为 `D:\`、挂载可写 |
| 运行 ID / Compose project | `20261013T214902Z-747fc6e8` / `rcnotif-verify-20261013t214902z-747fc6e8` |
| 验收开始 / 结束 | `2026-10-13T21:49:02Z` / `2026-10-13T21:53:44Z` |
| Docker Desktop / Engine / Compose | Docker Desktop `4.69.0` / Engine `29.4.0` Linux `amd64` / Compose `5.1.1` |
| Go / MySQL / RocketMQ / Go SDK | Go `1.27.2` / MySQL `8.4.8` / Apache RocketMQ `5.5.0` / `rocketmq-client-go/v2 v2.1.2` |
| 应用镜像 | `rc-notification:0.1.0`, `sha256:f62dac0fe3a176739b51bb0ca7e2ce6f2ceb65a948c565f289c81ddfa4f46488` |
| 测试镜像 | `rc-notification-test:0.1.0`, `sha256:b24d5810bca925365593c60f50bce3ce5e9d4fe30d0a23912afe7e1083709769` |
| 镜像来源 | Go `1.27.2-bookworm`、Debian `13.1-slim`、MySQL `8.4.8`、RocketMQ `5.5.0` 均使用 Dockerfile/Compose 中锁定的 digest |

复现命令：

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

脚本实际执行 Docker Engine/Compose 检查、`docker compose config --quiet`、应用和 test-runner 构建、隔离 Compose 栈启动及真实健康检查，之后运行 Broker 中断恢复和 SIGTERM 故障场景，并执行：

此前还从 Windows 执行过 `scripts/dev-up.ps1` 演示入口；`/healthz` 返回 live，`/readyz` 返回 ready。演示 Compose project `rcnotif-dev` 保持运行，MySQL 和 Broker 管理端口没有暴露到宿主机。

```powershell
docker compose -p rcnotif-verify-20261013t214902z-747fc6e8 -f D:\rc_notification\compose.yaml -f D:\rc_notification\compose.verify.yaml run --rm test-runner
```

测试容器各阶段开始/结束为 `2026-10-13T21:51:58Z` / `2026-10-13T21:53:36Z`：

| 阶段 | 退出码 |
|---|---:|
| gofmt 差异检查 | 0 |
| `go vet ./...` | 0 |
| `go test -count=1 ./...` | 0 |
| `go test -race -count=1 ./...` | 0 |
| integration build-tag 编译检查 | 0 |
| 真实 MySQL/RocketMQ API E2E | 0 |
| 宿主机 Broker 暂停/恢复场景 | 0 |
| 宿主机 SIGTERM 场景 | 0 |
| 日志敏感字符串 / task-trace 配对扫描 | 0 / 0 |
| 本次隔离 Compose project 清理 | 0 |

## 场景证据

证据目录 [`docs/evidence/20261013T214902Z-747fc6e8/`](evidence/20261013T214902Z-747fc6e8/) 保存精简摘要、阶段输出、真实集成结果、限量脱敏日志和容器状态；完整本地日志位于 `reports/20261013T214902Z-747fc6e8/`，并由 `.gitignore` 排除。

| 要求场景 | 真实验证结果 | 证据 |
|---|---|---|
| API 接收、notification + outbox 同事务、重复入口幂等和同键异内容 409 | 1 个任务、1 条 outbox；成功一次；重复请求和重复 MQ 消息未多发 HTTP；冲突为 409 | `integration.json`：`acceptance_transaction_and_idempotency` |
| 并发幂等和大小写敏感键 | 8 个并发同键请求收敛为 1 个任务/Outbox；区分大小写的键各自建立任务 | `integration.json`：`case_sensitive_and_parallel_idempotency` |
| MQ 已收到但本地 Outbox 发布状态更新失败 | MySQL trigger 使本地标记失败 2 次；Publisher 再发相同事件，恢复后变为 PUBLISHED，mock 仍仅收到 1 次 HTTP | `integration.json`：`outbox_send_mark_failure_duplicate_safe`。这是本地确认记录失败窗口的故障注入，没有注入 Broker TCP 响应包被丢弃 |
| 503 后成功 | 两次业务尝试、两次 mock 请求后 DELIVERED | `integration.json`：`http_503_then_success` |
| 连续 503 到业务上限 | 恰好 3 次业务尝试和 3 次 mock 请求；保存终止 FAILED；在重复观察窗口内再投递不发 HTTP | `integration.json`：`three_503_terminal_duplicate_delivery` |
| 机构快速失败及恢复 | 开启时无外部请求并保存 FAST_FAILED；关闭后新任务发送，旧任务仍未调用 | `integration.json`：`fast_fail_terminal_manual_recovery` |
| 参数/Body 透传与存储时间/查询计划 | XML、表单内容类型通过；MySQL UTC session 为 `+00:00`，Outbox 到期扫描使用 `idx_outbox_pending_due` range | `integration.json`：`raw_body_0`、`raw_body_1`、`outbox_explain_utc` |
| 数据库执行异常 | 真实 MySQL trigger 令消费处理失败，RocketMQ 原生重投可观察到 `mq_reconsume=1`；业务请求仅 1 次 | `integration.json`：`database_exception_native_redelivery` |
| 外部成功但结果写库短暂失败 | 前两次保存失败，第 3 次成功；总计 3 次结果写入、1 次 HTTP、DELIVERED | `integration.json`：`external_success_result_write_retried_without_repeating_http` |
| 外部成功但结果写库持续失败 | 结果保存 3 次耗尽后原生重投；尝试记录为 UNKNOWN、任务 UNCERTAIN，重投没有再次调用 HTTP | `integration.json`：`external_success_result_write_exhaustion_no_http_repeat` |
| Broker 中断、Outbox 保留、恢复及任务过期 | Broker 停止期间任务保持待发；恢复后新鲜任务发布并消费；过期任务 HTTP 计数为 0 | `outage.json`：`broker_outage_outbox_recovery_and_task_expiry`；宿主机停启退出码 0 |
| 并发日志与敏感内容 | 8 个 task/trace 对逐条核对；消费在途数最高 4；敏感 canary 扫描通过 | `log-context.tsv`、`summary.txt` |
| SIGTERM 与处理中断 | 容器收到 SIGTERM 并正常退出；重启后任务标记 UNCERTAIN/attempt UNKNOWN，HTTP 仅 1 次 | `sigterm.json`：`sigterm_interrupts_inflight_http_and_redelivery_does_not_repeat` |
| RocketMQ 原生重试上限与 DLQ | 固定 Broker/SDK 实测消息 `reconsume_times=3` 后进入 `%DLQ%notification_delivery_workers` | `integration.json`：`rocketmq_native_retries_reach_dlq` |

## WSL 入口状态

`/mnt/d/rc_notification` 已确认是 D 盘的真实可写挂载，`bash -n scripts/verify-docker.sh` 退出码为 0。WSL 内的 `docker info` 退出码为 1，原始错误为：`The command 'docker' could not be found in this WSL 2 distro. ... activate the WSL integration in Docker Desktop settings.` 因用户约束不改全局 Docker/WSL 配置，本次没有从 WSL 运行完整 Bash 验收；Windows PowerShell 完整 Docker 验收已 PASS。

## 限制与边界

这是固定单应用、单 MySQL、单 RocketMQ Broker 的开发验收拓扑，不是 HA 或性能证明。`UNCERTAIN`、`FAST_FAILED`、过期和 HTTP 终止失败是可持久查询的结果，不代表通知已送达；下游幂等能力仍取决于对方真实实现。DLQ 需负责人使用 README 中的 `mqadmin` 命令查询，并通过管理员 resolve API 记录人工结论。当前没有自动 DLQ 工单、重放、机构级分布式限流、自动熔断或高可用。

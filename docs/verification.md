# Docker 验收与证据

## 已归档的完整验收

**运行 `20261014T063922Z-6c5cb01c`：PASS，完整脚本退出码 0。** 该验收由用户指定的实现任务完成，本次文档审查重新读取了该任务的交付结论和仓库中的原始摘要、阶段输出及场景证据。以下结果均对应这一次已完成运行，不表示文档修订本身产生了新的 Docker PASS。

相关代码与证据随 [`9ed109dce1a7e3373e052d06c7d343c1ba63c652`](https://github.com/ordinary-s/rc_ordinary-s/commit/9ed109dce1a7e3373e052d06c7d343c1ba63c652) 提交；原报告记录运行 ID、镜像 ID 和时间，未内置 Git SHA，不反向改写其原始字段。本次文档审查以该提交为源码基线，没有修改业务代码、SQL、配置、测试或验收脚本。

| 项目 | 已归档实际值 |
|---|---|
| 固定项目目录 / 执行环境 | `D:\rc_notification` / Windows PowerShell、Docker Linux 容器 |
| 运行 ID / Compose project | `20261014T063922Z-6c5cb01c` / `rcnotif-verify-20261014t063922z-6c5cb01c` |
| UTC 开始 / 结束 | `2026-10-14T06:39:22Z` / `2026-10-14T06:44:17Z` |
| test-runner UTC 开始 / 结束 | `2026-10-14T06:42:20Z` / `2026-10-14T06:44:06Z` |
| Docker Desktop / Engine / Compose | `4.69.0` / `29.4.0` Linux amd64 / `5.1.1` |
| Go / MySQL / RocketMQ / Go SDK | `1.27.2` / `8.4.8` / `5.5.0` / `rocketmq-client-go/v2 v2.1.2` |
| 应用镜像 | `sha256:b15d4454ab0c1d8e5fd3476a21be238071ec1e8475e86d2d1bb50c35b8b2c432` |
| 测试镜像 | `sha256:2793748d12a55109ced879c6f9b6d01ebba14446ca2d5f9be3eefef334b15e2c` |
| 基础镜像 | Go、MySQL、RocketMQ 均使用 Dockerfile/Compose 锁定的 digest |

精简证据见 [`docs/evidence/20261014T063922Z-6c5cb01c/`](evidence/20261014T063922Z-6c5cb01c/)；完整原始报告保存在被 Git 忽略的 `reports/20261014T063922Z-6c5cb01c/`。文档维护保留已有证据的原始运行时间和范围。

## 验收入口与阶段结果

先用 `docker info` 确认引擎就绪。该 Windows 机器需要启动 Docker Desktop 时，使用 README 中的固定 launcher；引擎已经就绪则直接使用。完整入口为：

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

脚本检查 Docker Engine/Compose 和 Compose 配置，构建应用及 test-runner，启动真实 MySQL、NameServer、Broker、mock target、migration 与应用，执行故障场景和容器内完整检查，最后采集日志并清理本次隔离资源。

| 阶段 / 实际命令 | 归档退出码 / 结果 | 证据 |
|---|---|---|
| 镜像构建、依赖启动及健康检查 | 成功，`stack_health=PASS` | `summary.txt`、`compose-ps.txt`，完整构建日志见本地 reports |
| Docker 内 `gofmt -d` 差异检查 | 0，无差异 | `checks.log`、`checks-summary.txt` |
| `go vet ./...` | 0 | 同上 |
| `go test -count=1 ./...` | 0 | 同上 |
| `go test -race -count=1 ./...` | 0 | 同上；普通包 race 不包含需要 integration tag 的 E2E |
| `go test -run '^$' -tags=integration ./tests/integration` | 0 | 同上；只编译集成测试 |
| `go test -tags=integration -run '^TestEndToEndWithMySQLAndRocketMQ$' -count=1 -v ./tests/integration` | 0；15 个 Go 子测试通过 | 同上及 `integration.json` |
| Broker 停止/恢复及 Outbox 场景 | 0 | `outage.json`、`summary.txt` |
| HTTP 在途时 SIGTERM、重启及恢复 | 0 | `sigterm.json`、`sigterm-app.log`、`summary.txt` |
| 日志敏感内容 / task-trace 配对扫描 | 0 / 0 | `summary.txt`、`log-context.tsv` |
| 隔离 Compose project 及命名卷清理 | 0 | `summary.txt` |
| 完整验收脚本 | **0，PASS** | `summary.txt` |

该记录证明 Windows 入口的上述链路；WSL 入口未包含在这次完整验收范围。WSL 使用前仍需先确认 `/mnt/d/rc_notification` 是 D 盘实际挂载，再检查该发行版的 Docker 可用性，不能由 Windows 结果推导 WSL 已通过。

## 真实场景与保证范围

下表来自该 run 的 [integration.json](evidence/20261014T063922Z-6c5cb01c/integration.json)、[checks.log](evidence/20261014T063922Z-6c5cb01c/checks.log) 及独立故障场景报告。JSON 的场景条目数与 Go 子测试数不同：XML/表单分为两条记录，结果写库也有独立场景，计数时不能混用。

| 要求场景 | 归档实际结果与证据条目 |
|---|---|
| 入口事务与幂等 | 1 个 notification、1 条 Outbox；同内容重复提交复用任务，重复消费未多发 HTTP。`acceptance_transaction_and_idempotency`。 |
| 并发幂等与键大小写 | 并发请求只生成 1 个任务/Outbox；大小写不同的键可区分。`case_sensitive_and_parallel_idempotency`。 |
| HTTP 503 后成功 | 两次业务尝试、两次 mock 请求后 DELIVERED。`http_503_then_success`。 |
| 连续 503 到上限 | 3 次业务尝试和 3 次 mock 请求；保存 FAILED，重复投递观察窗口内仍为 3 次。`three_503_terminal_duplicate_delivery`。 |
| 持久化重试到期时间 | 首次结果与 `next_attempt_at` 保存后取消，立即再次处理；到期前 mock 仍为 1 次，之后第 2 次尝试成功。`persisted_retry_due_survives_context_cancellation`。此夹具使用真实 MySQL/mock，直接调用本地 Engine，不是该场景通过真实 MQ 发起第二次回调。 |
| 快速失败与管理员关闭 | 开启时 HTTP 为 0；关闭后新任务发送，旧 FAST_FAILED 任务不复活。`fast_fail_terminal_manual_recovery`。 |
| 外部成功后结果保存暂时故障 | 第 3 次保存成功，HTTP 仍为 1 次。`external_success_result_write_retried_without_repeating_http`。 |
| 外部成功后结果保存重试耗尽 | MQ 原生重投后 UNKNOWN/UNCERTAIN，HTTP 在观察窗口内仍为 1 次。`external_success_result_write_exhaustion_no_http_repeat`。 |
| 管理员处置数据库错误及身份 | 受控数据库错误返回 500，恢复后 200；审计身份为配置的 `integration-test`。`resolution_database_failure_maps_to_500_then_recovers`。 |
| Outbox 发送成功、本地标记故障 | MySQL trigger 注入 2 次 PUBLISHED 更新错误；实测标记重试间隔 1110ms，最终 PUBLISHED，HTTP 仍为 1 次。`outbox_send_mark_failure_duplicate_safe`。没有注入网络层确认包丢失。 |
| XML/表单、UTC 与索引 | 原始解码正文保持；runner 时区 `+00:00`；EXPLAIN 使用 `idx_outbox_pending_due`，type 为 range。`raw_body_0`、`raw_body_1`、`outbox_explain_utc`。 |
| 数据库执行异常与原生重投 | 真实 MySQL 故障触发重投，观察到 `mq_reconsume=1`，HTTP 未重复。`database_exception_native_redelivery`。 |
| 原生重试耗尽进入 DLQ | 锁定 Broker/SDK 实测 `reconsume_times=3`，进入 `%DLQ%notification_delivery_workers`。`rocketmq_native_retries_reach_dlq`。 |
| 并发日志与资源上限 | 8 组 task/trace 配对，在途数不超过配置 4；日志扫描通过。`concurrent_tasks_keep_trace_and_task_context_paired`、`log-context.tsv`、`summary.txt`。 |
| Broker 中断及 Outbox 恢复 | Broker 停止时发布责任保留在 MySQL；恢复后发布、消费，过期任务零 HTTP。见 [outage.json](evidence/20261014T063922Z-6c5cb01c/outage.json)。 |
| SIGTERM 及在途处理 | 在途停止应用时未提前确认；恢复后 UNCERTAIN，没有重复 HTTP。见 [sigterm.json](evidence/20261014T063922Z-6c5cb01c/sigterm.json)。 |

## 文档核对与已知限制

本次文档审查对 11 份 Markdown 检查相对路径/章节锚点、代码围栏、表格列数、旧业务规则与凭证模式，并执行 `git diff --check`。对历史、否定和未来语境另行人工复核。检查结果与源码范围记录见 [documentation-checks.json](evidence/docs-review-20261014/documentation-checks.json) 和 [source-context.json](evidence/docs-review-20261014/source-context.json)。源码哈希用于证明本次未改代码，不回填旧 Docker 运行未记录的哈希证明。

另只读执行既有 dev Broker 的 `mqadmin help printMsg`，退出 0，核实 `-c` 表示字符集、`-d` 控制正文打印；README 示例修正为 `-c UTF-8 -d false`。本次未用该手工命令导出 DLQ，help 检查不等于人工处置成功。DLQ 行为有上表的集成证据，但 PENDING/PROCESSING 或缺失任务不能通过现有 resolve 结案。

日志扫描覆盖截取的应用日志和对应匹配规则；通过不证明所有启动、迁移、驱动或 SDK 错误路径均已脱敏。README 中保留的 token 是已注明用途的演示值。

验收采用单应用、单 MySQL、单 RocketMQ Broker 的开发拓扑，不是性能、HA 或外部恰好一次证明。三次是业务尝试上限，不能据此声称所有底层 Transport 网络发送绝不超过三次。快速失败切换竞态、发送时目标复核、migration 中断恢复、锁等待等能力边界见[实现审查](implementation-review.md)。源码已实现、特定场景通过、生产能力承诺应分别判断。

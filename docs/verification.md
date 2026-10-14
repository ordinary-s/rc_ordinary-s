# 测试与验收报告

## 验收结果

2026-10-14 的归档 Docker 验收通过，完整脚本退出码为 **0**。验收使用真实 MySQL、RocketMQ NameServer/Broker、应用和可计数 mock target；15 个 E2E 子测试通过，另有 Broker 中断恢复和 SIGTERM 两个独立场景。

该结果对应运行 `20261014T063922Z-6c5cb01c`。原始[阶段摘要](evidence/20261014T063922Z-6c5cb01c/checks-summary.txt)、[完整摘要](evidence/20261014T063922Z-6c5cb01c/summary.txt)和[场景结果](evidence/20261014T063922Z-6c5cb01c/integration.json)随代码提交 [9ed109d](https://github.com/ordinary-s/rc_ordinary-s/commit/9ed109dce1a7e3373e052d06c7d343c1ba63c652)归档。报告保留了运行时间和镜像 ID，未内置 Git SHA。

## 验证了哪些行为

测试重点是故障发生后任务状态是否准确、是否产生额外 HTTP 调用，以及消息能否按预期恢复。下表汇总上述归档运行的结果，详细输出见 [checks.log](evidence/20261014T063922Z-6c5cb01c/checks.log) 与[场景 JSON](evidence/20261014T063922Z-6c5cb01c/integration.json)。

| 场景 | 观察结果 |
|---|---|
| 入口事务与重复提交 | 同内容重复提交复用原任务；数据库中只有 1 个 notification 和 1 条 Outbox；重复消费没有增加 HTTP 调用。 |
| 并发提交与键大小写 | 并发使用同一幂等键只生成一个任务；大小写不同的键能够区分。 |
| HTTP 503 后恢复 | 第一次收到 503，第二次成功；业务尝试和 mock 请求均为 2 次。 |
| 连续三次 HTTP 503 | 3 次尝试后保存 FAILED；再次投递消息，在观察窗口内 HTTP 仍为 3 次。 |
| 持久化退避时间 | 首次结果保存后中断处理，再次执行时到期前没有提前请求，到期后的第 2 次尝试成功。 |
| 机构快速失败 | 开关开启时没有 HTTP 请求；关闭后新任务可投递，旧 FAST_FAILED 任务保持终态。 |
| HTTP 成功、结果保存暂时故障 | 第 3 次数据库保存成功，HTTP 仍为 1 次。 |
| HTTP 成功、结果保存持续故障 | MQ 原生重投后记录 UNKNOWN/UNCERTAIN，在观察窗口内没有重复 HTTP。 |
| 管理员处置与审计身份 | 数据库故障返回 500，恢复后返回 200；保存的操作人为配置的 `integration-test`。 |
| Broker 接收后、本地发布标记故障 | 注入 2 次 PUBLISHED 更新错误后最终保存成功；发布标记重试间隔为 1110ms，重复消息未增加 HTTP。 |
| 请求内容与数据库设置 | XML、表单解码后的正文保持原值；数据库会话时区为 `+00:00`；Outbox 查询使用 `idx_outbox_pending_due` range 扫描。 |
| 数据库执行异常 | 真实数据库故障触发 RocketMQ 原生重投，观察到 `mq_reconsume=1`，HTTP 没有重复。 |
| 原生重试耗尽 | 消息进入 `%DLQ%notification_delivery_workers`，实测 `reconsume_times=3`。 |
| 并发与日志关联 | 8 组任务的 task/trace 对应正确，观察到的在途数不超过配置 4；应用日志扫描通过。 |
| Broker 中断与恢复 | 中断期间发布任务保留在 MySQL；恢复后完成发布与消费，过期任务没有发起 HTTP。见 [outage.json](evidence/20261014T063922Z-6c5cb01c/outage.json)。 |
| HTTP 在途时收到 SIGTERM | 应用退出前未提前确认消息；重启后转为 UNCERTAIN，HTTP 仍只调用一次。见 [sigterm.json](evidence/20261014T063922Z-6c5cb01c/sigterm.json)。 |

部分场景记录会拆成多条 JSON，例如 XML 和表单分别记录，因此 JSON 条目数与 Go 子测试数不同。持久退避场景使用真实 MySQL/mock 并直接调用本地 Engine；它验证的是取消后的恢复逻辑，真实 MQ 重投由另外的数据库异常和结果保存故障场景覆盖。Outbox 场景通过 MySQL trigger 注入本地标记故障，未模拟网络确认包丢失。

## 如何复现

准备 Docker Engine 和 Compose，运行 `docker info` 确认引擎就绪，然后在项目根目录执行 Windows 入口：

```powershell
.\scripts\verify-docker.ps1
```

仓库还提供 Linux/WSL 的 Bash 入口：

```bash
bash scripts/verify-docker.sh
```

WSL 使用前需确认项目所在盘已正确挂载，且该发行版可以访问 Docker。下面的已归档结果来自 Windows PowerShell，未包含 WSL 完整运行。

脚本构建固定版本的应用与 test-runner 镜像，创建隔离的 Compose project，初始化 MySQL、Topic 和消费组，再执行测试与故障注入。运行报告写入 `reports/<run_id>/`，精简证据写入 `docs/evidence/<run_id>/`。正常流程末尾清理该次创建的容器、网络和命名卷；异常退出后需按运行 ID 核对资源和报告中的清理结果。

## 运行环境与报告

| 项目 | 实际值 |
|---|---|
| 执行平台 | Windows PowerShell、Docker Linux amd64 容器 |
| 运行 ID | `20261014T063922Z-6c5cb01c` |
| UTC 开始 / 结束 | `2026-10-14T06:39:22Z` / `2026-10-14T06:44:17Z` |
| Docker Desktop / Engine / Compose | `4.69.0` / `29.4.0` / `5.1.1` |
| Go / MySQL / RocketMQ / Go SDK | `1.27.2` / `8.4.8` / `5.5.0` / `rocketmq-client-go/v2 v2.1.2` |
| 工具链与基础镜像 | Dockerfile/Compose 锁定 digest，Go 检查在 test-runner 内执行 |
| 证据目录 | [已归档报告](evidence/20261014T063922Z-6c5cb01c/)；镜像 ID 和清理结果见 `summary.txt` |

## 验证范围的限制

上述测试采用单应用、单 MySQL、单 Broker 的开发拓扑，未覆盖生产容量、跨故障域高可用或多实例协调。普通包的 race 检查没有包含需要 integration tag 的 E2E。

日志扫描覆盖截取的应用日志，启动、迁移、驱动和 SDK 的全部错误路径仍需补充检查。三次调用计数验证了明确返回 503 的业务场景，未覆盖 Transport 在连接故障下的所有内部重试行为。DLQ 测试验证消息进入死信队列，现有人工接口对中间态或缺失任务的处理仍有限制。

更详细的实现边界与改进建议见[已知限制与改进方向](implementation-review.md)。

## 附录：阶段命令与退出码

以下均为运行 `20261014T063922Z-6c5cb01c` 的实际结果。

| 阶段 / 命令 | 退出码 / 结果 | 证据 |
|---|---|---|
| 镜像构建、依赖启动及健康检查 | `stack_health=PASS` | `summary.txt`、`compose-ps.txt` |
| Docker 内 `gofmt -d` 差异检查 | 0，无差异 | `checks.log`、`checks-summary.txt` |
| `go vet ./...` | 0 | 同上 |
| `go test -count=1 ./...` | 0 | 同上 |
| `go test -race -count=1 ./...` | 0 | 同上 |
| `go test -run '^$' -tags=integration ./tests/integration` | 0；集成测试编译 | 同上 |
| `go test -tags=integration -run '^TestEndToEndWithMySQLAndRocketMQ$' -count=1 -v ./tests/integration` | 0；15 个子测试通过 | 同上及 `integration.json` |
| Broker 停止/恢复 | 0 | `outage.json`、`summary.txt` |
| HTTP 在途时 SIGTERM、重启及恢复 | 0 | `sigterm.json`、`sigterm-app.log`、`summary.txt` |
| 日志敏感内容 / task-trace 配对扫描 | 0 / 0 | `summary.txt`、`log-context.tsv` |
| 隔离 Compose project 及命名卷清理 | 0 | `summary.txt` |
| 完整验收脚本 | **0，PASS** | `summary.txt` |

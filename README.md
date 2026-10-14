# HTTP 通知服务 MVP

这是一个基于 Go、MySQL/InnoDB 和 Apache RocketMQ 的 HTTP 通知服务。HTTP API 将通知任务与 Outbox 事件写入同一个 MySQL 事务；Publisher 将小型事件投递到 RocketMQ；同一 Go 进程中的消费者按任务执行外部 HTTP。当前服务按单实例设计。

完整设计、技术取舍和实现边界见下方[文档导航](#文档导航)。**已归档 Docker 验收 `20261014T063922Z-6c5cb01c` 为 PASS**；运行日期、命令、真实场景与保证范围见[验收记录](docs/verification.md)。该结果适用于记录中的运行环境与场景。

## 边界与语义

- RocketMQ 内部消息采用至少一次交付，重复消费由 task 状态和尝试记录去重。
- 每个逻辑通知最多三次 HTTP 业务尝试，包含首次。可重试的 HTTP 429/5xx 采用两段退避；明确的非重试 4xx 结束为 FAILED。
- 重试到期时间与 HTTP 尝试结果在同一 MySQL 事务持久化；MQ 重投或进程重启会遵守该时间。超出任务时效或本次有界处理预算的 `Retry-After` 会以 FAILED 结束，不提前发送，也不新建业务重试队列。
- 超时或断连属于远端结果不明时，保守地写入 UNCERTAIN 并结束消费，避免盲目重复有副作用的请求。外部成功但结果无法保存时，重投恢复会把在途尝试标为待核对，不再发一次 HTTP。
- HTTP 成功而结果暂时无法落库时，同一次消费内最多进行 3 次结果保存（初次 + 两次短等待后重试），不重新调用 HTTP；仍失败才返回 MQ 原生重投。重投发现 STARTED 会写成 UNCERTAIN。
- FAILED、FAST_FAILED、EXPIRED、UNCERTAIN 在结果可靠保存后均是已完成的消费结果。快速失败由管理员设置，关闭后只影响后续任务，旧终态不会复活。
- 因为允许有限重试后终止、快速失败和不明结果，本服务不承诺通知最终必达，也不承诺外部恰好一次。下游需按稳定 task ID 的 `Idempotency-Key` 实现去重，才能降低远端成功/本地未知时的副作用。
- “最多三次”指应用层业务尝试；Go `net/http` Transport 对某些幂等请求可能进行内部网络重试，因此不声称底层网络发送绝不超过三次。

## 架构

```text
调用方 --HTTP/鉴权/幂等--> API --单个 MySQL 事务--> notification + outbox
                                                    |
                                       Outbox 定时扫描并确认发送
                                                    v
                                            RocketMQ Topic
                                                    |
                     结果可靠保存 <--- 单条消费 + 有界并发 ---+--- HTTP(S) 目标
```

细节见[设计说明](docs/design.md)。系统边界、可靠性和扩容分析见[题目逐项答复](docs/assignment-answers.md)，技术选型与代价见[设计决策](docs/decisions.md)。

## 本机启动

要求 Docker Desktop/Linux 容器支持和 Compose 插件。无需宿主机预装 Go、MySQL 或 RocketMQ。

```powershell
Set-Location 'D:\rc_notification'
if (-not (Test-Path -LiteralPath '.env')) { Copy-Item .env.example .env }
# 按本机使用需要改掉仅供演示的示例凭证
.\scripts\dev-up.ps1
```

仅首次启动且尚无 `.env` 时复制示例，已有本地配置应保留。先用 `docker info` 检查引擎；本 Windows 机器若未启动，运行 `& 'C:\Users\admin\.codex\scripts\Start-DockerDesktop.ps1'`，等待引擎就绪后再运行上述脚本。已运行的引擎无需重启。

演示应用绑定到 `127.0.0.1:8080`，MySQL 和 MQ 管理端口不映射到宿主机。配置由 `.env` 加 Compose 环境变量提供；真实凭证不要提交。

管理员 token 对应单个 `ADMIN_ACTOR` 配置身份，MVP 不接受调用方通过自报 Header 选择审计人。默认消费处理预算为 90 秒，RocketMQ 消费超时为 2 分钟；启动配置会校验其覆盖三次 HTTP 超时、两段退避和结果落库余量。可用 `DELIVERY_BUDGET`、`MQ_CONSUME_TIMEOUT` 调整，但两者须保持预算小于消费超时。

快速检查：

```powershell
docker compose -p rcnotif-dev ps
Invoke-RestMethod http://127.0.0.1:8080/healthz
Invoke-RestMethod http://127.0.0.1:8080/readyz
```

## API

客户端身份来自 Bearer 凭证映射的 `CLIENT_ID`，不接受请求体自报身份。示例仅适用于 `.env.example` 演示凭证。

```powershell
$headers = @{
  Authorization = 'Bearer demo-client-token'
  'Idempotency-Key' = 'order-2026-001'
}
$body = @{
  destination_id = 'mock-target'
  method = 'POST'
  headers = @{ 'Content-Type' = 'application/json' }
  body = '{"order_id":"2026-001"}'
} | ConvertTo-Json -Depth 5
$task = Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/api/v1/notifications -Headers $headers -ContentType 'application/json' -Body $body
Invoke-RestMethod -Uri "http://127.0.0.1:8080/api/v1/notifications/$($task.task_id)" -Headers @{ Authorization = 'Bearer demo-client-token' }
```

| 接口 | 鉴权 | 说明 |
|---|---|---|
| `POST /api/v1/notifications` | 客户端 Bearer + `Idempotency-Key` | 同键同内容返回原 task；同键异内容 `409`；事务提交后 `202` |
| `GET /api/v1/notifications/{id}` | 客户端 Bearer | 仅返回该客户端任务和尝试记录，不返回原请求头/正文 |
| `GET /api/v1/notifications?status=FAILED&page=1&page_size=20` | 客户端 Bearer | 调用方隔离的分页查询，最大页大小 100 |
| `PATCH /api/v1/admin/destinations/{id}/fast-fail` | 管理员 Bearer | 请求体 `{"enabled":true,"reason":"人工原因"}`；保存配置的管理员操作人、原因和时间 |
| `POST /api/v1/admin/notifications/{id}/resolve` | 管理员 Bearer | 请求体 `{"conclusion":"人工核对结论"}`；记录配置的管理员操作人和结论，不重放任务 |
| `GET /healthz` | 无 | 进程存活 |
| `GET /readyz` | 无 | MySQL、RocketMQ 客户端和配置 Broker TCP 探测；MQ 不可用时返回 503，API 在进程仍运行且 MySQL 可用时仍可持久化受理 |

演示目的地通过 migration seed 到 `http://mock-target:8090/notify`，只有配置的协议、主机、端口允许。MVP 没有目标管理平台、凭证轮换或任意 URL 转发。

## 完整 Docker 验收

Windows 一键入口：

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

WSL/Linux 等价入口（先确认 `/mnt/d` 真正映射 D 盘）：

```bash
findmnt -T /mnt/d/rc_notification
cd /mnt/d/rc_notification
bash scripts/verify-docker.sh
```

脚本检查 Docker/Compose、构建锁定版本的应用与 Go/race test-runner 镜像、启动隔离的 MySQL 8.4.8 + RocketMQ 5.5.0 + NameServer + mock target，等待真实依赖健康，运行 migration/Topic/Group 初始化、Broker 停启和 Docker 内完整测试。覆盖真实重试/DLQ、持久化 HTTP 重试时间、Outbox 标记失败、管理员处置错误、结果保存失败及 SIGTERM。指定已归档运行的 ID、退出码、源码归属及精简证据见[验收记录](docs/verification.md)，保留其原始时间和结果。

## 人工异常闭环

按 `status=FAILED` 或 `status=UNCERTAIN` 查询任务 API，检查尝试和 `result_reason`。resolve 只允许 FAILED、FAST_FAILED、EXPIRED、UNCERTAIN，保存结论但不改状态、不重发。原生 DLQ 中仍为 PENDING/PROCESSING 或根本不存在的任务无法用此接口结案，当前须另存受控处置记录，详见[实现审查 R03](docs/implementation-review.md#r03dlq-消息不一定能通过现有-resolve-接口闭环)。原生死信 topic 为 `%DLQ%notification_delivery_workers`。RocketMQ 管理工具在 Broker 镜像内可查询：

```bash
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin consumerStatus -n namesrv:9876 -g notification_delivery_workers
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin printMsg -n namesrv:9876 -t '%DLQ%notification_delivery_workers' -c UTF-8 -d false
```

在锁定版本的 Broker 镜像中，`-c` 指定字符集，`-d false` 关闭正文打印。需要检查小事件中的 task/event ID 时，在受控终端按需开启正文，留存仅保留必要标识、时间和核对结论。MVP 不自动把 DLQ 同步成数据库待办，不自动重放死信或已终态任务。

## 文档导航

| 阅读目的 | 文档 | 内容 |
| --- | --- | --- |
| 理解完整架构与故障处理 | [完整设计](docs/design.md) | 17 节覆盖边界、流程、事务、幂等、Outbox、消费、状态机、三次尝试、人工处理、安全与演进。 |
| 按作业问题阅读设计判断 | [题目逐项答复](docs/assignment-answers.md) | 第一人称回答系统边界、可靠性、AI 取舍、未来演进、中间件选择五部分。 |
| 了解选择原因与代价 | [设计决策](docs/decisions.md) | 初始建议、最终决定、修改理由、收益、代价与实现状态。 |
| 区分 AI 建议和用户决定 | [AI 使用说明](AI_USAGE.md) | 可追溯的方案、纠正、辅助实现与实际验证经历。 |
| 核对代码是否满足设计 | [实现与证据核对表](docs/implementation-review.md) | 代码位置、实际行为、目标差异、风险及建议。 |
| 判断哪些检查实际运行过 | [Docker 验收记录](docs/verification.md) | 已归档通过验收的时间、命令、退出码、真实依赖场景及能力边界。 |

首次阅读建议从本页进入完整设计，再按需要查看题目答复和决策记录。评审实现时，结合实现核对表与验收记录，分别判断代码存在、归档场景已测试和能力保证范围。机构限流、分组隔离、多实例全局配额、自动熔断和独立扩容属于后续演进，归档验收不构成这些能力的实现或性能证明。

## 已知限制

单实例、单 MySQL、单 Broker；没有全局机构 QPS 配额、自动熔断/半开、严格顺序、通用供应商响应映射、凭证刷新平台、自动 DLQ 重放或审批后台。宿主机意外断电/单 Broker 损坏不具备高可用保证。同步刷盘与命名卷只提升本机验收拓扑的持久性，不构成备份或跨故障域容灾。

快速失败按消费者观察到的配置生效，配置更新与发出请求之间仍有竞态；已有在途请求不会撤销。执行时目标复核、DDL 中断恢复、部分日志关联和锁等待预算也有已记录边界，具体分析与改进建议见[实现审查](docs/implementation-review.md)。

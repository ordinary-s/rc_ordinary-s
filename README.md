# HTTP 通知服务 MVP

这是一个可运行、可复现的 Go 通知服务作业实现：HTTP API 将通知任务与 Outbox 事件写入同一个 MySQL 事务；Publisher 将小型事件投递到真实 RocketMQ；同一 Go 进程中的消费者按任务执行外部 HTTP。当前服务按单实例设计。

## 边界与语义

- RocketMQ 内部消息采用至少一次交付，重复消费由 task 状态和尝试记录去重。
- 每个逻辑通知最多三次 HTTP 业务尝试，包含首次。可重试的 HTTP 429/5xx 采用两段退避；明确的非重试 4xx 结束为 FAILED。
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

细节见[设计说明](docs/design.md)。题目逐项回答见[assignment answers](docs/assignment-answers.md)，讨论决策见[决策表](docs/decisions.md)，Go/MySQL/RocketMQ 依据与落点见[工程规范](docs/standards.md)。

## 本机启动

要求 Docker Desktop/Linux 容器支持和 Compose 插件。无需宿主机预装 Go、MySQL 或 RocketMQ。

```powershell
Set-Location 'D:\rc_notification'
Copy-Item .env.example .env
# 按本机使用需要改掉仅供演示的示例凭证
.\scripts\dev-up.ps1
```

演示应用绑定到 `127.0.0.1:8080`，MySQL 和 MQ 管理端口不映射到宿主机。配置由 `.env` 加 Compose 环境变量提供；真实凭证不要提交。

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
| `PATCH /api/v1/admin/destinations/{id}/fast-fail` | 管理员 Bearer + `X-Admin-Actor` | 请求体 `{"enabled":true,"reason":"人工原因"}`；保存操作人、原因和时间 |
| `POST /api/v1/admin/notifications/{id}/resolve` | 管理员 Bearer + `X-Admin-Actor` | 请求体 `{"conclusion":"人工核对结论"}`；只记录结论，不重放任务 |
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

脚本检查 Docker/Compose、构建固定版本的应用与 Go/race test-runner 镜像、启动隔离的 MySQL 8.4.8 + RocketMQ 5.5.0 + NameServer + mock target，使用健康检查等真实依赖就绪，运行 migration/Topic/Group 初始化、宿主机控制 Broker 停启场景和 Docker 内完整测试。最新完整验收 `20261013T214902Z-747fc6e8` 退出码为 0；包含真实重试/DLQ、Outbox 标记失败重复发布、结果保存失败及 SIGTERM 故障窗口。详见[验收记录](docs/verification.md)与[精简证据](docs/evidence/20261013T214902Z-747fc6e8/)。

## 人工异常闭环

按 `status=FAILED` 或 `status=UNCERTAIN` 查询任务 API，检查投递尝试和 `result_reason`；管理员通过 resolve 接口记录核对结论。原生死信 topic 为 `%DLQ%notification_delivery_workers`。RocketMQ 管理工具在 Broker 镜像内可查询/导出：

```bash
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin consumerStatus -n namesrv:9876 -g notification_delivery_workers
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin printMsg -n namesrv:9876 -t '%DLQ%notification_delivery_workers' -c 20
```

先用该版本 `mqadmin help printMsg` 核对可用参数，再将必要消息导出到受控目录供负责人核对；MVP 不自动把 DLQ 同步成数据库待办，不自动重放死信或已终态任务。

## 文档导航

- [系统设计和状态边界](docs/design.md)
- [作业问题逐项回答](docs/assignment-answers.md)
- [讨论决定与替代方案](docs/decisions.md)
- [题目/补充约束映射](docs/requirements-mapping.md)
- [AI 使用说明](AI_USAGE.md)
- [Go/MySQL/RocketMQ 工程规范](docs/standards.md)
- [Docker 验收记录与证据链接](docs/verification.md)

## 已知限制

单实例、单 MySQL、单 Broker；没有全局机构 QPS 配额、自动熔断/半开、严格顺序、通用供应商响应映射、凭证刷新平台、自动 DLQ 重放或审批后台。宿主机意外断电/单 Broker 损坏不具备高可用保证。同步刷盘与命名卷只提升本机验收拓扑的持久性，不构成备份或跨故障域容灾。

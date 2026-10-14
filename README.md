# HTTP 通知服务 MVP

业务系统在用户注册、订单支付等事件发生后，常常需要调用外部机构的 HTTP 接口。如果直接在业务请求中等待外部响应，下游的延迟和故障就会影响上游；每个业务系统还需要自行处理重试、重复通知和结果查询。

本项目将这部分工作集中到一个异步通知服务中。调用方提交目标机构和请求内容，服务先保存任务，再完成后台投递，并提供状态查询与人工核对入口。技术栈为 **Go + MySQL/InnoDB + Apache RocketMQ + Transactional Outbox**，目前按单实例部署。

## 系统架构

```text
调用方 --HTTP/鉴权/幂等--> API --同一 MySQL 事务--> 通知任务 + Outbox
                                                        |
                                            Go 定时扫描并发布消息
                                                        v
                                                    RocketMQ
                                                        |
                               单条消费 + 有界并发 ------+
                                         |
                                         v
                                    外部 HTTP 接口
                                         |
                                         v
                                保存投递结果，完成消费
```

API、Outbox 发布器和消费者运行在同一个 Go 进程中。MySQL 保存任务、尝试记录和发布状态；RocketMQ 负责消息交付及执行异常的原生重试。完整流程、事务边界和状态转换见[设计说明](docs/design.md)。

## 三个关键设计

**1. 先可靠受理，再异步投递。** 通知任务和 Outbox 在同一个 MySQL 事务中提交，避免任务已经受理、消息却没有发出的双写问题。调用方使用幂等键重复提交时，同内容复用原任务，不同内容返回冲突。接口返回 `202` 表示任务已保存，外部机构的投递结果需要另外查询。

**2. 将业务重试和执行异常分开。** 每个任务最多进行三次 HTTP 业务尝试，包含首次。429/5xx 可以在剩余时间内退避重试；次数耗尽后保存终止失败结果并完成消费。只有数据库故障、进程中断等导致执行未完成或结果未可靠保存的情况，才交给 RocketMQ 原生重试，重复消息不会重新获得三次机会。机构长期不可用时，管理员可以开启快速失败；命中开关的任务保存终态，关闭开关后旧任务不会自动补发。

**3. 对结果不明的请求采取保守策略。** HTTP 成功后写库暂时失败，先重试保存结果，避免再次调用外部接口。若超时、断连或中断后的恢复无法确认远端是否执行，则记录 `UNCERTAIN` 并停止自动发送，交由人工核对。这降低了重复副作用的风险，代价是部分通知需要人工处理；服务不承诺最终必达或外部业务恰好一次。

这些选择的替代方案和代价见[设计决策](docs/decisions.md)，系统边界与扩容分析见[题目逐项答复](docs/assignment-answers.md)。

## 快速启动

需要 Docker Engine 和 Compose 插件；Windows 可使用运行 Linux 容器的 Docker Desktop，无需另装 Go、MySQL 或 RocketMQ。启动 Docker 后，在项目根目录执行：

```powershell
docker info
if (-not (Test-Path -LiteralPath '.env')) { Copy-Item .env.example .env }
.\scripts\dev-up.ps1
```

首次使用时复制 `.env.example`，已有配置保持不变。示例凭证只用于本地演示，实际使用前应替换。默认应用地址为 `http://127.0.0.1:8080`，MySQL 和 MQ 管理端口不映射到宿主机。

```powershell
docker compose -p rcnotif-dev ps
Invoke-RestMethod http://127.0.0.1:8080/healthz
Invoke-RestMethod http://127.0.0.1:8080/readyz
```

`/healthz` 检查进程存活，`/readyz` 检查 MySQL、MQ 客户端状态和 Broker TCP 连通性。默认配置及超时参数见 [.env.example](.env.example) 和[设计说明](docs/design.md)。

## API 与调用示例

客户端身份由 Bearer 凭证映射到 `CLIENT_ID`，不接受请求体自报身份。以下示例使用 `.env.example` 的演示 token；修改配置后需替换为相应值。

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
| `POST /api/v1/notifications` | 客户端 Bearer + `Idempotency-Key` | 同键同内容返回原任务；同键异内容 `409`；事务提交后返回 `202` |
| `GET /api/v1/notifications/{id}` | 客户端 Bearer | 查询当前客户端的任务与尝试记录，不返回原请求头或正文 |
| `GET /api/v1/notifications?status=FAILED&page=1&page_size=20` | 客户端 Bearer | 按状态分页查询，最大页大小 100 |
| `PATCH /api/v1/admin/destinations/{id}/fast-fail` | 管理员 Bearer | 请求体 `{"enabled":true,"reason":"人工原因"}`；记录配置身份、原因和时间 |
| `POST /api/v1/admin/notifications/{id}/resolve` | 管理员 Bearer | 请求体 `{"conclusion":"人工核对结论"}`；记录结论，不改变投递状态或重放任务 |
| `GET /healthz` | 无 | 检查进程存活 |
| `GET /readyz` | 无 | 检查依赖可用性；MQ 不可用时返回 503 |

演示目的地由 migration 初始化为 `http://mock-target:8090/notify`。请求只能引用已配置的机构，受理时会检查目标协议、主机和端口。管理员审计身份来自 `ADMIN_ACTOR`，不接受客户端自报。

## 测试与验证

2026-10-14 的归档 Docker 验收通过，覆盖 Go 格式和静态检查、单元测试、race 测试，以及真实 MySQL、RocketMQ、应用和 mock target 的集成链路。重点场景包括三次 503 后终止、重复消息、快速失败、结果写库故障、Broker 中断恢复和 SIGTERM。具体环境、命令、结果与证据见[验收报告](docs/verification.md)。

在项目根目录运行完整验收：

```powershell
.\scripts\verify-docker.ps1
```

仓库也提供 Bash 入口：

```bash
bash scripts/verify-docker.sh
```

已归档的通过记录使用 Windows PowerShell。使用 WSL 时，需先确认项目所在盘的挂载与 Docker 集成正常。脚本创建独立的 Compose project 和命名卷，报告输出到 `reports/<run_id>/`。

## 人工核对与死信排查

可按 `FAILED` 或 `UNCERTAIN` 查询任务，结合尝试记录和 `result_reason` 核对外部机构的实际结果。resolve 接口仅接受 FAILED、FAST_FAILED、EXPIRED、UNCERTAIN，保存结论后仍保留原投递状态。

RocketMQ 执行异常重试耗尽后进入 `%DLQ%notification_delivery_workers`。如果对应任务仍处于 PENDING/PROCESSING，或任务记录不存在，现有 resolve 接口无法记录结论，需要另外保留人工处置记录，详见[人工处理的限制](docs/implementation-review.md#r03dlq-消息不一定能通过现有-resolve-接口闭环)。可在 Broker 容器中查询消费组和死信：

```bash
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin consumerStatus -n namesrv:9876 -g notification_delivery_workers
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin printMsg -n namesrv:9876 -t '%DLQ%notification_delivery_workers' -c UTF-8 -d false
```

`-c` 指定字符集，`-d false` 关闭正文打印。需要查看事件中的 task/event ID 时，可在受控终端按需开启正文，仅保留必要标识和核对结论。当前没有自动死信重放功能。

## 文档导航

| 文档 | 重点内容 |
|---|---|
| [完整设计](docs/design.md) | 处理流程、事务边界、幂等、状态机、HTTP 投递与资源控制 |
| [题目逐项答复](docs/assignment-answers.md) | 系统边界、可靠性、AI 取舍、未来演进与中间件选择 |
| [设计决策](docs/decisions.md) | 备选方案、选择理由、收益和代价 |
| [AI 使用说明](AI_USAGE.md) | AI 参与范围、关键选择及具体实现案例 |
| [验收报告](docs/verification.md) | 测试方法、实际结果和可查阅的原始报告 |
| [已知限制与改进方向](docs/implementation-review.md) | 当前实现的边界、改进建议及源码依据 |

## 当前限制

当前采用单应用、单 MySQL、单 Broker，尚未提供多实例执行协调、机构级 QPS 配额、自动熔断或高可用部署，也没有生产吞吐量的压测结论。共享消费者资源意味着慢机构可能影响其他机构，后续需要结合负载引入分组隔离。

三次上限针对 HTTP 业务尝试，Go Transport 在部分连接故障下可能内部重试；快速失败开关也不会撤销已经在途的请求，配置读取与发送之间存在竞态。发送时目标复核、迁移中断恢复、异常日志与锁等待预算等问题详见[已知限制](docs/implementation-review.md)。

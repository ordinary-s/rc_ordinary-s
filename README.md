# HTTP 通知服务 MVP

这是一个单实例 Go 服务：上游通过 RocketMQ 直接发送版本化通知事件；服务先将事件持久化到 MySQL，再异步调用受控目标的 HTTP 接口。这里不再提供 HTTP 通知提交接口，也不在服务内创建通知或发布 Outbox。API 仅提供任务查询和管理操作。

## 边界与语义

- RocketMQ 内部按至少一次交付处理；重复消息通过 `(client_id,event_id)` 唯一约束、请求指纹和终态检查去重。
- 每条通知最多三次 HTTP 业务尝试，包含首次。三次 503 是正常业务终态，保存 `FAILED` 后确认消息，不再次发 MQ。
- `FAST_FAILED`、`EXPIRED` 和 `UNCERTAIN` 都是有记录的终止结果，不表示已送达。关闭快速失败只影响后续非终态通知，不复活旧任务。
- 只有消息尚未可靠保存处理结果的执行异常才交 RocketMQ 原生重试，耗尽后进入消费组 DLQ。
- 不承诺每条通知最终必达或外部业务恰好一次。下游幂等键只有在下游实际实现去重时才有作用。
- 新旧版本不能混跑同一个 Topic/Group：新版使用 `notification_ingress_v1` / `notification_ingress_workers_v1`，消息体必须符合 `schema_version=1`。

## 架构

```text
上游业务事务 + 上游 Outbox（由上游负责）
          |
          | RocketMQ：完整版本化事件（event_id 稳定）
          v
Go 消费者（单条回调、有界并发）
          |
          | MySQL：先持久化 notification；唯一键去重
          v
HTTP 投递器（最多 3 次业务尝试）
          |
          +--> 终态保存后 ACK
          +--> 执行未完成时 RocketMQ 原生重试 / DLQ

HTTPS 查询/管理员 API ------> MySQL
```

MQ 事件至少包含：

```json
{
  "schema_version": 1,
  "event_id": "order-123-notification",
  "trace_id": "trace-7a91",
  "destination_id": "mock-target",
  "method": "POST",
  "headers": {"Content-Type": "application/json"},
  "body": "{\"order_id\":\"123\"}",
  "expires_at": "2026-10-15T16:00:00Z"
}
```

生产者通过配置把可信上游身份绑定到消费组设置 `MQ_TRUSTED_CLIENT_ID`；事件正文不能自报 `client_id`。`event_id` 必须在上游事务重发时保持不变。指纹覆盖目标、规范化方法、规范化 Header、原始 UTF-8 Body 和绝对 UTC 有效期，不覆盖 `trace_id`。同身份同事件 ID、同指纹复用任务；同 ID 不同内容是协议冲突，按原生异常重试/DLQ，不覆盖、不发送。

## 启动演示

需要 Docker Engine、Compose 和可用网络；本地无需 Go、MySQL 或 RocketMQ：

```powershell
Set-Location 'D:\rc_notification'
if (-not (Test-Path .env)) { Copy-Item .env.example .env }
.\scripts\dev-up.ps1
```

```powershell
curl.exe --ssl-no-revoke --cacert .local-certs\ca.crt https://localhost:8443/healthz
curl.exe --ssl-no-revoke --cacert .local-certs\ca.crt https://localhost:8443/readyz
docker compose -p rcnotif-dev ps
```

`--ssl-no-revoke` 只用于缺少吊销服务的本地演示证书，仍验证证书链；生产客户端不可照搬。默认 TLS 最低为 1.3。数据库、NameServer 和 Broker 管理端口不映射到宿主机。示例 token、HMAC key 和证书仅用于本机。

发送演示消息（该命令仅是测试生产者，不代表上游业务 Outbox）：

```powershell
docker compose -p rcnotif-dev --profile verify run --rm --build test-runner go run ./cmd/demo-producer -event-id demo-order-123 -count 2 -content-type application/json -body '{"order_id":"123"}'
```

两条消息使用同一事件 ID 和内容，用于验证 Broker 收到重复消息时只处理同一任务。查看 Broker 消费组和 DLQ：

```bash
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin consumerStatus -n namesrv:9876 -g notification_ingress_workers_v1
docker compose -p rcnotif-dev exec broker /home/rocketmq/rocketmq-5.5.0/bin/mqadmin printMsg -n namesrv:9876 -t '%DLQ%notification_ingress_workers_v1' -c UTF-8 -d false
```

查看死信正文应在受控终端按需执行，并只记录必要的事件标识和处置结论。系统不自动同步 DLQ 为数据库工单，也不自动重放。

## 查询与管理 API

除 `/healthz`、`/readyz` 外，`/api/*` 都要求 HTTPS、客户端或管理员 Bearer token、RFC 9421 HMAC-SHA-256 签名及未使用 nonce。HMAC 验证来源和完整性，不加密正文；TLS 才保护传输。签名覆盖方法、authority、目标 URI、path、Authorization、Content-Digest 和有效期；不同 key ID 区分客户端与管理员。生成签名需要 RFC 9421 客户端实现，不能复制复用 nonce。

| 路由 | 身份 | 说明 |
|---|---|---|
| `GET /healthz` | 无 | 进程存活探针 |
| `GET /readyz` | 无 | 检查 MySQL、消费者启动状态和 Broker 连通性；依赖不可用返回 503 |
| `GET /api/v1/notifications/{id}` | 客户端 | 只查询认证客户端自己的任务和尝试记录 |
| `GET /api/v1/notifications?status=FAILED&page=1&page_size=20` | 客户端 | 分页列表，最大页大小 100 |
| `PATCH /api/v1/admin/destinations/{id}/fast-fail` | 管理员 | 请求 `{"enabled":true,"reason":"人工原因"}`；记录操作人、原因、时间 |
| `POST /api/v1/admin/notifications/{id}/resolve` | 管理员 | 请求 `{"conclusion":"核对结论"}`；记录结论，不改变投递状态或重发 |

目标通过初始化数据配置，且必须匹配 `ALLOWED_TARGET` 的 HTTP(S) scheme、host 和 port。Body 限为 UTF-8 原始文本，支持 JSON、XML、表单和纯文本，不支持 multipart/大文件。下游 HTTPS 会验证证书；服务关闭自动重定向以限制 SSRF 风险。

## Docker 完整验收

Windows：

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

WSL/Linux（先确认 /mnt/d 确实映射 D 盘）：

```bash
findmnt -T /mnt/d/rc_notification
cd /mnt/d/rc_notification
bash scripts/verify-docker.sh
```

脚本使用固定版本镜像和独立 Compose project，启动真实 MySQL、RocketMQ NameServer/Broker、应用及可计数 mock target；容器内运行格式、vet、单测、race 和真实集成/E2E。它验证直入 MQ、重复/冲突、3 次 503、数据库异常原生重投和 DLQ、SIGTERM、快速失败、到期及 TLS/HMAC。最新验收 PASS：run 20261015T150311Z-bf5a516b，完整脚本退出码 0；精简证据见[本次报告目录](docs/evidence/20261015T150311Z-bf5a516b/)和[验收说明](docs/verification.md)。完整日志保存在本机 reports/<run_id>/，不上传。只有最新代码完整脚本退出码为 0 才记录 Docker PASS。

## 迁移与切换

新版另用 notification_ingress_v1，不消费旧 ID-only 消息，也不读取旧 Outbox。切换前让旧版本停止接收新业务、排空旧 Topic 和消费组；检查旧数据库中未发布 Outbox：

```sql
SELECT o.outbox_id, o.event_id, o.task_id, o.publish_state, o.next_attempt_at
FROM outbox AS o
ORDER BY o.next_attempt_at, o.outbox_id
LIMIT 1000;
```

旧表和卷保留，不运行旧/新版本共同写同一任务。旧待发记录须由旧版本继续发布或由负责人核对后按新 schema 显式导出；不能把旧消息直接改成新版格式，也不能把旧 FAST_FAILED/FAILED 自动复活。迁移 004_source_event_id.sql 为旧任务增加 nullable 新事件去重键，不删除历史数据。

## 文档

- [设计和状态机](docs/design.md)
- [作业逐项答复](docs/assignment-answers.md)
- [决策记录](docs/decisions.md)
- [要求映射](docs/requirements-mapping.md)
- [Go、MySQL、RocketMQ 规范落实](docs/standards.md)
- [AI 使用说明](AI_USAGE.md)
- [Docker 验收记录](docs/verification.md)
- [实现审查与限制](docs/implementation-review.md)

## 限制

MVP 以一个 Go 实例、一个 MySQL 和单 Broker 开发拓扑为目标，不提供多实例全局配额、HA、生产容量保证、自动熔断、半开探测、自动 DLQ 工单或终态批量重放。上游需要自行保证其业务事务到上游 Outbox 的原子交接；本项目不接管上游事务。远端成功、本地无法确认时保存 `UNCERTAIN`，下游不支持幂等时仍存在重复副作用风险。`http_attempts` 表示应用启动的业务尝试；Go `net/http` 对少数连接失败有内部重试行为，因此不能将三次业务预算表述成任意网络故障下绝对最多三次物理发送。

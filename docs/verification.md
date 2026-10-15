# Docker 验收记录

## 最新完整结果

**PASS。** 最新源代码及配置于 2026-10-15 在 Docker Linux 容器内完成全部验收，PowerShell 脚本退出码 0。测试使用隔离 Compose project，真实 MySQL、RocketMQ NameServer/Broker、应用 HTTPS API 和可计数 mock target；不是 Mock MQ 验收。

| 项目 | 实际结果 |
|---|---|
| 项目路径 | `D:\rc_notification` |
| Run ID / Compose project | `20261015T074216Z-479b6b19` / `rcnotif-verify-20261015t074216z-479b6b19` |
| 完整运行时间（UTC） | `2026-10-15T07:42:16Z` 至 `2026-10-15T07:47:04Z` |
| 验收命令与退出码 | `.\scripts\verify-docker.ps1`；**0** |
| Docker Desktop / Engine / Compose | `4.69.0` / `29.4.0` Linux amd64 / `5.1.1` |
| Go / MySQL / RocketMQ / Go SDK | `1.27.2` / `8.4.8` / `5.5.0` / `rocketmq-client-go/v2 v2.1.2` |
| HMAC 实现依赖 | `github.com/lestrrat-go/htmsig v1.0.0` |
| 应用镜像 | `rc-notification:0.1.0`，ID `sha256:3d2c4552dea91d833104ee74f7e6d62a29fb7933c158dd8d6255d680d58b4a3c` |
| test-runner 镜像 | ID `sha256:feed932bfbcaa7155c31a668cde772f45bedf799dc31cb4e4c84b5a44687046d` |
| cert-init 镜像 | ID `sha256:518280b90b4b8788ca504db19e35652f7b79909d563976b31161bbdb9fcbb393` |
| 核心检查 | gofmt、vet、普通测试、race、集成编译、真实 E2E 均退出码 0 |
| 外围故障步骤 | Broker 中断恢复、SIGTERM 在途处理恢复、日志敏感内容扫描、task/trace 配对扫描、隔离资源清理均退出码 0 |

精简、脱敏证据在 [`docs/evidence/20261015T074216Z-479b6b19/`](evidence/20261015T074216Z-479b6b19/)，完整本地报告在 `reports/20261015T074216Z-479b6b19/`（该目录被 `.gitignore` 排除）。证据摘要为 [`summary.txt`](evidence/20261015T074216Z-479b6b19/summary.txt)、[`checks-summary.txt`](evidence/20261015T074216Z-479b6b19/checks-summary.txt) 和 [`integration.json`](evidence/20261015T074216Z-479b6b19/integration.json)。

## 通过的 17 个真实集成场景

| 场景 | 实际观察 |
|---|---|
| TLS 1.3 与 HMAC | 协商 TLS1.3；TLS1.2-only 握手被拒；无签名、正文篡改、Authorization 篡改、过期签名、nonce 重放均被拒；客户端签名密钥不能访问管理员路由 |
| 可靠受理及入口幂等 | 同一事务保存 notification 和 Outbox；并发重复提交只生成一个任务；同键不同内容 409；键大小写敏感 |
| HTTP 恢复预算 | 首次 503 后第二次成功为 2 次；连续 503 的请求数/业务次数均恰好 3，重复终态消息未增加调用 |
| 快速失败 | 开启时外部调用为 0；关闭开关后新任务执行，旧 FAST_FAILED 不复活 |
| 原始请求体 | XML 和表单内容、Content-Type 按预期透传 |
| 外部成功与结果写库故障 | 临时写入失败后成功保存而 HTTP 只调用 1 次；持续写库失败在 MQ 恢复后记录 UNCERTAIN，也没有再次调用 HTTP |
| 数据库异常与 RocketMQ | 真实 DB 故障触发原生重投；受控重试审计收到 `mq_reconsume=1`；异常达到配置上限后实测进入 `%DLQ%notification_delivery_workers` |
| Outbox 和查询计划 | Broker 接收后模拟本地发布标记失败，重复消息不重复 HTTP；MySQL `EXPLAIN` 使用 `idx_outbox_pending_due`，会话时区为 UTC |
| 任务日志上下文 | 8 个并发 task/trace 对应正确，在途数不超过配置 4；应用日志敏感 canary 检查通过 |
| 中断恢复 | Broker 停止期间 Outbox 保留，恢复后发布；HTTP 在途时 SIGTERM，应用重启后不提前确认，任务以 UNCERTAIN 恢复且无重复 HTTP |

部分验证边界：持久退避场景用真实 MySQL/mock 和本地 Engine 调用；其他异常恢复覆盖真实 MQ。Outbox 场景用 MySQL trigger 注入本地标记错误，没有伪称模拟了丢失确认包。503 测试证明此受控场景的应用业务尝试数和 mock 请求数均为 3，不推广为任意网络故障下 Transport 绝不重发。

## 复现命令

Windows PowerShell，在项目根目录运行：

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

脚本会核验 Docker/Compose、校验 Compose、构建固定版本应用和测试镜像、启动独立 Compose project、等待真实健康状态、迁移/初始化 Topic 和 Group，并在 test-runner 容器中执行所有检查及 E2E。它在宿主脚本中驱动 Broker 和应用停启，保存 `reports/<run_id>/` 与可提交的精简 `docs/evidence/<run_id>/`；任何必需步骤失败都会返回非零。

Bash 等价入口是 `bash scripts/verify-docker.sh`。本次在 Windows PowerShell 执行；WSL D 盘挂载和 Bash 入口未执行。

另外，完整验收前实际执行了 `dev-up.ps1` 和 README 中的签名客户端命令，均退出码 0；客户端返回 `status=202 tls=TLS 1.3`，CA 校验下 `/healthz` 和 `/readyz` 分别返回 `live`、`ready`。Windows Schannel 首次因本地演示证书没有吊销服务而返回 60；README 本地命令使用 `--ssl-no-revoke` 跳过吊销查询但仍校验证书链。该独立演示记录在 [`dev-up-and-demo.txt`](evidence/20261015T073116Z-b685fa22/dev-up-and-demo.txt)。本地演示使用既有 `rcnotif-dev` Compose project 与命名卷；完整验收则是另一个隔离 project。

## 失败运行与修正记录

本轮验收前保留了两次失败记录，不能将其当作通过：

| Run ID | 失败点 | 处理 |
|---|---|---|
| `20261015T072053Z-74b908aa` | SIGTERM 测试重启应用时 cert-init 覆盖测试 CA，test runner 仍信任旧 CA | 证书初始化改为完整文件集存在时复用 CA/服务证书；完整脚本重跑 |
| `20261015T072540Z-0f422001` | 安全用例改写 `Request.Body` 却复用原请求体工厂，导致实际发出的不是被篡改线缆正文；同时签名受理用例的任务未在下个场景 reset mock 前完成 | 测试现在构造新的实际线缆请求，并等待已受理任务终态；这修正的是测试隔离与请求构造，不放宽服务验签。之后两轮完整脚本均通过 |

失败运行的本地原始报告分别位于 `reports/20261015T072053Z-74b908aa/`、`reports/20261015T072540Z-0f422001/`；这些完整调试日志由 `.gitignore` 排除。仓库只包含最新成功验收的精简证据，失败原因与修复摘要保留在上表。

## 限制

- 本次是单应用、单 MySQL、单 Broker 的开发/验收拓扑，不证明 HA、生产吞吐或多实例全局限流。
- Compose 自签名证书与示例 HMAC/Bearer 值只供本地演示。生产环境需配置有效 TLS 证书和随机秘密，制定密钥轮换，并限制 TLS 终止网关到应用的网络入口。HMAC 不加密，MySQL 正文仍为明文应用数据。
- 当前签名覆盖应用实际收到的 URI；如使用 TLS 终止网关重写 URI，需实现并严格信任网关传递的原始目标 URI，不能信任外部任意转发头。
- 没有生产容量压测、跨故障域 HA、所有 HTTP Transport 连接故障组合测试、自动密钥轮换和数据静态加密验证。
- RocketMQ DLQ 能力已真实验证，但不自动生成数据库人工待办；当前操作人需查询任务、记录结论，并在需要时用 `mqadmin` 检查 DLQ。

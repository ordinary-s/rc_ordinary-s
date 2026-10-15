# Docker 验收记录

## 最新 MQ 直入版本验收

**结果：PASS。** 2026-10-15 在独立 Docker Compose project 中运行完整脚本，真实启动 MySQL、RocketMQ NameServer/Broker、Go 应用和可计数 mock target。测试通过 API 检查/管理，再经真实 RocketMQ 消费完成端到端处理。

| 项目 | 实际值 |
|---|---|
| 项目目录 | D:\rc_notification |
| Run ID / Compose project | 20261015T150311Z-bf5a516b / rcnotif-verify-20261015t150311z-bf5a516b |
| 本机时间 | 2026-10-15 23:03:11 至 23:10:18，Asia/Shanghai |
| UTC 时间 | 2026-10-15 15:03:11Z 至 15:10:18Z |
| 执行命令 | .\scripts\verify-docker.ps1 |
| 退出码 | 0 |
| Docker Desktop / Engine / Compose | 4.69.0 / 29.4.0 Linux amd64 / 5.1.1 |
| Go / MySQL / RocketMQ / Go SDK | 1.27.2 / 8.4.8 / 5.5.0 / rocketmq-client-go/v2 v2.1.2 |
| 应用镜像 | rc-notification:0.1.0，sha256:b1b6e22332826014c30cff61797a5f335e6aed473874f1ea7702c4a414d68f74 |
| Go test-runner 镜像 | rc-notification-test:0.1.0，sha256:845833a037f102b083a92bff41350696a89f57e1d596dc08a92c338b2fccd61f |
| 证书初始化镜像 | rc-notification-cert-init:0.1.0，sha256:518280b90b4b8788ca504db19e35652f7b79909d563976b31161bbdb9fcbb393 |

必需阶段退出码全部为 0：Docker/Compose 检查、Compose config --quiet、三镜像构建、隔离服务健康、SIGTERM 注入、gofmt、go vet、go mod tidy -diff、go test、go test -race、集成编译、真实集成/E2E、日志 secret scan、task/trace 配对 scan、仅清理本次 project。真实集成耗时 238.442 秒，SIGTERM 子测试耗时 34.90 秒。没有用 Mock MQ 替代 Broker。

### 关键场景结果

| 场景 | 实际结果 |
|---|---|
| TLS、签名和入口边界 | TLS1.3 协商通过；TLS1.2 和无签名 API 请求被拒；已移除的 HTTP submit 路由不接受提交 |
| 直入 MQ、固定 ID 重发 | 两条相同 event_id 消息只生成一行任务和一次 HTTP 调用；演示生产者直接向固定 Topic 发送 |
| 同 ID 不同内容 | 不覆盖已有任务、不调用冲突正文，实际原生重试并进入 DLQ |
| 并发重复消息 | 8 条重复事件对应一个 task 和一次 HTTP 调用 |
| HTTP 首败次成 | 一次 503 后第二次成功；业务尝试和 mock 请求均为 2 |
| 连续 503 | 终态 FAILED；业务尝试和 mock 请求均恰好为 3；重复事件观察窗口内没有增加调用 |
| DB 写入异常、HTTP 前恢复 | 真实 MySQL trigger 注入失败；没有提前发 HTTP；同一 MQ 消息重投后任务持久化并 DELIVERED |
| 结果写入异常 | HTTP 仅调用一次；结果无法可靠确认时保存 UNCERTAIN，不重复发送 |
| 快速失败、有效期、人工处置 | FAST_FAILED 时零 HTTP；关闭后新消息可投递，旧 FAST_FAILED 不复活；过期任务不调用；resolve 保存结论并保留状态 |
| 格式与身份隔离 | XML 和表单原文/Content-Type 透传；不同 trusted client 可使用相同 event_id 而得到不同任务 |
| 原生重试与 DLQ | 坏 JSON、不支持 schema、事件内容冲突、执行错误实际到达 DLQ；验收 max reconsume 配置为 2，SDK/Broker 交付对象报告 ReconsumeTimes=3，测试记录该具体行为 |
| SIGTERM | 宿主脚本在 HTTP 调用过程中停止应用，等待退出再启动；消息经原生重投后任务保存 UNCERTAIN，实际 HTTP/mock 请求均为 1 |

测试没有精确注入“终态数据库 commit 成功但 Broker ACK 前立即进程死亡”这一窗口。重复终态事件验证了持久状态幂等，但不能冒称精确模拟 ACK 丢失。SIGTERM 验证的是 HTTP 在途中断；数据库注入另验证 HTTP 前执行未完成的重投。未运行生产容量压测、HA 或多实例验证。

### 可复查证据

- 精简脱敏目录：[docs/evidence/20261015T150311Z-bf5a516b/](evidence/20261015T150311Z-bf5a516b/)
- 版本、退出码和镜像 ID：[summary.txt](evidence/20261015T150311Z-bf5a516b/summary.txt)
- Docker 检查输出：[checks.log](evidence/20261015T150311Z-bf5a516b/checks.log)
- 每项集成场景：[integration.json](evidence/20261015T150311Z-bf5a516b/integration.json)
- SIGTERM 场景：[sigterm.json](evidence/20261015T150311Z-bf5a516b/sigterm.json)
- Compose 状态：[compose-ps.txt](evidence/20261015T150311Z-bf5a516b/compose-ps.txt)
- 限量应用结果日志：[app-results.log](evidence/20261015T150311Z-bf5a516b/app-results.log)
- task/trace 配对核验：[log-context.tsv](evidence/20261015T150311Z-bf5a516b/log-context.tsv)
- 完整本机报告：D:\rc_notification\reports\20261015T150311Z-bf5a516b\（Git 忽略，不上传全部运行日志）

### 首轮失败及修正

MQ 直入首轮 20261015T145442Z-31cfd646 完整脚本退出码为 1，不能作为通过。它发现 Go SDK resolver 需要先解析 NameServer IP，以及 MySQL trigger 持续阻断直到 DLQ 会使 HTTP 前恢复用例超出观察窗口并影响后续测试。修正为在 consumer 和演示 producer 初始化前解析 NameServer DNS，并让 trigger 只失败一次，确认无 HTTP 后在 Broker 重投前撤除。随后完整运行 20261015T150311Z-bf5a516b 退出码 0。失败完整日志保存在本机 reports，未纳入精简证据目录。

旧架构 run 20261015T074216Z-479b6b19 属于 HTTP + 本地 Outbox 方案，不作为本次 MQ 直入验收依据。

## 复现命令

Windows PowerShell：

    Set-Location 'D:\rc_notification'
    .\scripts\verify-docker.ps1

WSL/Linux 入口等价，但本次没有执行 WSL 版本。运行前用 findmnt 确认 /mnt/d/rc_notification 实际位于 D 盘，并确认 WSL 可访问同一 Docker Engine。

## 剩余限制

- 单实例、单 MySQL、单 Broker；SYNC_FLUSH 和命名卷不等于 HA、备份或机器永久损坏不丢失。
- Compose CA 和 Bearer/HMAC 值仅供本机；生产证书、随机密钥、轮换、MQ ACL/TLS 和网关网络限制需另外配置审核。
- 没有生产压测、自动熔断、机构全局限流、跨实例执行协调、自动 DLQ 工单或死信自动重放。
- Outbox 是上游职责；切换前须排空旧链路并核对旧 outbox.pending，旧 outbox 表保留。
- 三次是应用业务尝试的记录上限，不是已证明的任意网络错误下物理发送绝对上限；下游不支持幂等时外部副作用可能重复或结果未知。

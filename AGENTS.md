# 项目约定

- 正式项目目录固定为 `D:\rc_notification`；WSL 仅使用经 `findmnt` 确认为 D 盘的 `/mnt/d/rc_notification`。
- 方案为 Go + MySQL/InnoDB + Apache RocketMQ + MySQL Transactional Outbox；这是单实例 MVP。
- 每个 task 最多三次 HTTP 业务尝试（首次加两次重试）。HTTP 终止失败、FAST_FAILED、EXPIRED、UNCERTAIN 在结果保存后结束消费；只有执行未完成/结果未可靠保存才返回 RocketMQ 原生重试。
- 机构快速失败是管理员明确配置的终止策略；关闭开关只影响后续非终态任务，不复活终态任务。
- RocketMQ 至少一次消费；不要新增业务失败重发队列、批次协调、自动熔断、Redis 锁或第二套异常重试。
- Go 工具链使用 Dockerfile 锁定的 Go 镜像。代码改动后运行 `gofmt`、`go vet ./...`、`go test -count=1 ./...` 和 `go test -race -count=1 ./...`；不可用宿主机伪装 Docker 验收。
- MySQL 表列小写 snake_case、utf8mb4、明确排序规则、UTC `DATETIME(6)`、参数 SQL 和版本 migration；DDL 隐式提交不可假设可回滚。
- 错误和日志不写请求/响应正文、Authorization、Cookie、凭证或含 Token 的 URL。trace/task 关联，不使用共享可变 logger 传请求状态。
- 不删无关文件、不运行全局 Docker prune、不覆盖/强推远端。验收仅清理由该 run 创建的 Compose project 和命名卷。

## Docker 验收

Windows：

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

WSL/Linux（先核对 D 盘挂载）：

```bash
findmnt -T /mnt/d/rc_notification
cd /mnt/d/rc_notification
bash scripts/verify-docker.sh
```

最终验收必须运行真实 MySQL、RocketMQ Broker/NameServer、应用与 mock target；报告在 `reports/<run_id>/`，精简证据在 `docs/evidence/<run_id>/`。仅当脚本退出码为 0 且关键场景都有实际证据时才可写 Docker PASS。

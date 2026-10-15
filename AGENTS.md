# 项目约定

- 正式项目目录固定为 `D:\rc_notification`；WSL 只在用 `findmnt` 确认 D 盘真实映射后使用 `/mnt/d/rc_notification`。
- 技术栈为 Go、MySQL/InnoDB 和 Apache RocketMQ；单应用、单实例 MVP。上游负责业务事务与自己的 Outbox。
- 本服务只从稳定 Topic `notification_ingress_v1` 的版本化 MQ 事件接收通知；不提供 HTTP 通知提交路由、不写新 Outbox、不内置 MQ 发布器。消费者组为 `notification_ingress_workers_v1`。
- 上游事件须含 schema_version、稳定 event_id、trace_id、destination_id、method、UTF-8 原始文本 body、headers 和绝对 UTC expires_at。身份从 `MQ_TRUSTED_CLIENT_ID` 配置绑定，不能从消息正文读取 client_id。相同身份和事件 ID 但业务指纹冲突是协议错误，按 RocketMQ 原生重试/DLQ 处理，不覆盖任务或调用 HTTP。
- 每个 task 最多三次 HTTP 业务尝试（首次+两次重试）。FAILED、FAST_FAILED、EXPIRED、UNCERTAIN 及 DELIVERED 在结果保存后都完成消费；只有结果未可靠保存或执行异常才交 RocketMQ 原生有限重试和 DLQ。
- 机构快速失败是管理员明确配置的终止策略。关闭开关只影响后续可执行任务，不复活终态任务。禁止第二套业务重放、异常次数调度、批次部分确认、自动熔断、Redis 锁或多实例能力声明。
- 每次 HTTP 请求使用下游业务幂等标识，但不得声称下游恰好一次。HTTP 次数表示本地启动次数；Go Transport 可能在特定网络失败内部重试。
- Go 使用短职责包、显式依赖、context 和错误包装。改动后在固定 Docker Go 镜像中执行 gofmt、vet、单测、race 与真实 MQ 集成验收。
- MySQL 表列索引使用小写 snake_case，utf8mb4 和明确 collation；幂等标识按大小写敏感比较，时间使用 UTC DATETIME(6)，参数化 SQL 和版本迁移。命名/时间形式是项目约定；DDL 的隐式提交不假定能回滚。
- API 服务最低 TLS 1.3；除健康探针外，/api 路由要求 Bearer token 与 RFC 9421 HMAC-SHA-256 签名以及一次性 nonce。HMAC 不加密数据，不引入自定义 Body 加密。生产密钥/证书不可使用 Compose 演示值。
- 日志不输出通知正文、完整 Header、Authorization、Cookie、凭证或含 Token 的 URL；trace/task 通过局部字段关联。
- 不删除旧 outbox 表、历史 Topic 或命名卷；MQ 协议切换使用新版 Topic/Group 并先排空旧链路。不得清理无关文件、运行全局 Docker prune 或强推 Git。

## Docker 验收

Windows:

```powershell
Set-Location 'D:\rc_notification'
.\scripts\verify-docker.ps1
```

WSL/Linux（先确认 D 盘挂载）:

```bash
findmnt -T /mnt/d/rc_notification
cd /mnt/d/rc_notification
bash scripts/verify-docker.sh
```

完整验收必须使用隔离 Compose project 启动真实 MySQL、RocketMQ NameServer/Broker、应用与 mock target。只有最新代码完整脚本退出码 0，且关键场景有报告证据，才可写 Docker PASS。完整报告在 reports/<run_id>/，精简证据在 docs/evidence/<run_id>/。

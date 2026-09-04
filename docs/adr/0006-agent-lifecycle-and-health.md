# ADR-0006: Agent 身份生命周期与运行健康分离

- Status: Accepted
- Date: 2026-09-04

## Context

Agent 的证书授权状态和最近是否收到 heartbeat 是两类事实。若把 `OFFLINE`、`DEGRADED` 直接写进授权用的 `status`，断线后的 Agent 会因为不再是 `ACTIVE` 而无法重新连接；定时改写状态也会产生无意义的数据库写放大和竞态。

Desired State 同时需要在 Server 重启、outbox 已发布但 Agent 尚未确认等情况下继续收敛，不能只依赖进程内队列或 `published_at`。

## Decision

- `agents.status` MUST 只表示身份生命周期：`ACTIVE | REVOKED`；
- `health` MUST 在读取时根据 `last_seen_at`、诊断代码和配置拒绝状态推导，不作为授权条件持久化；
- ACTIVE Agent 在 45 秒内收到有效 heartbeat 为 `ONLINE`，诊断或当前配置拒绝为 `DEGRADED`，超过 45 秒未收到 heartbeat 为 `OFFLINE`；
- `last_connected_at` MUST 只表示 mTLS stream 建立；`last_seen_at` MUST 只在接受有效 heartbeat 后更新；
- REVOKED 身份 MUST 始终拒绝连接，并在 API 中显示 `REVOKED`；
- Server MUST 在每次连接时比较 Agent 报告的 `accepted_revision` 与持久化 `desired_revision`；前者较小时直接重发完整快照；
- outbox 仍用于事务边界和异步通知，但 Desired State 重连收敛 MUST NOT 依赖内存队列或未发布 outbox 行；
- Agent MUST 先验证并写 staging，再原子替换 active 配置；配置拒绝 MUST 保留 last-known-good。

## Consequences

- 离线 Agent 可凭仍有效且未撤销的证书重新连接；
- Dashboard/metrics 查询会执行一次基于当前时间的 health 投影，不需要周期性状态更新任务；
- heartbeat 验证失败不会刷新 `last_seen_at`；
- outbox publish 与 Agent ACK 之间的崩溃不会丢失配置：重连比较会再次发送相同 revision，Agent 幂等确认；
- API 同时暴露 `status` 与 `health`，调用方不能再把 `ACTIVE` 翻译为“在线”。

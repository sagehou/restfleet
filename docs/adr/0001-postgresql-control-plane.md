# ADR-0001: PostgreSQL 作为 V1 控制面数据库

- Status: Accepted
- Date: 2026-09-02

## Context

控制面需要同时处理 Agent heartbeat、Desired State、Operation 状态、日志、通知、审计和仓库维护任务。任务需要可靠租约、幂等、行锁和事务 outbox。

## Decision

V1 生产控制面使用 PostgreSQL 16+。Agent 本地状态使用 bbolt。V1 不引入 Redis 或独立消息中间件。

## Consequences

- Docker Compose 多一个持久服务；
- 可以使用 `FOR UPDATE SKIP LOCKED`、advisory lock、JSONB 与 partial indexes；
- 避免产品可用后再从 SQLite 迁移关键状态机；
- Worker 必须幂等，PostgreSQL queue 不意味着 exactly-once execution。

## Rejected

- SQLite for production：初期简单，但并发写、worker lease 与后续迁移风险不匹配。
- Redis queue：增加必需依赖，仍不能替代 authoritative SQL transaction。

# ADR-0003: Agent 本地调度与出站 mTLS 长连接

- Status: Accepted
- Date: 2026-09-02

## Context

中心定时向每台 VPS 发命令会使控制面短暂故障变成全局停止备份；中心主动连接 VPS 还要求入站端口、NAT 处理或 SSH 权限。

## Decision

- Agent 主动建立 gRPC/HTTP2 + mTLS 双向流；
- 中心下发 versioned Desired State；
- Agent 原子保存 last-known-good，并使用本地 cron/timezone scheduler；
- remote Backup Now 等即时任务通过 durable job stream 下发；
- Agent 离线时继续 schedule，并在重连后幂等补报。

## Consequences

- VPS 不开放管理端口；
- 控制面离线不影响调度逻辑；
- Gateway 与中心同机离线仍可能使数据写入失败，这是不同故障域；
- Agent 需要小型 durable local state 和 reconciliation 逻辑；
- 协议必须设计 at-least-once delivery、duplicate 与 crash recovery。

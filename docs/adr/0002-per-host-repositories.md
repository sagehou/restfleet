# ADR-0002: V1 默认且仅支持一 Host 一 Repository

- Status: Accepted
- Date: 2026-09-02

## Context

Restic backup 客户端必须读取仓库索引并持有 repository password。Gateway 的 append-only 保护防止删除/改写，但不是 write-only，也不能阻止该 Host 解密自身历史备份。

Shared Repository 会让任何持有 password/read access 的 Agent 扩大到其他 Host 的历史数据，并扩大损坏与维护故障域。

## Decision

V1 每个 Host 创建独立 Repository、Restic password、gateway username/password 与 backend UUID path。API 明确拒绝 Shared Repository。

## Consequences

- 单 Host 失陷不能读取其他 Host 的 Repository；
- restore/maintenance 故障域更小；
- 失去跨 Host 去重；
- Repository 数量增加，但目标规模可接受；
- V2 若支持共享，必须新建显式模型、迁移和风险确认，不能仅移除一个约束。

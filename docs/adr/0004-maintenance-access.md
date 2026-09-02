# ADR-0004: 完整仓库权限只用于中心 Maintenance Plane

- Status: Accepted
- Date: 2026-09-02

## Context

Agent 需要 append-only gateway 进行 backup，但 `forget`、`prune`、`check` 与 `unlock` 需要完整 repository access。对公网提供第二个 full-access REST endpoint 会增加凭据和路由误配置风险。

## Decision

V1 默认由中心 Maintenance Worker 通过内部 rclone backend/stdio 或不可路由的内部 listener 访问 Repository。公网只暴露 append-only Data Plane Gateway。

## Consequences

- Agent 永不收到 maintenance/rclone credentials；
- destructive maintenance 可集中串行、预览和审计；
- 中心是最高权限 trust boundary，必须保护 master key、DB 与 runtime files；
- 若部署内部 admin listener，必须有网络不可达验收测试；
- 中心完全失陷仍可能删除备份，需依赖中心加固和未来外部 immutability 进一步降低风险。

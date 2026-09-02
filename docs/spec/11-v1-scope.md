# 11 — V1 范围与发布边界

## 1. V1 定义

V1 是可以真实用于少量到数十台 Linux VPS 的完整最小产品，不是 UI prototype。必须覆盖 enrollment → plan → local schedule → backup → status → snapshot → download/restore → retention/maintenance → notification/audit 的闭环。

## 2. V1 Must Have

### Platform

- Go Server、Gateway wrapper 与 Agent；
- React/TypeScript Web；
- PostgreSQL；
- Docker Compose 中心部署；
- Native systemd + Docker Agent；
- linux/amd64 + linux/arm64。

### Identity/Security

- 管理员 bootstrap/login/session/CSRF；
- single-use enrollment token；
- Agent local Ed25519 key + mTLS；
- 证书 30d/提前轮换、server-side revoke；
- per-Host Repository/password/gateway identity；
- envelope-encrypted central secrets；
- append-only/private Repository Gateway；
- secret redaction；
- audit for sensitive actions。

### Fleet/Config

- Host/Agent management；
- Repository/StorageCredential；
- Retention/Maintenance policy；
- BackupTemplate revision；
- Plan + override/effective config；
- desired/accepted revision；
- local scheduler、misfire、jitter、FORBID concurrency；
- center offline last-known-good execution。

### Execution

- scheduled Backup；
- Backup Now、Cancel、Retry；
- Restic JSONL parsing；
- Operation state/timeline/logs；
- exit 0/3/10/11/12/130 + unknown handling；
- local allowlisted pre/post hooks；
- durable jobs/outbox/idempotency。

### Snapshots/Restore

- metadata index；
- snapshot list/filter；
- on-demand file browser；
- single regular-file download；
- Agent staging restore, overwrite NEVER；
- progress/cancel/audit。

### Maintenance

- check；
- retention dry-run preview；
- safe forget；
- prune；
- stale unlock preview/execute；
- per-Repo serialization/lease；
- unmanaged Snapshot protection。

### Operations

- Dashboard / Attention Queue；
- Agent Health vs Backup Health；
- Gotify + signed Webhook；
- notification retry/dedupe/resolved；
- Prometheus metrics；
- structured logs；
- immutable/hash-chained audit；
- documented backup/restore of RestFleet control plane。

## 3. V1 Simplifications

- 单租户；
- ADMIN 为唯一必须实现角色；
- 一个中心实例；
- 默认/唯一一 Host 一 Repo；
- OneDrive + rclone crypt 为唯一生产验证后端；
- 模块化单体；
- PostgreSQL job queue，无外部 broker；
- Snapshot entries 按需/cache，不预索引全部文件；
- Restore 只到 staging；
- Agent/Restic 手动升级；
- Hooks 本地 allowlist，不远程下发 shell。

## 4. V1.1 / V1.5

优先候选：

- VIEWER role；
- OIDC（Authentik 等）登录；
- Web 内完整 OneDrive OAuth reauthentication + PKCE；
- Repository password key rotation workflow；
- trust bundle/CA 自动轮换；
- 目录 tar/zip download；
- 更细的 restore include/exclude/overwrite policy；
- Agent package repositories；
- audit external sink；
- provider quota 适配增强。

## 5. V2

- HA Control Plane；
- 多管理员/RBAC/审批流；
- Shared Repository（显式风险与权限模型）；
- 其他 rclone providers；
- Agent/Restic staged auto-update；
- 可选短期 gateway credential；
- break-glass in-place restore；
- SSO/SCIM；
- external KMS/Vault；
- remote log/object-lock audit；
- large-fleet sharding。

## 6. 明确不做

- 任意远程 shell；
- 中心 SSH/root key 管理；
- 备份数据经 Control API/gRPC；
- Agent 持有 rclone/OneDrive/Crypt credentials；
- Agent 执行 forget/prune/check/unlock；
- 把 append-only 宣称为不可读；
- 以 `rclone obscure` 作为 secret encryption；
- Windows/macOS/Kubernetes；
- 文件同步产品、对象浏览器或通用云盘 UI；
- Bare-metal image；
- 未经预览的 destructive maintenance。

## 7. Release Gates

V1 release 前必须：

- 所有 P0/P1 acceptance tests 通过；
- amd64/arm64 Native/Docker smoke tests 通过；
- external security review 或至少完成 threat model checklist；
- append-only deletion/overwrite negative test 通过；
- cross-Host read isolation test 通过；
- center outage local scheduling test 通过；
- secret canary leak test 通过；
- PostgreSQL/master key disaster recovery drill 通过；
- OneDrive token refresh persistence test 通过；
- 文档和示例均无真实 credentials；
- 固定依赖版本、checksums、SBOM 与 release notes。

## 8. 性能边界

V1 测试目标：

- 50 Hosts / 100 Plans；
- 10,000 Snapshots metadata；
- 100 concurrent Agent streams；
- 10 concurrent backups（受 gateway/provider 限制）；
- 4 concurrent central read operations；
- 2 concurrent user downloads；
- 每 Operation 10 MiB retained raw logs；
- 1M snapshot entries 的受控流式扫描测试。

这些是正确性/资源上界测试，不承诺所有 OneDrive 租户都达到相同吞吐。

## 9. Definition of Done

一个 V1 capability 完成需要：

1. Spec/API/proto/schema 已提交；
2. domain logic 与 adapters 分层；
3. unit/integration/E2E 正负测试；
4. metrics/log/audit；
5. secret redaction；
6. cancellation/retry/restart 行为；
7. UI loading/error/stale/empty；
8. operator documentation；
9. 通过对应 acceptance test IDs。

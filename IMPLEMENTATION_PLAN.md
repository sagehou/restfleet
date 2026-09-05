# RestFleet Implementation Plan

## 使用方式

- 每次只实施一个 Milestone；
- 开始前创建/更新对应 issue，列出本节 Acceptance IDs；
- PR 必须说明 API、DB、Agent、Web、Security 与 Tests 的影响；
- 每个 Milestone 结束时 `main` 必须可构建、可测试；
- 不满足前置条件时不得通过临时代码绕过安全不变量。

## M0 — Repository Scaffold

**Status: COMPLETE**

### Goal

建立可持续开发、测试和发布的 monorepo 骨架。

### Deliverables

- Go workspace/module；
- `cmd/restfleet-server`、`cmd/restfleet-gateway`、`cmd/restfleet-agent`；
- `internal/domain` 与 adapters 边界；
- React/TS/Vite `web/`；
- OpenAPI/protobuf 目录与代码生成；
- PostgreSQL migration framework；
- Compose dev stack；
- lint/test/build CI；
- pinned tool/dependency policy；
- secret scanner、SBOM baseline。

### Tests/Exit

- Go/Web unit skeleton、production build；
- amd64/arm64 binaries/images build；
- empty DB migrate；
- no real secret in fixtures；
- `AGENTS.md` enforced in contributor docs。

## M1 — Control Plane Skeleton and Auth

**Status: COMPLETE**

### Goal

可安全启动 Server、初始化 Admin、登录并访问空 Dashboard。

### API/DB

- bootstrap/login/logout/session；
- users/sessions/bootstrap_state；
- RFC 9457 problem、request ID、CSRF、rate limit；
- audit writer/hash chain skeleton；
- `/health/live`、`/health/ready`、metrics。

### Web

- login/bootstrap；
- app shell/navigation；
- empty/error/loading state；
- build/version page。

### Tests/Exit

- AUTH-001–006；
- OBS-005/006 foundation；
- database unavailable readiness；
- canary secret redaction baseline。

## M2 — Host, Enrollment and mTLS

**Status: COMPLETE**

### Goal

创建 Host，用一次性 token 安全接入 Agent。

### Server

- Host CRUD/status；
- enrollment token create/list/revoke；
- internal CA/secret encryption；
- CSR verification/cert issue/revoke；
- Agent gRPC listener + Hello/Welcome。

### Agent

- local Ed25519 generation；
- secure identity files；
- enroll command；
- connection manager/reconnect；
- bbolt initial state。

### Web

- Hosts list/detail；
- Add Host wizard；
- token one-time display；
- Agent fingerprint/status。

### Tests/Exit

- ENR-001–010；
- AGT-001/002/010；
- DEP-001–004 enrollment subset。

## M3 — Heartbeat, Inventory and Desired State

**Status: COMPLETE**

### Goal

可靠展示 Agent health，并版本化下发空/基础配置。

### Protocol

- heartbeat/inventory；
- DesiredStateSnapshot；
- accepted/rejected revision；
- capability/version negotiation；
- bounded offline message storage。

### Server/DB

- agents/inventory/revisions；
- reconciler/outbox；
- online/degraded/offline computation；
- Agent detail/audit/metrics。

### Tests/Exit

- AGT-003/004/009/010；
- Agent envelope 重放/乱序防护（AGT-008 的 log chunk 语义随日志流里程碑完成）；
- OBS-008/009；
- Server restart reconciliation。

## M4 — Storage Credential, Repository and Gateway

**Status: IN PROGRESS** — [#15](https://github.com/sagehou/restfleet/issues/15)

分批交付：第一批凭据导入/metadata/替换/禁用已合并（#16）；第二批中心 tmpfs/runtime/token watcher 与固定版本 rclone 在 #17。第三批接入持久化异步 Test Operation、租约恢复/幂等、token 加密 CAS 与测试状态 UI。Gateway、仓库初始化、凭据轮换和真实 OneDrive refresh 人工验收仍待完成。

### Goal

在中心安全接入 OneDrive+rclone crypt，创建 per-Host append-only Repository。

### Server/Gateway

- envelope secret store；
- StorageCredential import/test/replace；
- rclone config tmpfs materialization + token watcher；
- `restfleet-gateway` supervisor/wrapper；
- append-only/private repo；
- Repo provisioning/init/index smoke test；
- gateway credential rotation protocol。

### Web

- Credential/Repository list/detail；
- import/test/status；
- no secret echo；
- provisioning progress。

### Tests/Exit

- REP-001–012；
- DEP-005/006；
- real OneDrive token refresh MANUAL/secure integration；
- public deletion/overwrite negative suite。

## M5 — Templates, Plans and Local Scheduler

### Goal

创建 Template/Plan，Agent 可在中心离线时本地调度。

### Server/DB/API

- retention/maintenance policy CRUD；
- immutable template revisions；
- Plan/effective config/override；
- apply workflow + optimistic concurrency；
- config validation/capabilities；
- Backup Health initial computation。

### Agent

- atomic desired state application；
- cron/timezone/stable jitter；
- misfire/concurrency/retry metadata；
- deterministic local operation IDs。

### Web

- Template/Plan pages；
- revision diff/update available/batch apply；
- schedule and accepted status。

### Tests/Exit

- SCH-001–006；
- AGT-003–006；
- API-003；
- center control outage 12h schedule test。

## M6 — Backup Execution

### Goal

按计划或 Backup Now 安全运行 Restic，并产生可信 Operation。

### Agent

- Restic argv/files adapter；
- JSONL parser；
- process group/cancel/timeouts；
- source/exclude paths；
- local hook allowlist；
- progress/log/result buffering。

### Server

- durable jobs/dispatcher；
- Operation state machine/events/log chunks；
- scheduled/local result reconciliation；
- retry/cancel/idempotency；
- post-backup snapshot index trigger。

### Web

- Backup Now/confirm；
- Operation list/detail/live stream；
- Retry/Cancel；
- summary/warnings。

### Tests/Exit

- BAK-001–012；
- AGT-006–008；
- API-001/002/006；
- DR-001–003。

## M7 — Dashboard and Health

### Goal

让管理员可靠识别 fleet 风险。

### Server

- Agent Health/Backup Health evaluator；
- next-run/overdue SLA；
- dashboard aggregates/attention；
- cached query performance；
- health transition events。

### Web

- Overview metrics；
- Attention Queue；
- Host/Plan status wording；
- recent Operations；
- stale/collected timestamps。

### Tests/Exit

- OBS-008；
- PERF-001–003；
- ONLINE+OVERDUE 与 OFFLINE+HEALTHY UI cases。

## M8 — Snapshot Index and Browser

### Goal

集中查看 Snapshot metadata 和文件树。

### Server

- `snapshots --json` adapter；
- managed/unmanaged mapping；
- two-pass missing semantics；
- `ls --json` streaming/parser/cache/limits；
- cursor APIs。

### Web

- snapshot list/filter；
- browser/breadcrumbs/table；
- live scan operation/progress；
- large-list virtualization。

### Tests/Exit

- SNP-001–005；
- API-004；
- PERF-005；
- 1M-entry bounded resource test。

## M9 — Single-file Download

### Goal

安全审计地从中心下载单个历史文件。

### Server

- download intent；
- safe snapshot/path validation；
- `restic dump` streaming/cancel；
- concurrency/bandwidth/headers/no-store；
- Operation + Audit fail-closed。

### Web

- regular-file Download；
- progress/result；
- error/expired intent states。

### Tests/Exit

- SNP-006–009；
- OBS-005；
- header/path injection suite。

## M10 — Staging Restore

### Goal

从 Console 触发安全的 Agent staging restore。

### Server/Protocol

- restore preview/job；
- signed/bound dispatch payload；
- target Host/repo validation；
- stale preview；
- progress/cancel/audit。

### Agent

- fixed staging root；
- safe path/open strategy；
- disk check；
- overwrite NEVER；
- partial restore marker。

### Web

- restore wizard/preview/reason/confirm；
- Operation/staging result。

### Tests/Exit

- RST-001–008；
- path/symlink escape tests；
- native/docker restore smoke。

## M11 — Retention and Maintenance

### Goal

集中安全执行 check/forget/prune/unlock。

### Server/Worker

- repository leases/advisory lock；
- retention dry-run preview/hash/stale check；
- Plan-tag scoping；
- check/prune parsers and bounded logs；
- stale unlock preview；
- schedule/retry/recovery；
- Agent protocol rejects maintenance jobs。

### Web

- Repository Maintenance section；
- impact preview/tiered confirmation；
- status/history/action guidance。

### Tests/Exit

- MNT-001–010；
- destructive safety chaos tests；
- no shared/unmanaged deletions。

## M12 — Notifications, Audit Hardening and Release

### Goal

完成可运营、可发布的 V1。

### Deliverables

- Gotify；
- signed Webhook + SSRF protection；
- delivery retry/dedupe/resolved；
- full Audit coverage/hash verification/export；
- diagnostics bundle/redaction scan；
- retention cleanup jobs；
- production Compose/proxy/systemd docs；
- upgrade/rollback/DR runbooks；
- multi-arch signed release、checksums、SBOM/provenance；
- V1 full acceptance report。

### Tests/Exit

- OBS-001–009；
- DR-004–006；
- DEP-001–008；
- PERF-001–005；
- 所有 P0/P1 acceptance；
- 外部/独立 security review findings 关闭或接受风险。

## 横向工作流

每个 Milestone 都必须同步处理：

### Security

- threat delta；
- authz negative tests；
- secret lifecycle/redaction；
- audit event。

### Contracts

- OpenAPI/protobuf first；
- generated types；
- compatibility fixture；
- error codes 文档。

### Reliability

- restart/cancel/retry；
- idempotency；
- resource limits；
- metrics/alerts。

### Documentation

- operator/user docs；
- migration notes；
- examples only synthetic；
- acceptance IDs in PR。

## 首个 Codex 开发指令模板

```text
Implement M0 only from IMPLEMENTATION_PLAN.md.

Read AGENTS.md and all referenced specs first. Do not implement later milestones.
Create the monorepo scaffold, reproducible dev environment, contract/codegen layout,
CI, multi-arch build skeleton, PostgreSQL migration skeleton, and empty Web shell.
Run every relevant test/build locally and report exact results. If a requested
implementation would violate a security invariant or needs an unspecified choice,
stop and surface that conflict instead of guessing.
```

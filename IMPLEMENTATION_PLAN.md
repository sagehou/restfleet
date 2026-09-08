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

分批交付：凭据管理（#16）、中心 tmpfs/runtime/token watcher（#17）、持久化异步 Test Operation/租约/幂等/加密 CAS 与测试 UI（#18）、中心 Restic 初始化适配器及固定二进制离线验证（#19）均已合并。第五批（#20，已合并）交付 Repository 记录事务、per-Host 独立加密凭据、创建/列表/详情 API 与 Web；创建仅得到 PROVISIONING，不代表云端初始化或 Agent 已接受。第七批（#22，已合并）接入初始化 Operation/jobs 与基础状态 UI；Agent ACK、Public Gateway、凭据轮换、完整 provisioning UI 和多后端真实服务人工验收仍待完成。

范围变更：根据用户要求，V1 存储后端扩展为 OneDrive、Google Drive、HTTPS WebDAV + rclone Crypt；第六批（#21，已合并）交付配置/凭据/网络安全与 UI 兼容。第七批（#22，已合并）接入 initialize API、持久化 Operation/jobs、双重租约、中心执行和状态 UI；成功仍为 PROVISIONING，Gateway/Agent ACK 留在后续批次。其他 rclone 后端需显式校验与测试后再加入，不能直接透传配置。

第八批（#23，已合并）实现 Gateway 单会话安全层、临时锁归属验证、内容哈希/路径/方法限制与固定二进制离线验证（ADR-0014，用户已同意正常备份临时锁例外）。本批不开放公网 listener 或会话创建 API；supervisor、持久化准入/审计、Agent 凭据下发/ACK 和离线协调仍待接线，M4 不标完成。

第九批（#24，已合并）接入进程内 Gateway supervisor/router：复用 tmpfs/token watcher，增加有界长会话入口、私有 rclone 进程组、ready 后挂载、互斥/容量和完整退出清理。固定二进制备份读回测试改为经 supervisor 执行；command 公网配置、持久化准入/审计、Agent ACK 和离线协调仍待接入。

第十批（#25，已合并）接入 Agent 仓库凭据交付：复用 outbound mTLS、专用 0600 原子文件、PostgreSQL 交付记录/outbox/审计与精确 ACK；Web 区分交付与确认。只交付本 Host 已初始化仓库的密码，兼容无 capability 的旧 Agent。尚未完成公网 Gateway、持久化 backup admission、密码 overlap/retirement、离线协调或 READY，M4 继续进行。

第十一批（#26，已合并）增加 supervisor 的有界 TLS transport：128 连接、认证前限流、聚合限流审计、传输 deadline 与关闭清理；固定 Restic/rclone 备份读回改走此入口。复用现有组件，不提供缺少持久化准入的公网 command；独立 readiness、持久化准入/审计、会话能力交付、rotation 和离线协调仍待完成。

第十二批实现持久化备份占用原语及中心写入口互斥（ADR-0016 / schema 11）：复用凭据锁、Repository advisory lock、审计/outbox；到期不自动释放占用，可信 owner 确认清理后才释放。尚不接 Gateway 进程/公开申请/会话下发，也不宣称离线授权或 M4 完成。

### Goal

在中心安全接入 OneDrive / Google Drive / HTTPS WebDAV + rclone crypt，创建 per-Host append-only Repository。

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

- REP-001–030；
- DEP-005/006；
- real OneDrive / Google Drive token refresh 与 WebDAV 认证、备份恢复 MANUAL/secure integration；
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

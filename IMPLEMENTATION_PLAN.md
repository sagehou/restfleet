# RestFleet Implementation Plan

## 使用方式

- 每次只实施一个 Milestone；
- 开始前创建/更新对应 issue，列出本节 Acceptance IDs；
- PR 必须说明 API、DB、Agent、Web、Security 与 Tests 的影响；
- 每个 Milestone 结束时 `main` 必须可构建、可测试；
- 不满足前置条件时不得通过临时代码绕过安全不变量；
- 跨里程碑验收 MUST 归属于其实现依赖已具备的阶段；调度组件测试不等于真实备份成功，移动验收归属 MUST 保留原 Acceptance ID、要求及 V1 发布门禁；
- 当前计划记录能力、剩余交付与退出条件；历史批次记录 MUST 与当前状态分开。

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

### 当前交付与收敛要求

基线：已合并 `07a85e3`（#60）。[分批交付历史](docs/development/m4-delivery-history.md) 保存三十批记录；[规格审阅](docs/development/m4-spec-review-2026-10-08.md) 记录依赖和协议缺口。内部组件完成与产品链路完成 MUST 分开描述。

| 能力 | 已有实现 | 尚需交付或验收 |
|---|---|---|
| 存储凭据与仓库 | 三后端显式校验、envelope store、test/init jobs、per-Host Repository、中心隔离初始化 | 真实三后端认证、刷新、重启与备份/恢复证据 |
| Agent 仓库凭据 | outbound mTLS 下发、保护文件原子保存、精确 ACK | 长期身份到单次会话能力的生产申请/交付协议、rotation overlap/retirement |
| Gateway 数据面 | 会话锁归属、append-only、supervisor、TLS、固定引擎备份读回 | 真实会话入口、可部署 daemon command、独立 readiness |
| 离线授权与回写 | 签名授权历史、原占用期限内续期、RFGA、两类可靠加密 Queue/RFGR | 自动可靠续期/吊销分发、离线会话准入与真实 Agent 集成 |
| 启动与退出 | protected source/pin/config、单次材料初始化、跨 UID 隔离、多仓库 Service、取消/join/清零 | 中央可信清理/封存/释放与新运行协调；显式 replay 仅恢复记录 |
| Repository 可用状态 | 初始化与 Agent ACK 独立记录，保持 PROVISIONING | 完整 provisioning/rotation UI 与经过完整条件核验的 READY |

剩余工作 MUST 围绕以下完整流程收敛，复用现有 Service、授权、fence 与回写；内部接缝或测试夹具不能替代产品入口：

1. **身份到会话**：先明确长期 gateway identity、单次能力与可信 Operation 的关系，以及离线准入、受保护交付、重复/取消/失效处理，再连接生产会话入口与 daemon/readiness。协议、信任边界或持久化变更 MUST 同批记录 ADR 和相关规范；禁止将每次定时备份改成必须在线请求 Control API。
2. **运行到恢复**：明确可信中央协调者及证据，串接正常停止、可靠回写、来源封存、占用释放与建立新运行。普通断线、备份失败或授权到期 MUST 保留 Agent 身份与 last-known-good 配置，中心恢复后重新核验并安全恢复，不要求重新 enrollment。占用到期或队列排空 MUST NOT 代替清理证明；不能仅添加 `run` 命令就宣称长期服务可用。
3. **轮换到可用状态**：交付 credential revision/ACK/overlap/retirement、活动会话行为及 provisioning UI，只有完整条件满足才设置 READY。
4. **后端与故障验收**：Actions 经生产入口、保护配置和固定引擎验证备份读回、删除/覆盖拒绝、轮换及停止/恢复；客户端夹具 MUST 明确标识，不能宣称产品 Agent 已完成。真实三后端仍需 MANUAL/secure integration 证据，未提供时对应门禁保持未通过。

签发后最多 12h、原占用不可变且最多 24h、到期不释放 fence 和失败 owner 不复活均继续按已接受 ADR 执行。任意断线后完整可用 12h 与签发后 12h 上限的取舍仍待明确决定；MUST NOT 从继续开发指令推定同意降低 AGT-005 或延长授权。授权续期与占用延长 MUST 区分，完整恢复/下一运行协议不能保持未定义。

M4 MUST 提供 M5/M6 可依赖的 Gateway 协议与离线基础；真正的 Agent 调度执行、离线 Operation 补报与 AGT-005 完整产品集成在 M6 验收，仍是 V1 发布门禁。M4 的测试客户端不替代 M6 的真实 Agent。

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

- REP-001–060；
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
- AGT-003/004；
- API-003；
- 控制面离线时本地调度持久化、触发与去重；只证明调度行为；
- AGT-005/006 的真实备份、离线结果补报及完整 12h 集成由 M6 验收，原要求与 V1 发布门禁保持。

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
- AGT-005–008（包含原 M5 的真实备份与离线 Operation 补报依赖）；
- center control outage 12h scheduled backup test：真实 Agent、Gateway、授权边界、结果持久化与重连 reconciliation；时长合同未明确决定前 AGT-005 保持未完成；
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

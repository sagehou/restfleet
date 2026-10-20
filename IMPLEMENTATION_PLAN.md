# RestFleet Implementation Plan

## 使用方式

- 每次只实施一个 Milestone；
- 开始前创建/更新对应 issue，列出本节 Acceptance IDs；
- PR 必须说明 API、DB、Agent、Web、Security 与 Tests 的影响；
- 每个 Milestone 结束时 `main` 必须可构建、可测试；
- 不满足前置条件时不得通过临时代码绕过安全不变量；
- 跨里程碑验收 MUST 归属于其实现依赖已具备的阶段；调度组件测试不等于真实备份成功，移动验收归属 MUST 保留原 Acceptance ID、要求及 V1 发布门禁；
- 当前计划记录能力、剩余交付与退出条件；历史批次记录 MUST 与当前状态分开。

## 当前执行顺序（2026-10-19）

近期目标是交付可用的 Agent 备份链路，按 **M4 → M5 → M6** 顺序推进，每次只实施一个 Milestone。当前只展开 M4 的三次剩余交付；M5/M6 到开工时再细化，M7–M12 保留既有范围与验收要求，不提前开工或扩展设计。

- 每次交付 MUST 围绕一个可验收流程，开始时列明目标、影响范围与 Acceptance IDs，结束时报告结果、Actions 证据及剩余项；SHOULD 用一个 PR 完成。修复归入对应交付，不另开内部组件阶段。
- MUST 优先连接已有组件。只有现有实现无法满足当前流程的明确要求时，才增加必要接口或状态；新增抽象、ADR 或验收编号本身不构成交付完成。
- 新发现的问题 MUST 说明影响、最小修复与所属交付；不得静默扩大范围。安全不变量及已有验收要求 MUST 保留，协议、信任边界、持久化或 API 变更按仓库规则同批更新规范与 ADR。
- 按用户当前指示，先实现功能，暂不新增或改写测试用例。编译、测试、认证及运行验证 MUST 仅在 GitHub Actions 执行；复用已有质量门禁，新增行为的验收保持待验证，不因源码完成而标记通过。
- 2026-10-19 已完成 M4 第二项剩余功能的源码接入：`gateway-rotate`、`gateway-recover`、Gateway 侧 `provision`/`finalize` 与 ADR-0036 / 09 §7.27；测试策略按用户选择在里程碑结束后统一补齐，当前仅新增元数据/策略边界测试。验收证据、剩余项与接手入口见 [轮换与恢复交接](docs/development/handoff-2026-10-19-rotation.md)。

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

已有凭据导入/测试、三后端校验、per-Host 仓库记录与中心初始化、Agent 凭据交付/ACK、Gateway Service/TLS/会话锁隔离、签名授权和可靠加密回写。后续 MUST 复用这些实现，保持现有安全边界；新仓库仍为 PROVISIONING，不能因组件测试或 Agent ACK 就宣称可备份。

### 配对与认证：复用已有能力（2026-10-14）

用户明确要求采用一次配对、长期密钥身份、可吊销的通信方式。现有 M2 已实现这个流程，M4 MUST 复用，不重做注册或另建 Agent 身份体系：

1. 首次注册使用随机、短期、单次 enrollment token，中心只保存 hash 并原子消费。
2. Agent 首次在本机生成并持久保存 Ed25519 密钥对；私钥 MUST NOT 离开 Host。中心验证 CSR 并绑定 Agent/Host 身份。
3. 后续主动连接使用已有 mTLS，断线后复用本机身份自动重连。长期私钥与证书有效期分开，证书按现有协议自动续期，不要求用户重新配对。
4. 管理员吊销 Agent 身份后，中心拒绝其连接和操作。普通断线、备份失败或授权窗口到期不撤销配对、不删除密钥。

撤回前述新增长期密码 verifier、单次 bearer 凭据配发与候选长期密码切换的推荐方案。数据面目标是复用 Agent 密钥身份认证；Gateway 仍须验证该身份只对应自己的 Repository，按当前备份的非秘密会话标识跟踪临时锁归属，拒绝旧会话、预存和维护锁删除。MUST NOT 把每次备份变成重新配对或在线请求 Control API；备份内容仍直接进入 Gateway。

改为 mTLS 身份与显式会话范围时，MUST 在实现同一批修订 ADR、安全/协议/部署规范及对应契约。负向验收要求保留，测试用例按用户指示暂缓新增；未经 Actions 验证 MUST NOT 声称当前会话锁隔离已验收。现有 Gateway 密码轮换要求随认证方式明确替代，保留原验收编号。Restic 仓库加密密码仍按 Host 独立保存，与通信身份分开；云端秘密仍只在中心。

### 剩余实现的最小处理

2026-10-14 源码进度：已接入复用 Agent mTLS 的 daemon/直接会话入口、非秘密独立会话路径、运行内请求去重、Agent 会话客户端及统一 24h 截止。新 capability 不再下发 Gateway 密码；旧 schema 引用保持兼容。PR #67 原 Actions 的 schema 负向失败已定位，并在统一检查补齐连续迁移校验，未改写测试。该提交 `d8a122a` 的既有 Actions 全部通过（run 38543186889）；新生产 mTLS 链路尚未专门验收；真正 Agent 调度/Restic 执行、中心持续协调/恢复、Repository READY 仍未完成，M4 保持 IN PROGRESS。

- **期限**：在统一会话入口将一次备份截止限制到任务期限、开始后 24h、原占用截止的较早值；授权有效性继续由现有 guard/watchdog 检查。保留默认 72h 的离线窗口，不把它当一次备份的运行时长。
- **停止恢复**：中心复用现有停止/join、可靠回写、来源封存与释放接口，验证本次运行已完成清理后再建立新运行。证据不足时保持占用，由中心受控维护恢复，不建设自动接管框架或离线冷启动。
- **可用状态**：接通初始化、Agent 凭据确认及 Gateway 就绪的真实状态，完整条件满足才设置 READY。身份续期和吊销复用现有 M2；不再预设整套候选密码状态机作为交付前置条件。

### M4 交付与剩余项（2026-10-19）

第一项已有源码及既有 Actions 通过，新增 mTLS 行为专门验收保持待完成。第二项已接入持续授权、正常停止后的清理证明/最终回写/原子封存释放，以及先持久保存固定 ID 再预留占用的准备命令；版本 2 plan 复用于初始化和授权 worker。未确认清理、满/坏队列和刷新不确定时继续保留占用。

此前 `c77daf5` 的 10 项既有 Actions 全部通过（run 38878015773）。PR #67 已于 2026-10-19 合并（`d1589ad`）；轮换/恢复交付经 PR #76 于 2026-10-20 合并（`4ed384e`），该 PR 的 10 项检查全部 success。当前 `main` 尚未部署。现有启动命令已接入显式等待、同 runtime 全局来源核验，以及初始化成功后直接接续原授权 worker；执行交接见 [Gateway 启动交接](docs/gateway-run.md)。

本轮第三项源码接入真实仓库 READY：生产 Gateway 复用原后端 HEAD 和签名加密回写，中心批量核验当前初始化、ACK、身份/交付/配置/授权及小于 10min 的后端观察，页面展示可用或降级；不新增密码、数据库表或任务队列。Agent 只对明确的探测忙碌响应最多在 30s 内以原 operation/原 deadline 重试。源码、契约及部署说明同步更新，既有 Actions 已通过；未新增/改写测试，新增行为专门验收仍待完成，见 ADR-0035。

第二项剩余：完整 fresh runtime 自动预置/期限前换运行、没有终结证明时的受控故障恢复。2026-10-19 已补中心状态机：`gateway-rotate` 在核验当前 binding 与剩余期限后明确撤回当前 runtime，等待签名终结记录和原子释放，再自动准备下一占用、注册下一全局来源、初始化并接续授权 worker；超时/丢失回执保留 fence。`gateway-recover` 仅在原占用到期、无终结记录且来源匹配时，由管理员显式撤回、关闭来源、写 `RECOVERY_CONFIRMED` 审计并释放，不伪造清理记录或恢复旧 owner（ADR-0036 / 09 §7.27）。Gateway UID 侧新增 `provision`/`finalize`：本地生成新 runtime/来源 seed、Queue/state 与 preparation/注册 metadata，再按中心 plan/注册结果生成受保护 service.json；中心不接触 Gateway 私钥，进程仍由部署者启动。源码、契约及部署说明已同步；未新增/改写测试，新增行为专门验收仍待 Actions。第三项源码已复用 M2 自动续证：Agent 每次新会话读取本机当前 identity；现有中心 worker 分发明确吊销，Gateway 原 guard/watchdog 停止使用，不新建身份生命周期。第三项仍需身份续证/吊销与 READY 的专门运行验收；M5–M12 保持既有完整范围。契约与必要实现 MUST 在所属交付内完成，不再追加独立的内部原语阶段；现有绿色门禁不能代替完整备份验收。

2026-10-20 合并状态：第二项剩余的轮换/恢复源码（`gateway-rotate`、`gateway-recover`、`provision`/`finalize`、draft plan）已随 PR #76 进入 `main`（`4ed384e`），三项交付的源码至此全部合并。三项的专门运行验收（生产 mTLS 会话、轮换/恢复状态机、自动续证/吊销、READY 过期降级、真实三后端）仍全部待 Actions，M4 保持 IN PROGRESS。

| 顺序 | 完成交付 | Actions 验收与退出证据 |
|---|---|---|
| 1. 身份到一次备份 | 复用现有 Agent 密钥身份，修订 Gateway 数据认证与当前备份范围的最小契约；连接生产入口、Gateway daemon command、保护配置与独立 readiness，正确限制单次备份期限 | 固定 Restic/rclone 客户端经正式入口完成备份、读回和本会话锁清理；跨 Host、删除/覆盖、旧会话及错误 TLS 被拒绝，重复请求不重复执行。无需重新配对或每次在线请求 Control API；不提前宣称长期运行或 Repository READY |
| 2. 连续运行与停止恢复 | 由可信中央协调者接入既有续期/吊销分发与可靠回写；完成正常/失败停止后的清理证明、来源封存、占用释放及建立新运行，在原占用截止前协调正常换运行 | 客户端在控制 API/DB 失联、Gateway 持续在线时于剩余授权内继续备份，恢复后可靠回写；覆盖授权到期、确认丢失、取消及停止/中心恢复。无清理证明不释放，不因到期、排空或重连复活失败 owner；普通故障保留 Agent 配对与配置 |
| 3. 身份生命周期与仓库可用 | 复用已有自动续证/吊销，完成 Gateway 侧状态应用；在既有仓库页面展示初始化、确认与 Gateway 就绪，仅完整条件满足时设置 READY | 创建仓库→中心初始化→Agent 凭据确认→Gateway 核验的状态准确；经修订的认证契约验证续期、吊销及旧会话拒绝，REP-011 的旧 Gateway 密码轮换要求 MUST 同批明确替代验收且保留原 ID。缺失条件不误报 READY，界面不回显秘密 |

### 范围与退出边界

- 已按用户决定采用 ADR-0032：离线窗口可配置，默认从最后中心签发起 72h，并受原占用截止限制；单次会话仍最多 24h。原占用不延长、到期不释放 fence、失败 owner 不复活。Gateway 不可达则暂停备份并报错，保留配对和配置；MUST NOT 将每次定时备份改成必须在线请求 Control API。
- M4 MUST 提供 M5/M6 可依赖的会话协议与离线基础。M4 客户端夹具 MUST 明确标识；真正的 Agent 调度执行、离线 Operation 补报与 AGT-005/006 完整产品集成在 M6 验收，仍是 V1 发布门禁。
- 本轮 MUST NOT 扩展后端、离线冷启动恢复、新服务依赖、通用编排框架或额外管理界面；只补当前流程必需的入口、协调和既有仓库页面状态。
- REP-016 的真实三后端认证、刷新、备份/索引/恢复及 rclone 兼容性仍在部署后的 GitHub Actions 验证，不作为前续源码交付的阻塞条件。源码交付与部署验收 MUST 分开记录，未验收不得声称相应后端生产验证通过。

三次交付只组织剩余工作，不删减规范或验收要求。每次 MUST 复用已有安全负向套件；当前暂不新增测试用例，不为每个 provider 扩建模拟套件。源码交付、已有 Actions 结果和未验证的新行为 MUST 分开记录。仅有 command、组件测试或队列排空不构成 M4 完成。

### Tests/Exit

- REP-001–060 的适用功能与关键安全边界，复用已有测试；REP-016 按下述部署验收执行；
- DEP-005/006；
- REP-016 的真实 OneDrive / Google Drive 刷新与 WebDAV 认证、备份恢复改为部署后的 Actions 验收；不阻塞前续源码交付；
- public deletion/overwrite negative suite。

## M5 — Templates, Plans and Local Scheduler

**Status: IN PROGRESS** — 模板与 Plan 的建表、不可变 revision、读取/创建/修订 API 已接入（Plan 绑定 Host/Agent/Repository，effective config 由中心合并并存档）；计划状态机（apply/pause/resume）、批量 `apply-to-plans`、模板归档与依赖列表已接入；Web 页面已接入；Agent 本地调度核心（cron/时区/stable jitter/misfire/FORBID、deterministic operation key）已落地为 `internal/agent/schedule`，`DesiredState` 与 Agent 控制协议已支持 `plans[]`（调度子集、参与 config_hash、空数组时哈希不变）；计划变更时会重写 Agent 快照并 bump desired revision（`AGENT_DESIRED_STATE_CHANGED` outbox）；Agent 调度循环属于 M6 执行链；Backup Health 计算待完成。

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
- AGT-005/006 的真实备份、离线结果补报及配置窗口内的完整离线集成由 M6 验收，原要求与 V1 发布门禁保持。

## M6 — Backup Execution

**Status: IN PROGRESS** — Restic argv、JSONL 解析、进程组控制已接入；Agent 本地调度循环、Operation 持久化（bbolt）与 Gateway 会话内执行已接线；中心 `backup_operations` 表、进度/结果接收与 Operations 页面已接入；取消/重试与离线补报保序待完成。

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
- 配置窗口内的控制面离线备份集成（默认 72h，可用受控时钟验证期限）：真实 Agent、Gateway、结果持久化与重连 reconciliation；另验 Gateway 不可达时暂停并报错、保留配对；
- API-001/002/006；
- DR-001–003。

## M7 — Dashboard and Health

**Status: IN PROGRESS** — 计划健康随终态结果更新，仪表盘新增 Plans/Operations/运行中/24h 失败/不健康计划计数。

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

**Status: IN PROGRESS** — 快照索引与列表已合入（#91）；`restic ls --json` 有界扫描、`SNAPSHOT_LIST` 后台任务、目录 API 与浏览页面已接入；大规模虚拟化与 Actions 验收待完成。

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

## 当前 Codex 开发指令模板

```text
Continue M4 delivery 2 (continuous authority delivery and trusted stop recovery).
Read AGENTS.md, the security model, affected subsystem specs and the M4 plan first.
Reuse existing Agent enrollment/mTLS, Service, RFGA/RFGR and PostgreSQL decisions.
Do not add Agent identity, passwords, bearer-token lifecycles or service dependencies.
Preserve exact-decision replay, original deadlines and cleanup-before-release.
Do not reconstruct failed owners or infer cleanup from expiry, receipts or queues.
Finish only the next necessary functional connection; keep later milestones queued.
Update affected specs/ADR with code. Do not add or rewrite test cases yet.
Compile, test, authenticate and validate runtime only in GitHub Actions.
Report actual Actions evidence separately from unverified new behavior.
```

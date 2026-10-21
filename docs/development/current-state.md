# 当前开发状态

更新：2026-10-21。本文档是会话中断或记录丢失后的接手入口；状态以 `main`、PR、GitHub Actions 和本文档为准，不依赖聊天记录。

## 仓库与验证

- `main`：`dc1633f feat(m8): add snapshot entry cursor pagination (#116)`。
- 本地与远端只有 `main`，工作区干净；批次分支均已删除。
- PR #99（P0 修复）、#100（session ID 保持 UUIDv7）、#101（READY 正向/降级测试）、#103（M6 终态/统计）、#104（M5 Plan 语义）、#105（路径/版本/分页）、#107（取消与 dispatch 竞态）、#108（崩溃恢复与 `pending_result_ids`）、#110（调度器持久化/jitter）、#111（M7 健康/next_run）、#112（M8 缓存边界/清理）、#114（M8 周期索引/UNMANAGED）、#116（M8 cursor/continuation）均通过 10 项检查并 squash 合并。
- 编译、测试、认证及运行验证只在 GitHub Actions 执行；开发工作区只做源码/文档编辑和只读检查。

## 里程碑

| 阶段 | 状态 | 说明 |
|---|---|---|
| M0–M3 | COMPLETE | 骨架、认证、Host/enrollment、心跳/库存/desired state |
| M4 | IN PROGRESS | 三项交付源码已合入；READY 正/负向已有 CI 证据；真实三后端、部署态轮换/恢复与 DEP-005/006 按用户决定暂缓（不在 GitHub 保存凭据） |
| M5 | IN PROGRESS | 模板、Plan、状态机、本地调度核心已接入；PENDING_APPLY、状态机绕过、accepted_revision、jitter/DST/持久化错误已修复；SCH/AGT/API 专项测试待补 |
| M6 | IN PROGRESS | 手动与定时上报、终态 guard、取消竞态、崩溃恢复（pending_result_ids/LOST）、调度器持久化已修复；结果/日志背压、`JobLogChunk` 与专项测试待补 |
| M7 | IN PROGRESS | NEVER_RUN/PAUSED/OVERDUE、next_run_at/last_success_at、unhealthy 计数已接入；Attention Queue 与分项 dashboard 指标待补 |
| M8 | IN PROGRESS | 索引/列表/浏览 UI、周期 `snapshots --json` 索引、UNMANAGED/missing marker、cursor/continuation 已接入；cache tree_id 校验与大规模虚拟化待补 |
| M9–M12 | NOT STARTED | 下载、恢复、保留/维护、通知与发布 |

## 已合入

- `#83`–`#93`：模板/Plan、既定调度、Restic runner、Job 协议、本地执行、Operation 持久化、Plan 健康、快照索引/列表、快照浏览、手动备份、取消/重试与结果确认。
- `#99`：定时 operation ID（UUIDv5）与 `SNAPSHOT_LIST` 读锁/事务提交 P0 修复。
- `#100`：Gateway 内部 session ID 与审计绑定保持 UUIDv7，仅请求去重键接受 UUIDv5。
- `#101`：READY 正向与吊销降级集成测试。
- `#103`：M6 终态 guard、拒绝 QUEUED 作业、报告竞态与 Restic summary 统计修复。
- `#104`：M5 只调度 ACTIVE Plan、apply/pause/resume 不新增 revision、accepted_revision 落地。
- `#105`：Restic 路径规范化/换行拒绝、`/version` schema 修复、Operation 分页 cursor 修复。
- `#107`：dispatch 原子 claim、Agent cancel-before-dispatch 集合、`CANCEL_REQUESTED` 未启动 closure，关闭取消与执行竞态。
- `#108`：Hello `pending_result_ids`、Agent 启动 RUNNING→LOST 恢复、Server `boot_changed_at` grace 后旧 boot LOST 对账；`JobAccepted` 记录 `started_at` 以保护已接收的取消作业。
- `#110`：调度器从当前时刻计算 jitter、operation/runtime 原子持久化、错误上报、DST spring-forward 测试。
- `#111`：Plan `NEVER_RUN`/`PAUSED`/`OVERDUE` 健康、`next_run_at`/`last_success_at`、heartbeat `next_runs` 与 overdue reconciliation。
- `#112`：快照缓存条目名/路径边界、过期缓存清理。
- `#114`：系统 `SNAPSHOT_INDEX` operation/job、`restic snapshots --json` 索引、managed tag 映射、UNMANAGED snapshot 与 `missing_at` 标记。
- `#116`：`restic ls` 分页 cursor、缓存 `cursor/next_cursor`、API `cursor` 参数与 Snapshot browser “加载更多”。
- M4 源码：`#51`–`#60`、`#67`、`#76`；历史批次与 CI 证据见 [M4 交付历史](m4-delivery-history.md)，运行验收矩阵见 [M4 运行验收记录](m4-acceptance-2026-10-21.md)。

## 已修复 P0（#99）

1. **定时备份 operation ID 已对齐。** Agent 用 UUIDv5 生成确定性键；Server 的进度/结果上报、Agent 的 Gateway 会话入口与 Gateway HTTP 去重路径接受规范 UUIDv5 或 UUIDv7。Host/Agent/Plan/Repository/Gateway 等其他 ID 仍只接受 UUIDv7；Gateway 生成的 session ID 与审计绑定仍为 UUIDv7（#100）。
2. **快照扫描可落库。** `lockCredentialJob` 对 `SNAPSHOT_LIST` 改用 `lockRepositoryForRead`，与 `ClaimCredentialJob` 一致；同时补上 `FinishSnapshotListJob` 缺失的 `tx.Commit`。已初始化/READY 仓库的扫描完成和凭据刷新不再被 `REPOSITORY_UNAVAILABLE` 拒绝。

## P1：正确性与安全

已修复（#103/#104/#105/#107/#108/#110/#111/#112/#114/#116）：终态结果覆盖、拒绝 QUEUED 作业重投、结果统计字段错配、PENDING_APPLY 下发、DRAFT pause→resume 绕过、apply/pause/resume 新增 revision、accepted_revision 恒为 0、路径换行/非规范化、schema version 硬编码、Operation 分页重复、取消与 dispatch 竞态、`pending_result_ids`、Agent 崩溃后 RUNNING→LOST、调度器 jitter/DST/持久化错误、Plan 健康/next_run、缓存条目边界与过期清理、周期快照索引/UNMANAGED/missing marker、cursor/continuation。剩余：

1. **结果与日志背压未实现。** 终态结果在重连后全量重放，缺少发送上限/背压；`JobLogChunk` 与每 Operation 10 MiB 限额、truncation marker、离线日志缓冲尚未接入。
2. **M8 浏览继续。** cache `tree_id` 校验、大规模目录虚拟化与 1M-entry 资源上限验证；filter 仍未实现。
3. **M7 Attention Queue。** 已有健康状态/unhealthy 计数，但 dashboard 没有 Healthy/Failed/Overdue/NeverRun/Paused 分项指标和 Attention Queue。
4. **M9–M12 未开工。** 下载、恢复、保留/维护、通知与发布仍需按规格实现。

## M4 设计/安全不一致

- **旧 Agent 兼容路径仍下发 Gateway password。** `WithAgentCredential` 在 `repository_identity_v1` 缺失时解密并填充 `GatewayPassword`（`internal/server/agent_credentials.go:26-31`）。规范把它限定为 legacy 交付，但 AGENTS 明确 Agent MUST NOT 收到 Gateway password；需要明确替代方案或移除兼容路径。
- **启动 metadata 的 `lifetime_seconds=0` 固定 72h**（`internal/server/gateway_startup_linux.go:48-51`），未使用 `RESTFLEET_GATEWAY_OFFLINE_WINDOW`；配置窗口小于 72h 时，`DecideGatewayAuthorization` 会拒绝（`internal/server/gateway_decisions.go:28-32`），与部署规范 §7.10 的零值语义冲突。
- **占用期限未受离线窗口约束。** `prepareGateway` 限制授权期限（`internal/server/gateway_preparation_linux.go:126`），但 `admission()` 直接使用 `AdmissionLifetimeSeconds`（`internal/server/gateway_preparation_linux.go:81`），占用可长达 30 天而授权只有 72h。
- **READY 已有正向/降级证据（#101）**；部署态轮换/恢复状态机仍缺少完整链路测试，现有测试主要覆盖元数据、拒绝和部分启动路径。

## 文档/契约不一致

- 产品规范定义 `NEVER_RUN`/`PAUSED` 健康状态（`docs/spec/00-product.md:110-111`），DB CHECK 与 OpenAPI 只允许 `UNKNOWN/HEALTHY/OVERDUE/FAILED`（`db/migrations/00018_m5_plans.sql:20`、`api/openapi/restfleet-v1.yaml:1483`）。
- 仪表盘规范要求 healthy/failed/overdue/never-run/paused 与 Attention Queue（`docs/spec/08-web-console.md:48-67`），当前 UI 只有“不健康计划”，且只统计 `FAILED`。
- OpenAPI 公开了尚未实现的 M9–M12 操作类型与 `next_cursor`，生成客户端可能误用。
- `Plans.tsx` 仍提示“等 Agent 调度接入后才真正执行”，与已合入的调度代码不一致。

## Issue 管理

- 现有 issue：`#11`（M2，closed）、`#13`（M3，closed）、`#15`（M4，open）、`#95`（M5）、`#96`（M6）、`#97`（M7）、`#98`（M8）。
- 这不是“一任务一 issue”。#15 与 #95–#98 都是里程碑追踪 issue；实际任务单位是 PR。M4 长期 open 是因为退出条件包含真实后端与部署态运行验收，源码合入不等于完成；其正文已按 2026-10-21 状态重写。
- M1 没有 issue，属于历史不一致；M5–M8 已补齐。M9 开工时再创建 M9 issue。不为每个任务单独建 issue。

## 下一步

1. 继续 M8：周期 `restic snapshots --json` 索引、UNMANAGED/two-pass、cache `tree_id` 校验、cursor/continuation 与大规模虚拟化。
2. 补 M6 结果/日志背压与 `JobLogChunk`，并补 M5/M6 的 SCH/AGT/BAK 专项测试。
3. 补 M7 Attention Queue 与 Healthy/Failed/Overdue/NeverRun/Paused 分项指标。
4. 之后按 M9 → M10 → M11 → M12 推进；不要从总范围删除未开工项。
5. M4 外部验收保持暂缓：不在 GitHub 保存真实云凭据；未来如执行，使用受控部署环境并只回填脱敏证据。M4 保持 IN PROGRESS，不阻塞上述代码推进。

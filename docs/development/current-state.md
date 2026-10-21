# 当前开发状态

更新：2026-10-21。本文档是会话中断或记录丢失后的接手入口；状态以 `main`、PR、GitHub Actions 和本文档为准，不依赖聊天记录。

## 仓库与验证

- `main`：`671c121 fix: harden paths and correct version/pagination (#105)`。
- 本地与远端只有 `main`，工作区干净；所有已合并/前缀分支已删除。
- PR #99（P0 修复）、#100（session ID 保持 UUIDv7）、#101（READY 正向/降级测试）、#103（M6 终态/统计）、#104（M5 Plan 语义）、#105（路径/版本/分页）均通过 10 项检查并 squash 合并。
- 编译、测试、认证及运行验证只在 GitHub Actions 执行；开发工作区只做源码/文档编辑和只读检查。

## 里程碑

| 阶段 | 状态 | 说明 |
|---|---|---|
| M0–M3 | COMPLETE | 骨架、认证、Host/enrollment、心跳/库存/desired state |
| M4 | IN PROGRESS | 三项交付源码已合入；READY 正/负向已有 CI 证据；真实三后端、部署态轮换/恢复与 DEP-005/006 按用户决定暂缓（不在 GitHub 保存凭据） |
| M5 | IN PROGRESS | 模板、Plan、状态机、本地调度核心已接入；定时 operation ID、PENDING_APPLY 下发、状态机绕过与 accepted_revision 已修复；调度器 jitter/DST 与专项测试待补 |
| M6 | IN PROGRESS | 手动与定时上报、终态 guard、拒绝闭环、summary 统计已修复；取消在途作业仍可能执行，崩溃恢复/日志流与专项测试待补 |
| M7 | IN PROGRESS | 终态健康与仪表盘计数；OVERDUE/NEVER_RUN/PAUSED、Attention Queue 未实现 |
| M8 | IN PROGRESS | 索引/列表/浏览 UI；扫描落库已修复；周期索引/UNMANAGED/缓存清理未实现 |
| M9–M12 | NOT STARTED | 下载、恢复、保留/维护、通知与发布 |

## 已合入

- `#83`–`#93`：模板/Plan、既定调度、Restic runner、Job 协议、本地执行、Operation 持久化、Plan 健康、快照索引/列表、快照浏览、手动备份、取消/重试与结果确认。
- `#99`：定时 operation ID（UUIDv5）与 `SNAPSHOT_LIST` 读锁/事务提交 P0 修复。
- `#100`：Gateway 内部 session ID 与审计绑定保持 UUIDv7，仅请求去重键接受 UUIDv5。
- `#101`：READY 正向与吊销降级集成测试。
- `#103`：M6 终态 guard、拒绝 QUEUED 作业、报告竞态与 Restic summary 统计修复。
- `#104`：M5 只调度 ACTIVE Plan、apply/pause/resume 不新增 revision、accepted_revision 落地。
- `#105`：Restic 路径规范化/换行拒绝、`/version` schema 修复、Operation 分页 cursor 修复。
- M4 源码：`#51`–`#60`、`#67`、`#76`；历史批次与 CI 证据见 [M4 交付历史](m4-delivery-history.md)，运行验收矩阵见 [M4 运行验收记录](m4-acceptance-2026-10-21.md)。

## 已修复 P0（#99）

1. **定时备份 operation ID 已对齐。** Agent 用 UUIDv5 生成确定性键；Server 的进度/结果上报、Agent 的 Gateway 会话入口与 Gateway HTTP 去重路径接受规范 UUIDv5 或 UUIDv7。Host/Agent/Plan/Repository/Gateway 等其他 ID 仍只接受 UUIDv7；Gateway 生成的 session ID 与审计绑定仍为 UUIDv7（#100）。
2. **快照扫描可落库。** `lockCredentialJob` 对 `SNAPSHOT_LIST` 改用 `lockRepositoryForRead`，与 `ClaimCredentialJob` 一致；同时补上 `FinishSnapshotListJob` 缺失的 `tx.Commit`。已初始化/READY 仓库的扫描完成和凭据刷新不再被 `REPOSITORY_UNAVAILABLE` 拒绝。

## P1：正确性与安全

已修复（#103/#104/#105）：终态结果覆盖、拒绝 QUEUED 作业重投、结果统计字段错配、PENDING_APPLY 下发、DRAFT pause→resume 绕过、apply/pause/resume 新增 revision、accepted_revision 恒为 0、路径换行/非规范化、schema version 硬编码、Operation 分页重复。剩余：

1. **取消在途 QUEUED 作业仍可能执行。** 终态 guard 已阻止迟到结果覆盖，但取消发生在 `JobDispatch` 已发出、`JobAccepted` 未到达的窗口时，Agent 仍可能启动 Restic。需要把 dispatch/cancel 的竞态在协议层闭合，而不是只靠终态保护。
2. **调度器边界。** `NextRun` 从已加 jitter 的时刻继续计算（`internal/agent/scheduler.go:96-98`），jitter 大于 cron 间隔时会跳槽；分钟扫描未处理 DST（`internal/agent/schedule/schedule.go:82-91`）；持久化错误被丢弃（`scheduler.go:90,104,128,130,145`）；重试/永久失败语义未实现。
3. **离线/崩溃恢复未闭合。** Agent `Hello` 未发送 `pending_result_ids`（`internal/agent/client_stream.go:56-67`）；没有 `LOST` 对账，崩溃后 `RUNNING` 记录会永久阻塞 `FORBID`；没有结果/日志背压，也没有 `JobLogChunk`。
4. **M8 缓存与截断。** 缓存键缺少 `tree_id`，过期行不清理；条目名/路径无长度上限；截断按全部后代计数，深目录可能提前耗尽预算；截断页没有 continuation；`SnapshotList.next_cursor` 仍未赋值。

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

1. 修复 P1：终态 guard/取消/拒绝、结果统计、Plan 调度语义、路径校验、崩溃恢复与 M8 缓存；为每个状态转换和负向边界补 Actions 测试。
2. M4 外部验收按用户 2026-10-21 决定暂缓：不在 GitHub 保存真实云凭据；未来如执行，使用受控部署环境并只回填脱敏证据。M4 保持 IN PROGRESS，但不再阻塞 M5–M8 代码推进。
3. M4 兼容路径与离线窗口不一致需要设计决定，不能静默保留。
4. 之后按 M9 → M10 → M11 → M12 推进；不要从总范围删除未开工项。

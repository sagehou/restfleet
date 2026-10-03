# 2026-10-03 开发进度与方向复核

评估基线为已合并 `main`：`da0d92f`。此前未合并、未推送的开发分支与本地改动已丢弃；不计入完成进度。Dependabot 的 #38–41、#43–50 共 12 个 PR 已合并，#42 被后续更新取代关闭，复核时没有开放的依赖 PR。React/React DOM 已配对升级并分组更新，依赖主分支 CI 八项检查通过。

| 范围 | 当前证据与状态 |
|---|---|
| M0–M3 | 计划标记完成，代码已合并：骨架/CI、认证、Host/enrollment/mTLS、心跳/inventory/基础 DesiredState。 |
| M4 | 进行中。存储凭据、三后端显式校验、初始化任务、Agent 仓库凭据 ACK、安全 Gateway 会话/supervisor/TLS、占用 fence、在线材料/审计、签名授权历史已合并。 |
| 本批 M4 开发分支 | 加密有界待回写区、独立来源认证、可靠精确确认、中央有序幂等审计/token CAS、内部 Unix 回放 listener 与文件密钥加载；schema 13 / ADR-0020 / REP-031–034。进入完成进度前仍需审查合并。 |
| 后续本地 M4 开发 | 将签名授权/待回写接入 supervisor 的唯一 owner 生命周期；连续备份维持 token revision、逐次转发核验、吊销/到期/容量取消及 join；ADR-0021 / REP-035–037。固定真实二进制走在线与本地签名两条测试路径，生产交付通道仍未完成，不计入已合并进度。 |
| 最新本地 M4 开发 | 来源签名挑战、中心加密材料与单次 Unix owner 安装；schema 14 / ADR-0022 / REP-038–040。事务提交后不重发，回执未知保留 fence，Close/失败清理；真实 DB 与引擎覆盖内部通道。生产 pin/source 配置与 UID 隔离、运行协调、command 尚未完成，不计入已合并进度。 |
| 跨 UID 接线（源码待 CI） | 显式共享组、精确 peer UID、0710/0660 socket 策略及 Server replay 配置；ADR-0023 / REP-041–042。新增 Actions 非 root 双进程及同组第三 UID 负向验收，尚未运行，不计入验收完成。生产身份/启动协调仍待接线。 |
| M5–M6 | Template/Plan、本地 scheduler 和实际 Agent backup 执行主链路尚未交付；基础消息/Operation/jobs 能复用，不能据此算作备份可用。 |
| M7–M11 | Backup Health、Snapshot browser、下载、staging restore、中央 retention/maintenance 的完整业务链路尚未交付。 |
| M12 | CI、日志脱敏和审计链有基础；通知、诊断、灾备/升级和正式发布的全量验收尚未完成。 |

当前不能作为生产备份系统发布。`cmd/restfleet-gateway` 仍只输出版本；仓库初始化成功或 Agent ACK 保持 `PROVISIONING`，不是 `READY`。内部回放 listener 和单次材料初始化接缝不能替代生产协调、受保护信任配置和数据面启动。模拟与离线二进制测试也不能替代 REP-016 三个真实后端的人工证据。

开发顺序 MUST 继续遵守一次一个 milestone：

1. 在 M4 完成授权/材料与会话能力交付，串接现有 supervisor、连续 fence 和可靠回写；由可信协调器负责吊销投递、退出、排空与 owner 恢复。
2. 接入 Gateway command 和独立 readiness、凭据 rotation/ACK/overlap、Web provisioning 状态；只在端到端条件全部满足后转 `READY`。
3. 完成 M4 安全和真实后端验收，再进入 M5 的配置/调度和 M6 的 Agent 备份执行；之后依赖实际备份推进健康、快照、下载、恢复和维护。

AGT-005 仍未通过。实验 MUST 分别记录中心签发、续期、失联和备份时刻：签发后最多 12h 的授权不等于任意失联时刻后仍剩完整 12h。发现要求冲突时必须报告取舍并取得明确决定，不能用重放或重置接收时间延寿。

本批验证使用本机临时 PostgreSQL 16 和从 CI 固定 OCI digest 提取的 Restic 0.19.1/rclone 1.75.1。验收重点是三后端回放、确认丢失、并发重复/跨绑定/跳序、非 token 配置和 CAS 拒绝、审计回滚、容量/fsync 故障、重启只回放、多占用隔离，以及现有真实 TLS 备份/读回回归；没有使用真实云凭据。最终检查结果以关联 PR 的当前提交 CI 为准。

验证环境调整：根据用户要求，后续编译、测试、认证及其他运行验证 MUST 仅在 GitHub Actions 执行，开发工作区仅用于源码/文档编辑和只读检查。本任务临时 PostgreSQL 已停止，数据库、引擎、下载工具链、编译缓存和构建产物已清理；上述本地结果保留为历史证据。CI 的 cross-build 同时直接编译 Gateway/queue/security 包，覆盖尚未接入 command 的生产代码；新变更仍须以 Actions 结果验证。

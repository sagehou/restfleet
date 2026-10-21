# M4 运行验收记录（2026-10-21）

状态：部分完成。源码交付已合入；自动化证据已补齐一部分，真实云后端与部署态轮换/恢复仍被外部环境阻塞。M4 保持 IN PROGRESS。

## 结论

- 三项交付源码已合入 `main`（#51–#60、#67、#76）。
- 定时备份与快照扫描的两个 P0 已修复（#99），并有 Server/Agent/Gateway 校验、扫描完成集成测试。
- READY 读模型已有正向与吊销降级测试（#101）。
- 真实 OneDrive / Google Drive / HTTPS WebDAV、部署态轮换/恢复、DEP-005/006 尚未执行；仓库没有配置相关 secrets，当前环境无法提供真实云凭据或受信部署。

## 自动化证据

| 验收项 | 证据 | 状态 |
|---|---|---|
| 生产 Gateway mTLS 会话、真实 Restic/rclone 备份读回、锁清理 | `TestPinnedGatewayBackupAndReadback`（online / signed-local / service）在 CI 使用固定版本 Restic 0.19.1 与 rclone 1.75.1；本地 fixture 替代云后端 | 通过（本地后端） |
| 跨 UID Service 生命周期与 root 拒绝 | `gateway-isolation` job：`TestCrossUIDServiceLifecycle`、`TestServiceStartupRejectsVolatileQueueAndBadTLSWithoutBindingChannels` | 通过 |
| 授权续期、重放、吊销与禁用身份 | `TestCentralAuthorityDeliveryRenewsReplaysAndRevokesDisabledIdentity`、`TestGatewayDecisionProvidersRenewalReplayAndWithdrawal` | 通过 |
| 幂等回写、确认丢失、Queue 重放 | `TestGatewayPendingProviderPipelineAndLostCommitAcknowledgement`、`TestGatewayPendingRejectsBindingsOrderConflictsAndConcurrentDuplicates` | 通过 |
| READY 正向与降级 | `TestRepositoryReadinessFromVerifiedGatewayObservation`（本批新增） | 通过 |
| Agent 凭据交付/ACK、配置指纹与隔离 | `TestAgentCredentialDeliveryDurableACKAndProviderIsolation` | 通过 |
| 启动交接、来源注册与材料初始化 | `TestGatewayStartupCoordinatesFirstGrantSourceAndMaterial` 等 | 通过 |
| 轮换/恢复元数据与拒绝路径 | `TestGatewayRotationProtectedCanonicalMetadata`、`TestGatewayRecoveryProtectedCanonicalMetadata` | 通过（仅边界） |

## 未完成

1. **真实三后端（REP-016）**：需要 OneDrive / Google Drive OAuth 与 WebDAV 测试端点和凭据。仓库 `gh secret list` 为空，未执行。
2. **部署态轮换/恢复状态机**：需要受信 Gateway 运行时、受保护 socket/UID 与真实终结记录；现有测试只覆盖元数据、拒绝和部分启动链路。
3. **DEP-005/006**：需要实际 Compose/主机部署与外部扫描；当前 CI 只校验 Compose 配置和跨 UID 隔离。
4. **REP-011 旧 Gateway password 兼容路径**：仍需明确移除或安全评审，不能据此声称生产认证只使用 mTLS。

## 完成真实后端验收的最低步骤

1. 在仓库或受控环境中配置一次性测试凭据（不得写入代码、日志或 PR）。
2. 部署 Server + Gateway + 一个 Agent，使用固定版本 Restic/rclone，执行创建仓库 → Agent ACK → Gateway 探测 → 备份 → 读回 → 停止/轮换/恢复。
3. 记录每步的 Actions run、审计事件、授权截止时间和清理证明；失败时保留 fence，不得以重试或清空目录绕过。
4. 将证据链接追加到 #15；只有全部通过后才关闭 M4。

在外部凭据和受信部署就绪前，M4 不能被标记 COMPLETE。

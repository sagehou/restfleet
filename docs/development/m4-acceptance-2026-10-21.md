# M4 运行验收记录（2026-10-21）

状态：自动化部分完成；REP-016、部署态轮换/恢复与 DEP-005/006 按用户 2026-10-21 决定暂缓，不在 GitHub 保存真实云凭据。M4 保持 IN PROGRESS，不阻塞后续代码推进。

## 结论

- 三项交付源码已合入 `main`（#51–#60、#67、#76）。
- 定时备份与快照扫描的两个 P0 已修复（#99），并有 Server/Agent/Gateway 校验、扫描完成集成测试。
- READY 读模型已有正向与吊销降级测试（#101）。
- 真实 OneDrive / Google Drive / HTTPS WebDAV、部署态轮换/恢复、DEP-005/006 按用户决定暂缓；GitHub Actions 继续只运行固定二进制的本地 fixture，不接收真实云凭据。

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

## 暂缓项

1. **真实三后端（REP-016）**：按用户决定暂缓。真实 OneDrive / Google Drive OAuth 与 WebDAV 测试端点和凭据不得保存到 GitHub 仓库或 Actions secrets。
2. **部署态轮换/恢复状态机**：按用户决定暂缓；需要受信 Gateway 运行时、受保护 socket/UID 与真实终结记录。现有测试只覆盖元数据、拒绝和部分启动链路。
3. **DEP-005/006**：按用户决定暂缓；当前 CI 只校验 Compose 配置和跨 UID 隔离。
4. **REP-011 旧 Gateway password 兼容路径**：仍需明确移除或安全评审，不能据此声称生产认证只使用 mTLS。

## 后续执行原则（暂缓）

- 不在 GitHub 保存真实云凭据；未来如执行 REP-016，使用受控部署环境并只回填脱敏证据。
- 失败时保留 fence，不得以重试或清空目录绕过。
- 在真实后端和受信部署未验证前，不声称生产后端通过，也不关闭 M4。

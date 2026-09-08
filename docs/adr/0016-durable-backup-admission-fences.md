# 备份占用到期与后端清理分离

schema 11 使用独立 `gateway_backup_admissions` 保存有界备份占用及释放历史，复用现有 Repository advisory lock、StorageCredential 行锁、审计与 outbox。它不是 Backup Operation、每次 Restic 备份会话或 Agent ACK，不创建虚假的任务状态。备份占用与 `repository_leases` 共同构成中心写入准入边界；原有短时 worker 租约和数据面占用必须分别验证，不能用一个会自动过期的行表示两种事实。

选择保守释放：有效期最多 24h，到期 MUST 禁止新的使用，但未释放占用 MUST 继续阻止同 Host/Repository/Gateway identity/StorageCredential 的另一个占用，以及中心测试、初始化、维护和凭据替换。DB 时间到期不证明旧 Gateway、网络请求或子进程已经停止，因此拒绝“到期即自动回收并运行维护”的方案。只有可信中心 owner 在撤销并等待所有会话、请求、子进程和刷新持久化清理完成后，才 MAY 提交精确 ID/owner 的释放；错误 owner、重复准入 ID 改参、过期重开和重用已释放 ID MUST 拒绝。释放可在撤销/到期后进行，保持幂等，不删除历史。

准入 MUST 从认证 Agent 查找唯一同 Host 已初始化仓库，验证当前精确的仓库凭据 ACK、公开 Gateway 配置指纹和状态；不接受调用方指定 Host、Repository 或云端配置。记录只保存 UUID、秘密引用和时间，不解密或下发任何秘密。首次准入/释放与脱敏 outbox、审计 MUST 同事务；并发重放只返回同一截止时间。凭据禁用保持可用以撤销后续使用，但不能代替清理确认。

代价是崩溃或释放确认丢失后可能保持阻塞；后续 Gateway owner 恢复必须先确认进程清理，再重试释放，不允许普通过期 reaper 或 Agent 自报释放。此批只提供持久化原语及中心写入口互斥，不接公网会话申请、Gateway 进程控制、秘密 materialization、轮换或离线许可。离线 12h 的调度、审计缓冲和 OAuth refresh 安全持久化仍按 AGT-005 验收；不把在线 Check 的 DB 依赖偷换成最终离线策略，也不宣称 Repository READY。

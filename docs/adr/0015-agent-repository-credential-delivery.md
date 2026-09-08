# Agent 仓库凭据独立下发与持久化 ACK

复用既有 outbound mTLS stream，以 `repository_credentials_v1` capability 协商可选的 `CredentialRevision` / `CredentialRevisionAccepted`，不将秘密混入 DesiredState、浏览器 API、通用 outbox 或日志。PostgreSQL 保存每 Agent 当前交付的 UUID、单调 revision、仓库秘密引用及公开 Gateway 配置指纹；首次交付和变更与脱敏 outbox 同事务，只有精确 ACK 才确认，断线后由持久记录重发。这个选择保留现有 1.0 / 0.9 协议兼容，同时避免配置 ACK 误充当秘密 ACK。

中心 MUST 从已验证证书的 Agent 查找同 Host 的唯一已初始化仓库，事务检查 Host/Agent/Repository/StorageCredential 状态并审计后，才解密两份仓库秘密。Agent MUST 在自己的 0700 state 目录中原子写入、fsync 专用 0600 凭据文件后 ACK；bbolt 和普通配置消息不保存这些明文。重复交付幂等，回滚、同版本冲突、跨 Host 或跨 Repository 替换 MUST 拒绝。重连强制重发当前版本，以恢复 ACK 丢失和本地文件缺失；在线心跳只重发未确认版本。

本批仅交付初始及版本化凭据保存/确认，不实现旧密码 retirement、Restic key 轮换或 Gateway 会话授权。持久 gateway password 不是 ADR-0014 的临时会话能力，MUST NOT 用凭据 ACK 代替备份准入、备份成功或 Repository READY。公网 listener、持久化 backup lease 与控制面离线协调仍须后续接入；已保存凭据不会因控制连接断开而删除。

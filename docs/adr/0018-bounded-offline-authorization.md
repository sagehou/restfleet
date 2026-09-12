# ADR-0018: Bounded Offline Gateway Authorization & Write-Back Area

## 状态

已接受

## 上下文

ADR-0017 记录了独立 Gateway 使用中心签发的有界离线授权方案。本 ADR 描述实现细节：授权签发/续期/验证的数据库 schema、写回区的持久化机制和生产接线约束。

## 决策

### 1. 授权 Schema（migration 00012）

`offline_authorizations` 表独立于 schema 11 的 `gateway_backup_admissions`，因为：
- 后者的 `expires_at` CHECK 约束不可更新（`created_at + interval '24 hours'`），不支持续期；
- 离线授权生命周期（签发/续期/撤销/禁用）与在线占用（保留/释放）不同；
- 两者可并存：在线备份仍需 `gateway_backup_admissions`，离线授权是额外的持久化 fence。

每条授权绑定：gateway_instance_id、agent、host、repository、gateway identity、storage_credential、delivery ID 和 configuration_hash。partial unique index 确保同一 host/repo/credential 同时只有一个活跃授权。

`authorization_sequence` 单调递增，用于 Gateway 的防回滚检测。续期必须提供当前 sequence 才能获得下一个。

### 2. 写回区（migration 00013）

`writeback_records` 表存储 Gateway 在离线期间产生的审计事件、token 刷新记录和占用变更。记录在提交前由 Gateway 加密，服务器以 opaque bytea 存储。

每条记录携带：
- 可信 owner/instance 绑定
- 唯一 ID
- 有序 sequence（per gateway_instance_id 唯一）
- SHA-256 checksum（明文加密前）

中央服务器：
- 拒绝 sequence 回滚或跳序
- 拒绝跨 instance 的 sequence 冲突
- 在 DB 提交后才确认记录
- 确认丢失时支持幂等重放

### 3. 受保护通道

Gateway 通过已有的中心材料通道（`BackupAdmissionMaterial`）获取解密后的 rclone 配置。离线授权的 HMAC 签名密钥通过同一通道的 tmpfs 材料化交付，不自选公钥。

### 4. 生产接线约束

离线授权不是生产可用的公网 Gateway 部署。以下仍未完成：
- 独立 `restfleet-gateway` command 的受保护配置加载
- 独立 readiness（区分控制面不可达、授权不足、写回区容量、恢复阻塞）
- 离线备份的会话能力交付
- AGT-005 的12h 离线备份验收

## 约束

- 授权最长12h（从签发时计算），续期不延长总时限
- 控制面失联时撤销/禁用最多延迟12h 生效
- 写回区有16 MiB / 4096 条硬上限
- 加密封装和来源认证必须独立验证
- 明文配置仍只允许受限 tmpfs
- 不得以离线为由保存普通磁盘明文或跳过 token-only 校验

## 关联

- ADR-0014: Gateway-owned lock cleanup
- ADR-0016: Durable backup admission fences
- ADR-0017: Bounded offline Gateway authority
- 部署规范 09 §7.10

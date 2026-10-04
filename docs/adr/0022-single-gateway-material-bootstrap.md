# Gateway 单次挑战绑定的加密材料初始化

## 背景

ADR-0021 的本地 owner 需要可信来源、当前云端配置和有效授权。直接传递结构体不足以证明独立进程接收者，重复材料交付也可能重置已刷新配置。复用现有来源、签名授权、Unix 帧和 supervisor，实现内部初始化接缝；生产运行协调器尚未接线。

## 决定

- Gateway MUST 在本地生成独立来源私钥和临时 X25519 接收私钥；中心 MUST 从可信协调器取得完整 binding 与已注册来源公钥。Agent、回放 payload 或待交付材料 MUST NOT 自行提供验证信任。中心签名公钥 MUST 经独立受保护配置固定。
- Gateway MUST 对 fresh nonce、临时接收公钥和完整 binding 签名。中心验签后，在同一事务重验当前身份、精确 ACK、连续占用、未封存来源、未开始回写、初始秘密 revision 和最新有效普通授权，再追加单次交付意图及秘密访问审计。审计等待后 MUST 复查 DB 期限，提交前失败全部回滚。
- schema 14 的 `gateway_material_deliveries` 每个 admission 只允许一条意图。中心 MUST 提交意图后才短时解密并加密交付；之后任意错误、精确重试、新挑战、无回执或排空均 MUST NOT 再发材料。可用性代价是提交后失败需可信协调恢复，不能重新构造 owner 或自动释放 fence。
- 云端 config、remote、初始 revision、占用期限、来源公钥、中心待回写接收公钥及最新签名授权 MUST 通过挑战绑定的匿名公钥加密和独立中心签名交付。Gateway MUST NOT 获得 DB 凭据、master key、中心签名/待回写解密私钥、Restic password 或 Agent session capability。
- 接收者 MUST 单次消费挑战，核验内外挑战、来源、签名、配置 hash 和有效授权，再构造 fresh Queue 与唯一 owner。借用 config 在回调后清零；安装回调 MUST 返回取消并 join owner 的 rollback，发送回执失败时执行。Close MUST 消费未使用接收者，或取消并等待正在执行的交换，最后清零临时私钥；不能从被等待的回调调用 Close。
- 来源签名回执 MUST 绑定挑战及精确 encrypted wire hash。它仅确认初始化，不是 Agent ACK、READY、备份成功、进程清理、中心封存或 fence release。未知回执结果保留占用和 Queue，禁止自动重试初始化。

协议编码、限制和生命周期见 [09 §7.15](../spec/09-deployment.md)。升级 MUST 停止旧 writer、迁移至 schema 14；应用角色仅 SELECT/INSERT，已有意图时 Down MUST 拒绝删除。

## 证据与边界

REP-038–040 覆盖三后端真实数据库事务、来源/状态/审计失败、并发单次提交、Unix 认证/帧/密文负向测试、取消和回执丢失 rollback，以及固定 Restic/rclone 的 TLS 连续备份与读回。

当前 socket 复用服务所有的 0700 目录和 0600 socket，正向测试双方为同 UID；检查预期 UID 不证明生产 UID 隔离。独立 pin/source 配置、启动器、非 root 跨 UID 拓扑、续期/吊销投递、全局离线审计、Agent 能力交付、可信清理/恢复、command/readiness、rotation/READY、真实云端和 AGT-005 仍待完成。本批没有新增公共 HTTP/gRPC 路由或启动配置。

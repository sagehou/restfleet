# ADR 0010 — 仓库初始化使用中心私有 Unix socket

状态：Accepted，M4。

中心初始化由 Server 直接管理 Restic 与 rclone 两个进程，通过受限 tmpfs 中的 Unix socket 连接；不使用 Restic 自动派生 rclone 的 stdio 后端。固定版本 Restic 会为 rclone 创建独立进程组，仅取消 Restic 的进程组不足以保证后端退出；分别管理可明确取消、回收两者，再完成 token final sync 和明文清理。

该 socket 属于 Maintenance Plane，不是 Public Gateway：MUST 位于服务 UID 独占的 0700 tmpfs 子目录，socket/password/config MUST 为 0600，MUST NOT 绑定 TCP、共享到 Agent 或映射给反向代理。这里以本机文件权限隔离，不涉及放宽网络 TLS；任何网络化管理 listener 仍须独立身份与 TLS/mTLS。公开 Data Plane 的 append-only 约束不变。

调用方 MUST 在解密与执行前取得 Repository 租约、排除活跃备份并写审计；适配器不代替持久化任务或 READY 状态机。重试 MUST 复用已持久化的 UUID、密码和已知 Restic ID：先读取 config，仅明确缺失且尚无已知 ID 时 init，随后验证 format v2、完整 ID 和空 snapshots。错误、部分退出、非空仓库或身份不符 MUST 失败；MUST NOT 删除半初始化仓库、重新生成密码或自动 unlock。

本批只交付初始化执行边界和离线真实二进制验证；Repository API/jobs/Agent ACK、Public Gateway 与真实 OneDrive 验收另行接线。镜像及 CI 使用相同 digest-pinned Restic 0.19.1、rclone 1.75.1；离线验证替换云端为测试本地目录，MUST NOT 声称已验证真实 OneDrive 权限/刷新。

依据：[Restic 0.19.1 后端进程组](https://github.com/restic/restic/blob/v0.19.1/internal/terminal/foreground_unix.go)、[Restic REST Unix socket 支持](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#rest-server)。

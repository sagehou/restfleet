# ADR 0008 — 中心 rclone 运行时与 token watcher

- 状态：Accepted
- 所属里程碑：M4

## 决策

中心 rclone adapter MUST 使用固定 argv 的只读 `lsjson <crypt-remote>: --stat` 连接测试。成功只表示 Crypt 根目录可读取；MUST NOT 将其解释为仓库初始化成功或云端写权限验证。根目录不存在时测试失败，创建动作属于后续 Repository provisioning。

运行目录 MUST 是显式配置、已存在、当前服务 UID 拥有的 canonical `0700` tmpfs 目录；配置 MUST 是 `0600` 普通文件，MUST NOT 接受 symlink、hard link、FIFO 或不受限权限。每个 runtime MUST 独占目录锁，取得锁后才清理自身命名范围的 stale 子目录。不同角色的 runtime SHOULD 使用独立目录。

子进程 MUST NOT 继承父进程环境，只传入固定 PATH/LANG 和当前私有 TMPDIR；MUST 显式传入配置路径，MUST NOT 使用 shell、禁用 TLS 验证或把 secret 放进 argv。连接测试最长一分钟，取消或超时 MUST 终止整个 process group。

watcher 每 250 ms 检查配置，并在进程退出后 final sync；MUST 在进程运行中持久化刷新，不能仅依赖正常退出。rclone 保存配置时会先 rename 旧文件再 rename 新文件，因此只对该短暂 ENOENT 间隙最多重试一秒；不安全 inode MUST 立即拒绝。变化只允许 OAuth token 字段，MUST NOT 改变云端路径、Crypt key 或 OAuth client identity。调用方 MUST 在回调中加密、进行 secret revision CAS 和审计，并遵守取消 context；回写失败 MUST 停止子进程，MUST NOT 报告成功。回调借用的 plaintext byte slice 返回后被清零。

原始 stderr MUST 丢弃；stdout MUST 限制为 64 KiB，仅解析必需的 IsDir 字段并忽略未知字段。API、日志和 audit 只能使用固定错误类别，MUST NOT 暴露 provider 输出、callback error 或进程启动错误。

## 验证与交付边界

- fake executable 覆盖运行中多次刷新、回写失败、未知退出码、损坏/超量输出、配置目标篡改、环境隔离和进程组取消。
- tmpfs 测试覆盖权限、symlink/hard link/FIFO、目录独占与 stale cleanup。
- GitHub CI 使用与中心镜像相同的 digest-pinned rclone 1.75.1，离线验证 config/listremotes/lsjson 参数和 JSON 契约。
- 本批 adapter 尚不提供 HTTP Test Operation；数据库任务队列、加密 refresh CAS、状态/UI 与 Gateway 生命周期接线在后续 M4 变更实现。
- 真实 OneDrive refresh 仍须在安全集成环境人工验证；离线 conformance MUST NOT 替代 REP-010 验收。

## 依据

- [rclone lsjson](https://rclone.org/commands/rclone_lsjson/)
- [rclone 1.75.1 官方 Dockerfile](https://github.com/rclone/rclone/blob/v1.75.1/Dockerfile)

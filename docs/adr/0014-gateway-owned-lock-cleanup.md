# ADR-0014：Gateway 验证本次备份的临时锁清理

用户确认：历史对象不可删除/覆盖，但正常 Restic 备份 MUST 能清理自身临时锁。固定版本 [Restic 0.19.1](https://github.com/restic/restic/blob/v0.19.1/internal/restic/lock.go) 刷新锁时先创建新锁、再删除旧锁；[rclone 1.75.1](https://github.com/rclone/rclone/blob/v1.75.1/cmd/serve/restic/restic.go) 的 append-only 允许删除任意 `locks/` 对象，单独使用该开关不能证明归属。

公开入口 MUST 经过 RestFleet Gateway 安全层，rclone MUST 只监听私有 Unix socket、启用 append-only 并关闭对象缓存。每个 socket MUST 固定到一个 Host 的一个 Repository，禁止请求改变 remote/path。中心维护通道保持独立。Agent MUST NOT 创建 config/keys、初始化仓库或执行维护性 unlock。

锁归属 MUST 绑定中心可信调用方确认的 Host、Repository、Gateway identity 和本次 Operation；MUST NOT 从 Agent 自报 hostname/PID、锁密文或 HTTP header 推断。安全层为单次会话生成独立的 256-bit 随机能力凭据，进程内仅保留其 SHA-256 verifier；长期 gateway password 本身不构成锁归属证明。只有本会话明确探测为不存在、成功创建且仍有归属记录的锁才 MAY 删除。旧会话、预存、维护或归属不明的锁 MUST 拒绝。失败上传 MUST NOT 授予删除权；删除发出前 MUST 消耗归属，响应丢失不自动重复删除。

所有可写对象 MUST 在有界缓冲中完成 SHA-256 校验后才发送给后端，名称 MUST 等于内容哈希；config/keys 始终只读。探测仅明确 404 才允许创建，其他错误 fail closed；同一会话的写入串行。内容寻址校验额外防止后端存在性查询竞态导致不同内容覆盖。单对象上限 32 MiB、锁上限 64 KiB、每会话最多 1,024 个锁创建记录及 8 个在途请求；采用默认 Restic pack 大小，不开放任意 pack-size。列表/读取保持流式，不将整个 Repository 缓存在内存。

会话失效、取消或 Gateway 重启 MUST 失去删除权限；残留锁只走中心维护流程。会话记录是短时删除权限证明，不是权威任务队列。后续 supervisor MUST 通过已有持久化 Operation/租约接入，保证每 Repo 同时只有一个有效会话、无中心并发写入，并在重启/替换 backend 前终止旧会话。控制面离线的调度和数据面准入协调仍须实现，MUST NOT 将每次定时备份改成必须在线请求 Control API。

本批仅交付安全层及 fake/pinned binary 验证，不开放网络会话创建 API，不改变 Agent protobuf/数据库，也不把 Repository 标为 READY。supervisor、TLS 部署、持久审计接线、离线准入、凭据下发/ACK 和轮换仍是 M4 后续验收；本内部能力凭据不替代长期 gateway credential rotation。

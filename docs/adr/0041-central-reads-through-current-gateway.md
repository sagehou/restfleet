# 中心只读沿当前 Gateway owner 执行

常驻 Gateway 占用禁止第二个 rclone/OAuth writer；直接让索引 worker 忽略 fence 会破坏此约束。中心 snapshot index/list 改为使用原 owner 内的一次只读会话，沿相同 runtime、材料、token watcher、授权和可靠审计执行；与备份及后台探测串行，绝不解除占用。没有委托 owner 的旧中心读取仍须取得原仓库 lease 并通过无占用检查，初始化/维护不获得例外。

中心为每个 durable read job 在本机生成短期 Ed25519 key，并用已部署中心 CA 签发独立 `urn:restfleet:repository-reader:<repository>:<operation>:<admission>:<runtime>` mTLS 身份，最长 15 分钟。Enrollment/续证继续覆盖 CSR 中自报 SAN，不能签出 reader；reader 同样不能成为 Agent。证书绑定仓库、任务和当前 runtime/admission，普通 Agent、其他仓库、旧 runtime 及过期身份均拒绝。Gateway 仅向 reader 开放 read-sessions 协议及该会话的 GET/HEAD，所有对象写入和删除均拒绝；会话关闭只清理本次进程，不释放 fence。Gateway 始终不持有 Restic password。

read binding 持久化在 jobs，enqueue/claim/完成均核验原绑定与当前授权，中心签发前与 Restic secret 读取同事务提交固定审计。worker 不读取云端材料，不接收任意 endpoint，TLS origin/CA 仅来自受保护服务配置；TLS 私钥和密码只在中心受保护 tmpfs 文件使用。旧 Gateway 不理解 read-sessions 时明确失败，不能回退启动第二个云端 writer。中心读开始/结束使用独立固定审计事件，不能冒充 Agent 备份或清理证明。

周期索引复用与 API 相同的 READY 投影，按 UUID cursor 遍历全部仓库，逐页最多 200 个；不能依赖持久化 status='READY' 或反复只选第一页。此决定为 #96 元数据索引及 #98 后续文件读取共用边界，不新增服务或通用远程命令接口。

# 12 — V1 验收测试

## 1. 规则

- **P0**：发布阻断；安全不变量或数据正确性。
- **P1**：V1 功能阻断。
- **P2**：可在明确记录的限制下延后。
- 每个测试必须自动化，除标记 `MANUAL_DRILL` 的灾备/真实云端场景。
- 测试输出不得包含真实 secret；使用 canary secret。
- 集成测试锁定 Restic/rclone 版本与 checksum/image digest。

## 2. Bootstrap 与 Web Auth

| ID | P | Given / When / Then |
|---|---:|---|
| AUTH-001 | P0 | 空数据库且 bootstrap secret 有效；创建首个 Admin；创建成功且 secret 永久失效。 |
| AUTH-002 | P0 | 已有 User；再次调用 bootstrap；请求被拒绝且审计。 |
| AUTH-003 | P0 | 错误登录连续发生；系统限速/退避，响应不泄露用户名存在性。 |
| AUTH-004 | P0 | 已登录 Session；缺少/错误 CSRF 修改资源；403 且无变更。 |
| AUTH-005 | P1 | Session idle/absolute TTL 到期；后续请求 401，session hash 标记失效。 |
| AUTH-006 | P0 | Canary secret 经过 login/problem/log/audit；所有输出均无明文。 |

## 3. Enrollment 与 PKI

| ID | P | Given / When / Then |
|---|---:|---|
| ENR-001 | P0 | 创建 token；DB 只含 keyed hash/fingerprint，不含原 token。 |
| ENR-002 | P0 | 有效 single-use token + 合法 Ed25519 CSR；签发 cert，Agent ACTIVE，token 原子 USED。 |
| ENR-003 | P0 | 两个并发请求使用同一 token；仅一个成功。 |
| ENR-004 | P0 | 过期/revoked/已用 token；Enrollment 均失败且不签证书。 |
| ENR-005 | P0 | CSR signature 无效或 key type 不允许；失败且 token 未被错误消费。 |
| ENR-006 | P0 | CSR 请求伪造 SAN/agent_id；签发证书只使用 Server 构造身份。 |
| ENR-007 | P0 | Agent private key；Server/DB/network capture 中不存在该 private key。 |
| ENR-008 | P0 | 被 revoke Agent 用仍有效 cert 建连；拒绝并记录安全审计。 |
| ENR-009 | P1 | cert 剩余 7 天；Agent 轮换、验证新连接、旧 cert overlap 后失效。 |
| ENR-010 | P1 | Docker Agent 容器重建且 state volume 保留；install identity 不变，不创建新 Agent。 |

## 4. Agent 连接与配置

| ID | P | Given / When / Then |
|---|---:|---|
| AGT-001 | P0 | Agent 无任何入站端口；仍能通过 outbound gRPC 完成管理。 |
| AGT-002 | P0 | 证书 agent A，payload 自报 agent B；Server 仍只按 A 授权。 |
| AGT-003 | P1 | Plan revision 42；Agent 原子保存后 ACK 42，Server 显示 accepted。 |
| AGT-004 | P0 | 新配置非法；Agent 拒绝并继续使用 last-known-good，不部分应用。 |
| AGT-005 | P0 | 中心控制 API/DB 离线 12h，但 Gateway 在线；Agent 按本地计划成功备份。 |
| AGT-006 | P1 | Agent 离线产生 Operation；重连后幂等补报，Server 无重复 Operation。 |
| AGT-007 | P0 | 同一 JobDispatch 重复 3 次；Restic 仅执行一次，重复请求得到同一结果。 |
| AGT-008 | P1 | 日志消息乱序/重复；Server 以 sequence 去重并正确显示。 |
| AGT-009 | P1 | Agent clock 偏移超过阈值；状态 DEGRADED/告警，但授权不只依赖 agent timestamp。 |
| AGT-010 | P1 | N Server + N-1 Agent；backup、heartbeat、config sync 正常。 |

## 5. Local Scheduler

| ID | P | Given / When / Then |
|---|---:|---|
| SCH-001 | P0 | timezone cron 跨 UTC 日期；Agent 在配置 IANA timezone 的正确本地时刻执行。 |
| SCH-002 | P1 | Agent 在 misfire grace 内重启；同一 scheduled time 只补跑一次。 |
| SCH-003 | P1 | Agent 超过 misfire grace 重启；不补跑，回报 MISSED。 |
| SCH-004 | P0 | 上一 backup 仍运行；FORBID 阻止重叠并回报 CONCURRENCY_SKIPPED。 |
| SCH-005 | P1 | 同 Plan/scheduled time 多次重启；stable jitter 和 deterministic key 防重复。 |
| SCH-006 | P1 | Plan pause；后续 schedule 不执行，已运行任务不被隐式取消。 |

## 6. Repository 与 Secret 隔离

| ID | P | Given / When / Then |
|---|---:|---|
| REP-001 | P0 | 新 Host 建 Repo；得到独立 UUID path、Restic password、gateway identity。 |
| REP-002 | P0 | Agent A credential 请求 Agent B repo path；Gateway 拒绝。 |
| REP-003 | P0 | Agent 对已有 repository object（包括预存、其他会话、维护锁）发 DELETE；Gateway 拒绝，对象仍存在。只有当前授权会话成功新建的临时锁可清理，刷新/结束正常，维护 unlock 不下放。 |
| REP-004 | P0 | Agent 对已有 object 尝试 overwrite；Gateway 拒绝或不改变原内容。 |
| REP-005 | P0 | Agent 备份需要读 index；正常成功，文档/UI 不标记为 write-only。 |
| REP-006 | P0 | Agent filesystem/process/env inventory；不存在 rclone/provider/Crypt/admin secret。 |
| REP-007 | P0 | 数据库 dump 无 master key；所有 secret plaintext 不可获得。 |
| REP-008 | P0 | 错误 master key 启动；Server fail closed，危险操作不可执行。 |
| REP-009 | P0 | Gateway rclone config materialize；位于 tmpfs、0600、重启后重新生成。 |
| REP-010 | P0 | rclone 自动刷新 OAuth token；新 revision 加密回 DB，重启后仍可访问。 |
| REP-011 | P1 | Gateway password rotation；Agent ACK 新 revision 后旧 secret 在 overlap 后失效，备份不中断。 |
| REP-012 | P0 | 请求 Shared Repository；V1 API 返回明确不支持。 |

多后端扩展验收：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-013 | P0 | OneDrive/Google Drive/WebDAV 导入与独立仓库记录创建；provider 自动识别，metadata 无目标或秘密回显，旧 OneDrive 数据兼容。 |
| REP-014 | P0 | Google Drive token 更新加密 CAS 回写；folder/team/scope/client 改写被 watcher 拒绝，WebDAV 静态秘密不能伪装成 OAuth refresh。 |
| REP-015 | P0 | WebDAV 内网、metadata、混合 DNS、重绑定、socket 注入和不可信 TLS 被拒绝；重定向不能选择非固定网络目的地。 |
| REP-016 | P0 MANUAL_INTEGRATION | 三种后端分别完成真实云端写入、备份、索引和恢复；OAuth 两种后端完成刷新/重启验收，WebDAV 完成认证失效/替换与服务兼容性验收。 |

初始化任务验收（不替代 REP-016）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-017 | P0 | initialize 要求 ADMIN/CSRF/幂等 key，无 body/query；任务、审计、事件与 outbox 原子提交，同 key 不重复创建。 |
| REP-018 | P0 | 初始化与有效 backup/maintenance lease 冲突时拒绝执行；job/repository 续租及审计后 fence 拒绝旧 owner 提交或刷新。 |
| REP-019 | P0 | worker 中断后重领，复用原路径/密码及最新 token；成功仅设置中心验证 metadata，保持 PROVISIONING，失败不自动 unlock/delete 或创建另一套凭据。 |

Gateway 临时锁例外验收（用户确认的安全模型变更，见 ADR-0014）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-020 | P0 | 锁归属绑定可信 Host/Repository/Operation 与独立会话能力；上传失败不授权，取消/过期/重启及删除响应丢失不恢复删除权，旧会话凭据不能删除新会话对象。 |
| REP-021 | P0 | 公共写入先验证完整内容 SHA-256、大小和规范路径，config/keys 只读；并发重复/畸形/跨 Host/未知方法/错误 TLS/后端非 404 探测均 fail closed，审计失败不执行锁删除。 |

Gateway supervisor 生命周期验收：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-022 | P0 | 配置/审计/socket/HEAD config 验证未完成或失败时，不发布路由/能力；ready 后请求只到固定 UUID root 的 append-only 后端；不自动 init 或重启。 |
| REP-023 | P0 | 会话结束、超时、取消、后端退出、refresh 目标篡改/回写失败；能力清零、路由撤销、进程组退出、tmpfs 清理完成后才释放进程内容量；同 Host/Repo/Gateway/Operation/Credential 互斥，不替代持久化租约。 |

Agent 初始仓库凭据交付验收（ADR-0015，不替代 REP-011 轮换和 AGT-005）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-024 | P0 | mTLS 下发仅包含同 Host 已初始化仓库的密码；三种 provider 均无云端材料泄漏；无 capability 的旧 Agent 不接收新消息，审计失败不得解密/下发。 |
| REP-025 | P0 | Agent 原子 0600 落盘后 ACK，重连/重复/并发不改变交付身份；拒绝回滚、冲突和不安全文件；错误保留 last-known-good。 |
| REP-026 | P0 | ACK 精确绑定当前 Agent/交付 ID/revision/秘密引用/Gateway 配置；跨 Host、禁用、吊销和过期版本被拒绝；ACK/outbox/审计原子提交，Web 不误报 READY。 |

Gateway TLS transport 验收（不等于 command 可公网部署）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-027 | P0 | 固定 Restic/rclone 经真实 TLS transport 备份、清理本会话锁及读回成功；错误 CA、旧 TLS、明文、HTTP/2 绕过、超大 header、非规范路径及伪造 proxy header 均不获得额外权限，无管理路由或原始秘密回显。 |
| REP-028 | P0 | 未完成握手/idle 占连接容量且超时释放；认证前固定窗口限流、审计洪泛合并且失败不放行；取消或 listener 故障中断 stalled upload 并等待请求/审计、进程组和 tmpfs 清理，已关闭 transport 不可重用。 |

持久化备份占用验收（ADR-0016，不替代 Gateway 接线或 AGT-005）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-029 | P0 | 三种后端均只从认证 Agent 的当前已 ACK 仓库申请占用；重复/并发重放不延长期限、无新任务；跨 Host、错误 owner、失效 ACK、禁用/撤销及不安全期限拒绝，原始秘密不进入记录/outbox。 |
| REP-030 | P0 | 同凭据的备份占用与 test/init/replace、有效 repository lease 互斥；过期仍排斥新 owner/维护，只有可信 owner 清理确认才释放；claim 不能旁路，禁用不自动释放，审计失败回滚占用/outbox/释放，Down 不能抹去历史。 |

Gateway 加密回写验收（ADR-0020，不替代生产接线、REP-016 或 AGT-005）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-031 | P0 | 中心当前授权与 fence 核验后注册不可替换的来源；Gateway 仅持接收公钥，来源签名与加密独立；伪造来源、密文/header 移植、跨实例/Host、非规范或超长记录拒绝，磁盘无配置明文。 |
| REP-032 | P0 | 文件和目录 fsync 后才接受；字节/记录数硬上限不丢旧记录；空间耗尽/写入不确定使 watcher/supervisor 停止；精确签名确认先保存再回收，确认丢失重放同 wire，错误确认不删文件。 |
| REP-033 | P0 | 三后端中央有序幂等接收；并发重复只有一个 effect，冲突/跳序/旧 ID/非 token 改写/CAS 失败拒绝；审计失败不产生历史、秘密版本或 ACK；旧有效记录可在过期/禁用/吊销后回放，不能借回写恢复授权或释放 fence。 |
| REP-034 | P0 | Unix 回写通道双向 UID + 实例签名认证，拒绝版本/大小/不安全 socket/错误 UID，取消等待 handler；重启只验证回放、缺失/损坏阻塞、不恢复数据面；未精确封存来源不能释放占用，历史 migration Down 拒绝删除。 |

Gateway 本地签名授权会话验收（ADR-0021；不替代生产材料交付、REP-016 或 AGT-005）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-035 | P0 | owner 核验完整占用/运行绑定、有效签名授权、注册来源/确认公钥及期限；跨绑定、已释放/封存/过期、第二 owner、已有历史/重开来源拒绝，排空不能重置 token revision 或恢复 owner。 |
| REP-036 | P0 | 每次路由及 backend 转发前核验本地授权和待回写容量，HEAD 成功/上传开始不授予后续写权限；生命周期 watchdog 包括未开始/会话间空闲期，吊销、自然到期、时钟回拨、Queue 关闭/冻结/耗尽/写入不确定主动取消并 join 所有工作、清零已借用的旧明文 buffer、冻结来源；失败 owner 不因高版本续期/排空恢复，重复/并发 Close 等待完成，原在线准入保持核验。 |
| REP-037 | P0 | 同一本地 owner 无中心调用连续两次备份，维持刷新后的配置与 expected revision；新会话拒绝旧 capability，审计在对应独立来源内持久化；固定 Restic/rclone 的 TLS 备份、读回及临时锁归属负向套件通过。返回/Close 只冻结清理本地，不自动释放中心 fence。 |

Gateway 单次加密材料初始化验收（ADR-0022；同 UID 内部通道，不替代生产隔离、REP-016 或 AGT-005）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-038 | P0 | 来源签名 fresh nonce/临时接收公钥/完整 binding；独立中心验签加密；错误 pin、绑定、签名、密文、hash、未知/重复/非规范字段和大小拒绝；中心私钥与 Restic password 不交给 Gateway，借用 config/临时私钥清零。 |
| REP-039 | P0 | 三后端真实 DB 核验当前身份、owner、ACK、来源、revision、期限与最新普通授权；禁用/吊销/封存/已回写/审计失败拒绝；意图和访问审计原子提交，并发仅一次，提交后新挑战或无回执不再发，不释放 fence；应用角色不能改删历史，Down 拒绝。 |
| REP-040 | P0 | 单次 Unix RFGM 交换拒绝错误 UID/版本/runtime/帧上限；安装失败、取消或回执发送丢失 rollback/join，Close 消费未使用或取消/join 接收者；已安装 fresh owner 连续两次固定二进制 TLS 备份及读回，旧初始化不能复活 owner。回执不代表 READY/ACK/释放。 |

跨 UID 通道验收（ADR-0023，GitHub Actions 执行；不替代完整生产接线）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-041 | P0 | 共享组仅显式启用；精确 0710 owner/group 目录与 0660 socket；错误 owner/group、权限、特殊位、多个 GID 和非规范 UID 配置拒绝；私有默认不接受共享目录，已有 socket 不接管，peer UID 验证与签名不削弱。 |
| REP-042 | P0 | Actions 运行不同非 root UID 的中心/Gateway 实际进程，Gateway 本地产生来源私钥、中心仅获公钥；独立 pin 下加密初始化及可靠签名回放/确认成功，借用明文清零；两个服务不能读取另一服务私钥或替换其 socket，同组第三 UID 不触发回写 handler。进程取消/失败必须等待退出再清理测试文件。 |

来源身份文件验收（ADR-0024，GitHub Actions 执行；不替代生产启动协调）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-043 | P0 | source-init 仅在服务所有 canonical 0700 私有目录生成新 0600 seed，三处 fsync 后只输出公钥；并发只有一个身份，旧文件不覆盖；fsync/发布不确定保留证据并拒绝自动重建；source-public 只导出已有公钥。错误/stdout 不含 seed、原始路径/argv/IO 错误；拒绝特殊权限、symlink/hardlink/pending。 |
| REP-044 | P0 | 来源 seed 与独立中心公钥 pin 从两个受保护文件装载，坏 pin 不返回私钥副本，不能从初始化 wire 选择信任；receiver 复制后临时来源副本清零。实际跨 UID 夹具由 Gateway 使用生产文件创建/装载 API，中心仅取得公钥；其他服务/第三 UID 不能读取 Gateway 私有 seed/pin，原加密初始化和持久签名回写全部通过。 |

中心初始化协调验收（ADR-0025，GitHub Actions 执行；不替代 Gateway 生产 daemon/READY）：

| ID | P | Given / When / Then |
|---|---:|---|
| REP-045 | P0 | 中心元数据仅从服务所有 canonical 0700 私有目录的单链接 0400/0600 文件装载；版本/大小/完整 binding/公钥/期限/UID/GID/socket 路径有界验证，拒绝特殊权限、symlink/hardlink/pending 与重复/未知/遗漏/大小写/null/非规范 JSON。命令非法参数及路径仅返回固定错误，生产禁止 root/同 UID 私有协调。 |
| REP-046 | P0 | 三后端真实 DB 从无授权/来源/意图开始，经来源签名与 peer 验证后首次授权→注册→单次加密交付，借用明文清零；错误来源/binding/UID、失效 ACK/禁用、审计与注册失败拒绝且不撤销先前 committed 阶段。安装失败 rollback 保留单次意图；新挑战不再交付，新 runtime 不能接管旧 admission，不释放 fence。 |

Gateway 授权决定交付验收（ADR-0026；不代表 daemon、cleanup receipt 或 READY）：

| ID | 优先级 | 场景与预期 |
|---|---|---|
| REP-047 | P0 | RFGA 独立签名域、fresh nonce、完整 binding 与精确回执；错误 UID/pin/magic/version/runtime/空或超长帧、伪造/非规范证明及错误回执拒绝。不同非 root 服务 UID 顺序续期/精确重放/吊销成功，同组第三 UID 不触发中心决定；取消关闭连接并 join 阻塞 callback，已观察拒绝的独立有界审计不因取消丢失，审计失败停止 listener，原私钥/socket 隔离不削弱。 |
| REP-048 | P0 | 三后端真实首次初始化后续期、相同幂等键重放与禁用后明确吊销；错误来源/注册绑定/UID/revision、失效 ACK、封存和审计失败不产生新 grant 或重发材料。回执丢失仅显式重放同一最新决定，不改到期或释放 fence；活动 owner 经通道吊销后取消/join、清零/冻结，续期不能越过原占用或复活失败 owner。 |

Gateway 全局离线审计验收（ADR-0027）：

| ID | 优先级 | 场景与预期 |
|---|---|---|
| REP-049 | P0 | 独立 origin/runtime 来源不含仓库/占用，global_audit 与仓库记录/回执域不能互认；拒绝 config/token、scoped/authenticated 事件、普通授权/refresh 和非规范 wire。全局 Queue 可靠加密有界存储、确认丢失不回收、准确 ACK 后回收，恢复仅回放，不确定文件保留并拒绝；跨非 root UID 两种回放均成功，第三 UID 无 effect。 |
| REP-050 | P0 | 固定全局 recorder 唯一认领，非法事件仅记录固定拒绝，不猜测身份。全局容量/时钟/关闭失效触发活动 owner watchdog 取消/join、清零/冻结，排空和高版本续期不能复活；未路由与限流观察可在中心离线时可靠保存。 |
| REP-051 | P0 | 三后端真实 DB 独立注册、无授权的离线观察→有序回放、确认丢失/并发重复仅一次 effect、精确 tail 封存。错误公钥/runtime/来源、未来/旧时间、跳序/冲突、封存后新记录及审计失败拒绝，不写 grant/材料/Host 归属、不释放 fence；注册/历史权限及 Down 保留约束通过。 |

Gateway 显式恢复回放验收（ADR-0028）：

| ID | Priority | 验收 |
|---|---|---|
| REP-052 | P0 | 受保护版本配置严格核验规范编码/文件/peer/域/上限，私钥不存在仍能恢复两类既有 Queue；丢失确认保留原 wire，显式精确重放与空队列回放得到相同 tail，取消关闭交换/释放本地 lock 但不恢复 producer。活跃 producer、换 binding/来源/recipient/中心 pin/上限、损坏记录与不确定文件 MUST 拒绝并保留证据，不触发 effect、授权、材料、子进程或 fence release。 |
| REP-053 | P0 | CLI 经保护配置→恢复→RFGR→精确回执，成功仅输出对应 binding 与已确认 tail；参数/配置/输出错误固定且不回显秘密，输出失败不重复 effect。跨非 root UID 对两个域装载生产配置并恢复回放，同组第三 UID 仍无 effect，私钥/配置/Queue/socket 隔离保持。 |

Gateway 启动等待与拒绝审计验收（ADR-0029）：

| ID | 优先级 | 场景与预期 |
|---|---|---|
| REP-054 | P0 | 显式初始化等待大于零且最多 5min；延后超过原 5s 的中心连接仍完成原认证交换，Accept 后交换最多 5s，父取消/Close 同时约束两个阶段。非法参数、等待超时和交换失败消费接收器、关闭 listener、清零私钥；不自动重试、不释放 fence。原 Receive 继续总 5s 上限，UID/pin/帧/密文和单次意图负向边界不变。 |
| REP-055 | P0 | 接收等待/交换超时、Close 和发送取消后已观察拒绝使用独立最多 3s 的有效审计 context；Close 和返回等待 callback/rollback 退出再清零，不因父 context 取消丢观察，不回显秘密或原始错误，不恢复数据面。 |

中心全局审计注册命令验收（ADR-0030）：

| ID | 优先级 | 场景与预期 |
|---|---|---|
| REP-056 | P0 | 中心公开 metadata 仅从服务所有 canonical 0700 私有目录的单链接 0400/0600 文件装载；1024 bytes、版本、完整 origin/runtime UUIDv7 和非全零 32-byte 来源公钥严格检查，拒绝未知/重复/遗漏/大小写/null/非规范 JSON、权限、symlink/hardlink/pending 和 64-byte 私钥输入。非法 CLI 参数/输出返回固定错误，无秘密或原始路径/错误。 |
| REP-057 | P0 | 实际 CLI 经保护 metadata/中心运行配置→DB/schema/审计链核验→原注册事务→仅公开 metadata/接收公钥输出；输出失败保留 committed 来源，精确显式重试不重复审计/改变注册时刻。换来源/runtime、同 runtime 新 origin、封存、无效 schema/审计链/DB 不注册；生产 root 拒绝且不产生 effect。三后端完整数据库从无 grant/material 开始，经生产元数据入口验证 audit-only 注册→Queue→回放，无授权/材料或 fence release。 |

Gateway 单次启动生命周期验收（ADR-0031）：

| ID | 优先级 | 场景与预期 |
|---|---|---|
| REP-058 | P0 | 保护版本 metadata/source/pin/TLS 文件严格装载，明确上限、完整绑定/独立来源/路径分离/UID；拒绝非规范配置、tmpfs/ramfs Queue、权限、坏 TLS/来源、旧 Queue/socket 和生产 root。全部单次初始化完成前不绑定公网，部分取消/错误 recipient 清理并保留已创建 Queue，不能接管旧来源。 |
| REP-059 | P0 | 一个 Service 支持两个隔离仓库、连续会话和 verified TLS；控制面失联保留两类加密审计，恢复后丢 ACK 精确 wire 重试。非法 ACK、全局容量与 RFGA 吊销停止全部 owner，取消/join 活动 callback/请求/后端进程组，清零来源及云端材料，重复 Close 等待同一退出。原 Queue 可恢复回放，运行失败或排空不复活，不封存/释放 fence。 |
| REP-060 | P0 | 跨非 root UID 的 protected production config→Service→材料/RFGA/TLS/两类回写→活动吊销与清理；Gateway 本地产生独立来源/TLS 私钥，中心秘密仅匿名 stdin 到中心，第三 UID/私钥读取/socket 替换拒绝且无 effect。固定 Restic/rclone 保留 online/signed-local 并增加 Service，连续两次备份及读回、各 session 锁清理、wrong CA、删除/覆盖/预存锁拒绝全部通过；不据此宣称 Agent 会话协议、READY、真实云或 AGT-005 完成。 |

## 7. Backup 与 Restic 解析

| ID | P | Given / When / Then |
|---|---:|---|
| BAK-001 | P0 | 有效 Plan；Restic argv 不经 shell，路径/排除按独立文件/argv 传入。 |
| BAK-002 | P0 | 文件名含空格、引号、换行、号 restic-like flags；不造成参数/命令注入。 |
| BAK-003 | P1 | Restic exit 0 + summary；Operation SUCCEEDED，统计和 snapshot ID 正确。 |
| BAK-004 | P0 | Restic exit 3 + snapshot；Operation SUCCEEDED_WITH_WARNINGS，Dashboard 不显示全绿。 |
| BAK-005 | P0 | exit 12；WRONG_REPOSITORY_PASSWORD，不进行无限/高频 retry。 |
| BAK-006 | P1 | exit 11；分类 REPOSITORY_LOCKED，并按策略 retry。 |
| BAK-007 | P0 | 未知 exit code；FAILED/UNKNOWN_EXIT_CODE。 |
| BAK-008 | P0 | exit 0 但无 summary；FAILED/INVALID_ENGINE_OUTPUT。 |
| BAK-009 | P1 | JSONL 含未知字段/message；已知 summary 仍解析成功。 |
| BAK-010 | P1 | `--skip-if-unchanged` 无 snapshot ID；Operation 成功并显示“无变化”。 |
| BAK-011 | P0 | pre-hook 只引用 Agent allowlist；Server 发送任意 command/arg 被拒绝。 |
| BAK-012 | P1 | Cancel running backup；终止 process group，Operation CANCELED，无孤儿 Restic。 |

## 8. Snapshot 浏览与下载

| ID | P | Given / When / Then |
|---|---:|---|
| SNP-001 | P1 | `snapshots --json`；完整 ID/tags/plan 映射正确。 |
| SNP-002 | P0 | 无 managed tag Snapshot；标为 UNMANAGED，不进入自动 retention。 |
| SNP-003 | P1 | 一次 index 暂时缺 Snapshot；先 missing，不因单次失败立即隐藏。 |
| SNP-004 | P1 | `ls --json` 1M nodes；流式受限处理，Server/Browser 内存不无界增长。 |
| SNP-005 | P0 | Snapshot path 含 `../`、NUL、header chars；拒绝且不执行 Restic。 |
| SNP-006 | P0 | 下载目录/symlink；V1 regular-file download 拒绝。 |
| SNP-007 | P0 | download intent；一次性、5min、绑定 user/session/path，重放失败。 |
| SNP-008 | P1 | 用户中止下载；Restic process 取消，Operation 有正确终态和审计。 |
| SNP-009 | P0 | access log/problem/audit；不泄露含敏感文件名的 query token 或 repo password。 |

## 9. Restore

| ID | P | Given / When / Then |
|---|---:|---|
| RST-001 | P0 | 有效 preview + confirm；只恢复到 `/var/lib/restfleet/restores/<job-id>`。 |
| RST-002 | P0 | Server 请求任意 absolute target/in-place/`--delete`；Agent 拒绝。 |
| RST-003 | P0 | Snapshot 属于其他 Host repo；restore create/dispatch 被拒绝。 |
| RST-004 | P0 | stale/changed preview；执行返回 conflict，未启动 Restic。 |
| RST-005 | P1 | 恢复成功；统计、staging path、Operation、Audit 一致。 |
| RST-006 | P1 | 恢复中取消；partial staging 保留并明确标记，不远程递归删除。 |
| RST-007 | P0 | symlink/path traversal 尝试逃逸 staging；失败，外部文件不变。 |
| RST-008 | P1 | Docker Agent；只能写已挂载 restore volume，backup mounts 保持只读。 |

## 10. Retention 与 Maintenance

| ID | P | Given / When / Then |
|---|---:|---|
| MNT-001 | P0 | Retention dry-run；只包含 `managed + plan:<id>`，不含其他 Plan/unmanaged。 |
| MNT-002 | P0 | Preview 后 snapshot set 改变；forget 拒绝并要求重新 preview。 |
| MNT-003 | P0 | 策略将删至少于 minimum 1；操作拒绝。 |
| MNT-004 | P0 | 尝试 empty `--group-by` 或 unsafe remove all；API/adapter 均拒绝。 |
| MNT-005 | P0 | Active backup lease；forget/prune/unlock 不启动。 |
| MNT-006 | P1 | 同 Repo 两个 maintenance；只串行执行。 |
| MNT-007 | P0 | Agent 收到 CHECK/FORGET/PRUNE/UNLOCK job；协议层拒绝。 |
| MNT-008 | P1 | Check JSON 报 broken pack；Repo DEGRADED/critical alert，不自动 repair。 |
| MNT-009 | P1 | Prune 中断；状态明确，下一步先 check，不盲目 success/retry。 |
| MNT-010 | P0 | Unlock preview 有 active lock；不得选择；仅 stale lock 可执行。 |

## 11. Notifications、Logs 与 Audit

| ID | P | Given / When / Then |
|---|---:|---|
| OBS-001 | P0 | Canary secrets 经过 Agent log、Server log、Operation log、metrics、audit、notification；零明文。 |
| OBS-002 | P1 | 同一故障持续；cooldown 内去重，恢复后发送 resolved。 |
| OBS-003 | P1 | 一个 webhook 失败；其他 channel 仍投递，原 Operation 状态不变。 |
| OBS-004 | P0 | Webhook 指向 metadata/loopback/private IP；默认 SSRF policy 拒绝。 |
| OBS-005 | P0 | Audit writer 不可用；download/restore/credential/forget/prune/unlock fail closed。 |
| OBS-006 | P0 | Audit row 被修改；hash chain verification 发现异常。 |
| OBS-007 | P1 | Operation 日志超过 10 MiB；bounded、truncated marker 存在、summary 保留。 |
| OBS-008 | P1 | Dashboard；Online 与 Backup Healthy 使用独立字段/计数。 |
| OBS-009 | P1 | Metrics；不存在 filename、raw UUID explosion 或 secret high-cardinality labels。 |

## 12. API 与并发

| ID | P | Given / When / Then |
|---|---:|---|
| API-001 | P0 | 同 Idempotency-Key + 同 body 并发；只产生一个副作用并返回同 resource。 |
| API-002 | P0 | 同 key + 不同 body；409，无第二副作用。 |
| API-003 | P1 | 旧 If-Match 更新 Plan；412，当前配置不被覆盖。 |
| API-004 | P1 | 10k snapshots cursor pagination；无重复/遗漏（固定 snapshot view）。 |
| API-005 | P0 | Problem response；无 subprocess command、backend path credential 或 secret。 |
| API-006 | P1 | SSE 断线重连；从 event ID 恢复，无状态倒退。 |

## 13. Restart 与灾备

| ID | P | Given / When / Then |
|---|---:|---|
| DR-001 | P0 | Server 在业务变更 commit 后/outbox publish 前崩溃；重启后事件仍处理。 |
| DR-002 | P0 | Worker lease 中崩溃；过期后幂等 reclaim，不重复数据副作用。 |
| DR-003 | P1 | Agent 在 Restic start 前/后崩溃；恢复为明确 LOST/FAILED，不永久 RUNNING。 |
| DR-004 | P0 | 只恢复 DB、无 master key；明确不可解密，系统 fail closed。 |
| DR-005 | P0 MANUAL_DRILL | 新中心 + DB backup + master key + CA/credential；恢复后可查询快照并接收旧 Agent。 |
| DR-006 | P1 MANUAL_DRILL | OneDrive throttling/network outage；指数退避、告警去重、恢复后成功。 |

## 14. 多架构与部署

| ID | P | Given / When / Then |
|---|---:|---|
| DEP-001 | P0 | linux/amd64 Native；enroll/config/backup/restore smoke pass。 |
| DEP-002 | P0 | linux/arm64 Native；同上。 |
| DEP-003 | P0 | linux/amd64 Docker；同上且无 privileged/docker socket。 |
| DEP-004 | P0 | linux/arm64 Docker；同上。 |
| DEP-005 | P0 | 外网扫描中心；Postgres/admin gateway/rclone RC 不可达。 |
| DEP-006 | P0 | 错误 CA/hostname；Agent 和 Restic TLS 连接失败，不 fallback insecure。 |
| DEP-007 | P1 | N→N+1 Server upgrade + N Agent；计划备份不中断，schema compatible。 |
| DEP-008 | P0 | Image/artifacts；有 checksum、SBOM、multi-arch manifest、非 latest pin。 |

## 15. 性能与资源

| ID | P | Given / When / Then |
|---|---:|---|
| PERF-001 | P1 | 100 concurrent Agent streams；heartbeat/reconnect 无明显泄漏。 |
| PERF-002 | P1 | 50 Hosts/100 Plans；Dashboard cached P95 <500ms。 |
| PERF-003 | P1 | 10k snapshot metadata；list P95 <300ms（不含 live Restic）。 |
| PERF-004 | P1 | 10 concurrent backups；Gateway/DB/Server 保持 bounded resources。 |
| PERF-005 | P1 | 大日志/目录；Web 不冻结，virtualization/streaming 有效。 |

## 16. Release 结果记录

每次 release candidate 生成：

```text
version / commit
Restic/rclone versions + checksums
DB schema version
test environment
passed/failed/skipped acceptance IDs
manual drill evidence links
known limitations
approver/date
```

任何 P0 skipped/failed 阻止 V1 发布。P1 例外需要明确 risk acceptance 和 follow-up issue。

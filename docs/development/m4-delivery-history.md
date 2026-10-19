# M4 分批交付历史

基线：2026-10-08 已合并 `07a85e39169c416817df97ecb5cd63224ebb9d8a`（#60）。以下保存各批交付时的边界与验证记录；其中“仍待完成”等表述仅代表当批时点，当前状态以 [实施计划 M4](../../IMPLEMENTATION_PLAN.md#m4--storage-credential-repository-and-gateway) 为准。三十批并非三十个独立 PR，#51 包含多个批次。

分批交付：凭据管理（#16）、中心 tmpfs/runtime/token watcher（#17）、持久化异步 Test Operation/租约/幂等/加密 CAS 与测试 UI（#18）、中心 Restic 初始化适配器及固定二进制离线验证（#19）均已合并。第五批（#20，已合并）交付 Repository 记录事务、per-Host 独立加密凭据、创建/列表/详情 API 与 Web；创建仅得到 PROVISIONING，不代表云端初始化或 Agent 已接受。第七批（#22，已合并）接入初始化 Operation/jobs 与基础状态 UI；Agent ACK、Public Gateway、凭据轮换、完整 provisioning UI 和多后端真实服务人工验收仍待完成。

范围变更：根据用户要求，V1 存储后端扩展为 OneDrive、Google Drive、HTTPS WebDAV + rclone Crypt；第六批（#21，已合并）交付配置/凭据/网络安全与 UI 兼容。第七批（#22，已合并）接入 initialize API、持久化 Operation/jobs、双重租约、中心执行和状态 UI；成功仍为 PROVISIONING，Gateway/Agent ACK 留在后续批次。其他 rclone 后端需显式校验与测试后再加入，不能直接透传配置。

第八批（#23，已合并）实现 Gateway 单会话安全层、临时锁归属验证、内容哈希/路径/方法限制与固定二进制离线验证（ADR-0014，用户已同意正常备份临时锁例外）。本批不开放公网 listener 或会话创建 API；supervisor、持久化准入/审计、Agent 凭据下发/ACK 和离线协调仍待接线，M4 不标完成。

第九批（#24，已合并）接入进程内 Gateway supervisor/router：复用 tmpfs/token watcher，增加有界长会话入口、私有 rclone 进程组、ready 后挂载、互斥/容量和完整退出清理。固定二进制备份读回测试改为经 supervisor 执行；command 公网配置、持久化准入/审计、Agent ACK 和离线协调仍待接入。

第十批（#25，已合并）接入 Agent 仓库凭据交付：复用 outbound mTLS、专用 0600 原子文件、PostgreSQL 交付记录/outbox/审计与精确 ACK；Web 区分交付与确认。只交付本 Host 已初始化仓库的密码，兼容无 capability 的旧 Agent。尚未完成公网 Gateway、持久化 backup admission、密码 overlap/retirement、离线协调或 READY，M4 继续进行。

第十一批（#26，已合并）增加 supervisor 的有界 TLS transport：128 连接、认证前限流、聚合限流审计、传输 deadline 与关闭清理；固定 Restic/rclone 备份读回改走此入口。复用现有组件，不提供缺少持久化准入的公网 command；独立 readiness、持久化准入/审计、会话能力交付、rotation 和离线协调仍待完成。

第十二批（#27，已合并）实现持久化备份占用原语及中心写入口互斥（ADR-0016 / schema 11）：复用凭据锁、Repository advisory lock、审计/outbox；到期不自动释放占用，可信 owner 确认清理后才释放。尚不接 Gateway 进程/公开申请/会话下发，也不宣称离线授权或 M4 完成。

第十三批（#28，已合并）接入在线 Gateway 占用生命周期：材料落盘前核验绑定、期限约束、周期复查、成功清理/审计后释放，重复与异常路径保留 fence；固定二进制 TLS 验收经过该接缝。继续复用现有 supervisor 与占用原语；公开 command、owner 恢复、材料/会话交付及离线许可尚未完成。

第十四批（#29，已合并）接入占用约束的中心材料服务：当前配置密文与访问审计原子读取、中心短时解密借用、token-only 验证与加密 revision CAS。复用既有校验锁和 envelope；无新 schema/公共 API，公网进程交付、数据面审计与离线/恢复仍待完成。

第十五批（#30，已合并）接入 Gateway 在线审计适配器：固定分类/字段 allowlist、未知输入拒绝且不回显、同步提交既有审计链、清理意图与完成结果明确区分。固定二进制 TLS 验收经过适配器，PostgreSQL 集成测试覆盖链与回滚；独立进程通道、公网 command、离线及恢复仍待完成。

已接受架构决策（ADR-0017）：独立 Gateway 使用中心签发、只可在线续期的有界离线授权，以及可靠落盘的有界加密审计/token 待回写区；控制面失联时撤销/禁用最多延迟 12h。本次只记录批准和交付约束，不宣称离线实现、AGT-005 或 M4 完成。后续顺序为授权/连续 fence → 受保护进程通道与持久化 → 幂等回写/恢复 → command/readiness/端到端验收，具体见部署规范 09 §7.10；三后端真实环境人工验收仍待提供证据。

第十六批（#36，已合并）实现签名授权决定和本地防回滚状态原语（ADR-0018）：区分失联、到期和明确吊销，独立 Ed25519 验签、精确运行绑定、12h 签发上限与并发/重放/时钟负向测试。不新增公共授权 API 或迁移；中心事务签发/续期、连续 fence、持久化回写、command 与 AGT-005 仍待完成。

第十七批接入中心授权决定事务与提交后签名（schema 12 / ADR-0019）：追加历史、精确幂等/revision CAS、原占用期限内续期、明确吊销与在线材料拒绝，保留连续 fence。三后端、并发、绑定/状态、审计回滚/到期与迁移负向测试交 CI 验证。密钥生产装载、独立通道、可信恢复/回写、command/readiness 与 AGT-005 仍待完成。

第十八批（#51，已合并）实现独立来源签名、公钥加密与有界 fsync 待回写区，以及中央有序幂等审计/token-only 加密 CAS（schema 13 / ADR-0020）。受保护本地回写 listener 和中心私钥文件装载已接入 Server；Gateway 审计/运行 watcher 经现有 supervisor 测试验证。重启只回放、不恢复授权/数据面，未封存来源保留 fence；新增 REP-031–034。授权/材料及 Agent 会话能力的生产通道、可信清理恢复、command/readiness、rotation/READY 与 AGT-005 仍未完成。

第十九批（#51，已合并）连接签名授权、待回写 Queue 与既有 supervisor 的内部会话生命周期（ADR-0021 / REP-035–037）：同一唯一来源 owner 连续备份并保留已接受 token/revision，每次转发前检查授权及容量，吊销/到期/时钟/持久化失败取消并等待清理。失败不恢复、Close 不释放中心 fence，原在线入口不变。固定二进制 TLS 套件同时覆盖在线和本地签名授权两条路径；来源/材料交付仍用明确夹具，不代表生产通道、可信恢复、READY 或 AGT-005 完成。

第二十批（#51，已合并）增加来源签名 fresh challenge、中心加密材料、单次 Unix 初始化与 owner 安装（schema 14 / ADR-0022 / REP-038–040）。当前身份/ACK/来源/revision/最新授权与单次意图及访问审计原子提交；提交后失败或回执丢失不重发、不释放 fence。固定真实引擎的本地授权路径经此通道初始化，三后端事务用真实 PostgreSQL 验证。同 UID 测试不代表生产 UID 隔离、受保护 pin/source 装载或 command 完成；生产协调、续期/吊销、全局离线审计、Agent 能力、可信恢复、rotation/READY、真实云和 AGT-005 仍待完成。

第二十一批（#51，已合并）增加显式共享组的跨 UID 材料/回写通道（ADR-0023 / REP-041–042），中心 replay 配置精确 peer UID/GID；私有默认与签名/加密协议保持。最终 head `08b1ebc` 的 Actions 十项全通过，独立 job 使用两个非 root 服务及同组第三用户验证私钥/目录/socket 隔离；开发环境只编辑源码。首轮发现的 Web 高危传递依赖已兼容修复，原生 npm audit 门禁报告零漏洞。合并 commit 为 `2fbe498`；完整生产信任配置、启动协调及 M4 其他退出条件继续待完成。

第二十二批（#52，已合并）接入来源 seed 的本地无覆盖可靠创建、source-public 导出与独立中心公钥 pin 文件装载（ADR-0024 / REP-043–044）。最终 head `ab11d1b` 与合并后的 `ea8921e` 均通过 Actions 十项检查。复用 protected reader / MaterialReceiver，补充特殊权限拒绝和可清零解码；跨 UID 套件走生产文件 API。身份命令不启动数据面、不注册来源、不确认 binding 或释放 fence。

第二十三批（#53，已合并）加入中心受保护初始化元数据、来源/UID 认证后首授权→来源注册→单次材料交付的实际协调与显式 `gateway-start` 命令（ADR-0025 / REP-045–046）。最终 head `71c3866` 与合并 commit `93e6aa1` 均通过 Actions 十项。复用既有通道/事务，失败保留部分提交与 fence，不自动重读文件或重试；三后端夹具从无授权/来源开始。

第二十四批（#54，已合并）修复空闲 owner 未主动清理过期材料的问题：复用唯一生命周期 watchdog 覆盖构造后、会话中与会话间空闲期，失效先阻止新会话，再取消/join、清零和冻结。最终 head `12fadab` 与合并 commit `5c13453` 均通过 Actions 十项。扩展 REP-036 验证自然到期/吊销/时钟/Queue 关闭与耗尽、并发 Close、借用 buffer 清零、高版本续期和排空不复活。

第二十五批（#55，已合并）加入 metadata-only RFGA 续期/明确吊销通道和中心已注册来源核验后的决定交付（ADR-0026 / REP-047–048）。最终 head `e7f3369` 与合并 commit `804307f` 均通过 Actions 十项。来源 fresh challenge 与精确回执独立签名，先验证 UID/pin/binding 才提交决定；回执丢失保留 fence，仅允许显式同一最新决定重放。更新同一 owner，不重发材料、延长原占用或复活失败；覆盖跨非 root UID、三后端 DB 和活动 owner 吊销清理，修复取消打断拒绝审计的时序。

第二十六批（#56，已合并）补齐独立全局离线拒绝审计（schema 15 / ADR-0027 / REP-049–051）。复用有界加密 Queue、fsync/确认及 Unix 回放，独立 audit-only 来源/签名域不猜测 Host/Repository 或授予数据访问；固定观察和中央接收历史原子提交。全局生产者失败同时停止本地 owner，取消/join 后清零/冻结，排空不复活。最终 head `5d2eb98` 与合并 commit `7985c20` 均通过 Actions 十项，格式化补丁由 Actions 生成并同步。Gateway 生产配置/daemon、多仓库数据面、自动可靠续期/吊销分发、Agent 能力、可信恢复/readiness、rotation/READY、真实云和 AGT-005 继续待完成。

第二十七批（#57，已合并）接入显式 `restfleet-gateway replay --config-file`（ADR-0028 / REP-052–053）：受保护规范 metadata 与独立中央公钥 pin，仅恢复/回放既有两类 Queue，不读取私钥或启动旧数据面。原密文/精确回执支持丢 ACK 后显式重放；活跃 flock、换身份/pin/上限、取消、篡改和不确定证据拒绝并保留，输出只报告已确认 tail。跨非 root UID fixture 经生产配置装载与恢复路径；最终 head `12b8e1c` 与合并 commit `79c89c6` 均通过 Actions 十项检查。完整 daemon、可信进程清理/恢复、自动授权与 Agent 能力、readiness/rotation/READY、真实云及 AGT-005 仍待完成。

第二十八批（#58，已合并）交付 Gateway 启动前置修复（ADR-0029 / REP-054–055）：显式有界等待与每次 5s 材料交换分开，既有 Receive 保持等待/交换合计 5s；失败审计独立最多 3s，Close/退出等待完成再清零。单次意图、拒绝旧来源接管和失败保留 fence 的边界不变。最终 head `2df76a3` 与合并 commit `2eaf44f` 均通过 Actions 十项检查；生产 daemon、全局审计注册 command、Agent 能力、可信恢复/readiness、rotation/READY、真实云及 AGT-005 继续待完成。

第二十九批（#59，已合并）接入中心显式 `gateway-audit-register --config-file`（ADR-0030 / REP-056–057）：保护公开元数据、中心运行配置与 DB/schema/审计链核验后复用 audit-only 注册事务，仅公开 JSON 输出；精确重复和输出失败保留原注册，冲突/封存/生产 root 拒绝。实际命令隔离 schema 与三后端完整数据库在 Actions 验证；最终 head `0957be0` 与合并 commit `934c4af` 均通过十项检查。Gateway daemon、自动授权分发、Agent 能力、可信恢复/readiness、rotation/READY、真实云及 AGT-005 继续待完成。

第三十批（#60，已合并）接入 Gateway 单次启动 owner（ADR-0031 / REP-058–060）：protected config/source/pin/TLS→并行单次材料初始化→原 owner RFGA→多仓库 TLS→两类 RFGR 精确密文回写。全部初始化完成前不绑定公网；部分失败、非法 ACK、全局容量或吊销取消/join 全部工作、清零并保留 Queue/fence。跨非 root UID 生产配置和固定引擎连续两次备份/读回经新 Service 验收，原在线与签名路径保留。最终 head `ff15e6a` 与合并 commit `07a85e3` 的 Actions 十项均通过；格式化/编译/运行验证仅 Actions。WithBackup 仍是可信进程内接缝，Agent 会话协议和可部署 daemon command 尚未连接；自动授权分发、可信清理恢复/readiness、rotation/READY、真实云与 AGT-005 继续待完成，12h 取舍尚无决定。

第三十一批（`feat/m4-rotation-recovery`，待合并）接入 M4 正常轮换、受控恢复与本地预置（ADR-0036 / 09 §7.27）：`gateway-rotate` 显式撤回旧 runtime，只有签名终结记录与原子释放后才预留下一占用、注册下一全局来源、单次初始化并接续 worker；`gateway-recover` 仅对到期、无终结记录且来源匹配的占用执行撤回/封存/释放，审计 `RECOVERY_CONFIRMED`，不伪造清理记录；Gateway UID 的 `provision`/`finalize` 生成新 runtime 来源 seed、Queue/state 与受保护 service.json，中心不接触私钥。未新增 schema/wire/公开 API，未改写既有测试，仅新增 metadata/策略边界测试；新增状态机、跨 UID 生产链路和部署验收仍待 Actions。

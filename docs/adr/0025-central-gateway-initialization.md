# 中心受保护元数据与显式单次 Gateway 初始化协调

## 背景

ADR-0022–0024 已提供单次加密材料通道、不同服务 UID 的访问策略和本地来源身份。中心仍需实际串接初次授权、来源注册与材料交付；不能让连接请求选取来源 pin，也不能把旧配置当作重启后可复用的启动配方。

## 决定

- 增加中心服务所有私有目录内的版本 1 初始化元数据。文件 MUST 是 canonical 路径、0700 父目录、0400/0600 regular file、单链接、无特殊权限/symlink/pending 痕迹，最多 4096 bytes。字段只包含完整 binding、来源**公钥**、初次 decision ID、有限授权时长、Unix socket 路径、预期 Gateway UID 与显式共享 GID，不包含任何 secret。祖先路径 MUST 由可信管理员维护。
- JSON MUST 按部署规范的字段顺序/类型编码；MAY 添加排版空白。解析后精确重新编码核验 MUST 拒绝重复、未知、遗漏、大小写替代、null 和非规范表示。公钥 MUST 是标准 base64 的 32-byte Ed25519 公钥；UUID MUST 是规范 UUIDv7。
- `ControlPlane.InitializeGateway` MUST 先检查 schema/审计可用性，再通过既有 `SendMaterial` 核验 socket 权限、精确 SO_PEERCRED UID、完整 binding 与来源签名挑战。只有认证通过才 MAY 依次调用初次 `DecideGatewayAuthorization`（expected revision = 0）、核验已签发完整 binding、`RegisterGatewayPending`、`GatewayMaterialDelivery`。数据库原语要求授权先于注册；三者不是一个新事务，各阶段 MUST 保持已有事务与审计约束。
- 增加显式中心命令 `restfleet-server gateway-start --config-file <absolute-path>`，复用中心受保护运行配置、数据库和密钥。生产 MUST 由非 root 中心 UID 执行，连接不同非 root Gateway UID，显式专用共享组；私有同 UID 模式仅供非生产兼容验收。该命令不创建 CA、不启动 HTTP/gRPC/rclone/Restic，不切换 UID 或启动 Gateway。默认单次交换最多 5s、总 context 最多 10s、不等待或重试。显式启动等待和初始化后的唯一 worker 交接按下述补充执行。
- binding/runtime MUST 由可信进程协调者为本次独立 Gateway incarnation 准备，并与 Gateway 本地配置一致；来源公钥 MUST 独立取得。文件读取或签名成功不能证明进程新鲜性。本命令 MUST NOT 配置为 Server 普通重启自动执行；旧 admission 不允许借新 runtime 接管，旧 runtime 不能借新挑战重新交付已提交意图后的材料。
- 初次授权提交后后续失败 MUST 保留授权和原 fence；注册后失败 MUST 保留来源。材料意图提交后（包括安装失败或回执丢失）MUST NOT 再发。命令 MUST NOT 续期、封存、释放占用、删除旧 socket/记录、修复文件或宣称 READY。错误仅返回固定分类，不回显路径、元数据、provider/DB 错误或明文。可信协调者 MUST 将本次交付与该 admission 的其他决定/投递/清理串行化。

## 验证与后续

### 中心准备入口（2026-10-16）

`gateway-prepare --config-file <request> --plan-file <plan>` MUST 从可信中心 metadata 选择 Agent，通过 PostgreSQL 读取其当前已初始化仓库与已确认交付，只读取 ID/状态，不读取 secret payload。占用 ID、owner 与首次 decision ID MUST 自动生成，并在预留占用前通过受保护文件的 fsync/no-replace 发布固定下来。文件发布不确定时 MUST NOT 预留；数据库提交或响应不确定时 MUST 保留文件，重试沿用原 ID/时长。绑定或请求变化、已释放/到期占用 MUST 拒绝，不创建替代占用或延长期限。原 StorageCredential 互斥保持，不允许同一云凭据的多个运行绕过排斥。

输入 MUST 沿用私有目录/文件与精确 JSON 校验，最多 4096 bytes；字段顺序见部署规范 §7.18。输出为最多 8192 bytes 的版本 2 plan，依序包含 `version`、`preparation`（原输入）、`startup`（原版本 1 初始化 metadata）。现有 `gateway-start` 与授权 worker MUST 能直接读取该 plan；原版本 1 文件保持兼容。版本 2 仅组织本地中心 metadata，不改变 RFGA/RFGR/材料 wire 或数据库 schema。

准备不签发授权、注册来源、发送材料或启动进程；全局审计来源继续走既有显式注册命令。独立来源公钥、fresh runtime 与本地私有 Queue/配置仍 MUST 由可信部署者准备。普通 Server 重启只 MAY 从 plan 续传已提交决定，不得隐式预留或初始化。准备、初始化和 worker MUST 串行交接；plan 不证明新鲜性、清理或 READY。按用户指示暂不新增/改写测试，新行为专门验收保持待完成。

### 显式启动与持续授权交接（2026-10-16）

`gateway-start` MAY 指定 `--audit-config-file`，在交付前复用 ADR-0030 的原事务注册/核验全局来源。runtime MUST 与仓库相同，来源公钥 MUST 独立；全部受保护 metadata MUST 在打开数据库前完成校验。注册失败 MUST 阻止交付，后续失败 MUST 保留已提交来源和占用。

`--wait-seconds` MAY 显式设置 0..300 秒；默认 0 保持立即单次尝试。正值只允许等待材料 socket 首次发布，路径不存在时有界等待；发现已有非 socket、其他文件错误或取消 MUST 结束。实际交换仍 MUST 重新核验受保护路径、SO_PEERCRED、来源 pin/签名，只执行一次；等待不能重新交付、延长 DB 授权/占用、删除旧 socket 或证明新鲜性。设置期总 context 最多为等待值加 10s。

`--follow` MUST 只接受已有版本 2 plan，初始化回执核验成功后才 MAY 在同一进程、同一已打开中心 store 上启动原授权 worker；初始化与续期 MUST 串行交接。设置期 context 结束后 worker 使用信号 context，保持原每轮上限/准确重放/期限/明确吊销规则，退出 MUST 取消并 join。显式 follow 与 `RESTFLEET_GATEWAY_AUTHORITY_FILE` 同时启用 MUST 拒绝，部署者仍 MUST 保证其他中心进程没有同 admission 的投递者。失败不进入 worker，不自动重试初始化；该命令 MUST NOT 配置为自动重启配方。中心中断后的授权投递 MAY 通过普通 Server 的原 authority-file worker 恢复，不能重跑初始化或恢复失败 Gateway owner。

REP-045–046 在 Actions 验证受保护元数据/命令拒绝、三后端真实 PostgreSQL 的首次签发→注册→交付、完整 pin/binding 验证、部分事务失败、安装 rollback、重新交付与新 incarnation 接管拒绝。测试夹具 MUST 不预先签发/注册，防止把原语测试误当成协调验收。

不增加 DB schema、wire、公开 HTTP/gRPC API、依赖或生产特权。此批交付中心单次协调入口；Gateway 生产配置和独立 daemon、多仓库数据面接线、续期/吊销、全局离线审计、Agent 会话能力、可信清理/恢复、readiness、rotation/READY、真实云端和 AGT-005 仍待完成。中心命令成功仅表示收到精确初始化回执。

# 中心受保护元数据与显式单次 Gateway 初始化协调

## 背景

ADR-0022–0024 已提供单次加密材料通道、不同服务 UID 的访问策略和本地来源身份。中心仍需实际串接初次授权、来源注册与材料交付；不能让连接请求选取来源 pin，也不能把旧配置当作重启后可复用的启动配方。

## 决定

- 增加中心服务所有私有目录内的版本 1 初始化元数据。文件 MUST 是 canonical 路径、0700 父目录、0400/0600 regular file、单链接、无特殊权限/symlink/pending 痕迹，最多 4096 bytes。字段只包含完整 binding、来源**公钥**、初次 decision ID、有限授权时长、Unix socket 路径、预期 Gateway UID 与显式共享 GID，不包含任何 secret。祖先路径 MUST 由可信管理员维护。
- JSON MUST 按部署规范的字段顺序/类型编码；MAY 添加排版空白。解析后精确重新编码核验 MUST 拒绝重复、未知、遗漏、大小写替代、null 和非规范表示。公钥 MUST 是标准 base64 的 32-byte Ed25519 公钥；UUID MUST 是规范 UUIDv7。
- `ControlPlane.InitializeGateway` MUST 先检查 schema/审计可用性，再通过既有 `SendMaterial` 核验 socket 权限、精确 SO_PEERCRED UID、完整 binding 与来源签名挑战。只有认证通过才 MAY 依次调用初次 `DecideGatewayAuthorization`（expected revision = 0）、核验已签发完整 binding、`RegisterGatewayPending`、`GatewayMaterialDelivery`。数据库原语要求授权先于注册；三者不是一个新事务，各阶段 MUST 保持已有事务与审计约束。
- 增加显式中心命令 `restfleet-server gateway-start --config-file <absolute-path>`，复用中心受保护运行配置、数据库和密钥。生产 MUST 由非 root 中心 UID 执行，连接不同非 root Gateway UID，显式专用共享组；私有同 UID 模式仅供非生产兼容验收。该命令不创建 CA、不启动 HTTP/gRPC/worker/rclone/Restic，不切换 UID 或启动 Gateway。单次交换最多 5s，命令包括数据库连接的总 context 最多 10s，不自动等待 socket 出现或重试。
- binding/runtime MUST 由可信进程协调者为本次独立 Gateway incarnation 准备，并与 Gateway 本地配置一致；来源公钥 MUST 独立取得。文件读取或签名成功不能证明进程新鲜性。本命令 MUST NOT 配置为 Server 普通重启自动执行；旧 admission 不允许借新 runtime 接管，旧 runtime 不能借新挑战重新交付已提交意图后的材料。
- 初次授权提交后后续失败 MUST 保留授权和原 fence；注册后失败 MUST 保留来源。材料意图提交后（包括安装失败或回执丢失）MUST NOT 再发。命令 MUST NOT 续期、封存、释放占用、删除旧 socket/记录、修复文件或宣称 READY。错误仅返回固定分类，不回显路径、元数据、provider/DB 错误或明文。可信协调者 MUST 将本次交付与该 admission 的其他决定/投递/清理串行化。

## 验证与后续

REP-045–046 在 Actions 验证受保护元数据/命令拒绝、三后端真实 PostgreSQL 的首次签发→注册→交付、完整 pin/binding 验证、部分事务失败、安装 rollback、重新交付与新 incarnation 接管拒绝。测试夹具 MUST 不预先签发/注册，防止把原语测试误当成协调验收。

不增加 DB schema、wire、公开 HTTP/gRPC API、依赖或生产特权。此批交付中心单次协调入口；Gateway 生产配置和独立 daemon、多仓库数据面接线、续期/吊销、全局离线审计、Agent 会话能力、可信清理/恢复、readiness、rotation/READY、真实云端和 AGT-005 仍待完成。中心命令成功仅表示收到精确初始化回执。

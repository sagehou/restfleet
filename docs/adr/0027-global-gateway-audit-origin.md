# Gateway 全局离线审计来源

## 背景

Supervisor 的未路由请求、认证前限流和本地通道拒绝没有可信 Host/Repository。ADR-0020 的队列绑定备份占用，不能为这些观察猜测资源身份；同步中心审计又不能用于控制面离线的独立 Gateway。

## 决定

- 增加仅含 origin_id/runtime_id 的独立全局审计来源，由可信中心协调器注册独立取得的 Gateway 来源公钥。一个 runtime 仅有一个来源；同 ID 的精确注册幂等，公钥/runtime 不可换，封存后不可重新注册。它不含 Host、Repository、凭据或占用引用，不签发授权、不读取云端材料、不释放 fence。
- schema 15 增加 `gateway_audit_origins` 与有序 `gateway_audit_records`。来源注册/封存与审计原子提交；回放的固定无身份事件与接收历史原子提交，来源行锁串行化顺序。回执丢失可精确重放，冲突/跳序/未来时间/早于注册/封存后的新记录拒绝。应用角色不能改删历史或换来源；有历史时 Down 拒绝。
- 复用既有有界加密 Queue、fsync、回收和 RFGR 精确 peer UID 通道。全局 header 添加 `audit_origin`，原 binding 必须为零值且 authorization_revision=0；kind 固定 `global_audit`，config=null、expected_secret_revision=0。只允许无 binding、未认证的固定全局拒绝/非法事件观察；无法携带会话启动、云端刷新或备份身份。
- 全局 wire/回执使用独立 Ed25519 签名域，见 09 §7.20。旧备份 wire/identity/回执的规范编码保持不变；旧 reader 不认识新域/字段时 fail closed，不能归一化为仓库记录。来源签名独立于中心公钥加密，中心接收私钥及签名私钥继续只在中心。
- `GlobalAuditRecorder` 唯一认领 fresh Queue，固定事件成功表示已可靠落盘，不表示中央入库。失效/容量不足/时钟回拨/关闭永久阻止生产者，排空和重连不复活。生产运行 MUST 将 Record 和 Ready 一起提供给 Supervisor；所有本地授权 owner 的构造、每操作 guard 与生命周期 watchdog 同时检查全局审计就绪，失败后取消/join、清零并冻结仓库来源。
- 全局审计可在仓库授权到期或身份禁用后回放，不授予新的数据访问。恢复仍仅回放；可信中心在公网入口及所有记录者已退出、精确 tail 已入库后才 MAY 封存，全局 tail/回执本身不证明清理，也不能代替每个 Repository 的来源封存与 fence release。
- 不新增公开 HTTP/gRPC 路由、依赖或 root 生产特权。中心既有 replay listener 接收两个严格隔离的记录域。生产启动配置/daemon 会使用此全局来源；本批不把注册或审计回执当作 READY。

## 验证

REP-049–051 在 Actions 验证编码/签名域和权限隔离、加密持久存储/不确定写/回执回收/只回放恢复、全局失效对活动 owner 的取消与清零、三后端真实 PostgreSQL 的离线观察→可靠回放/确认丢失/并发重放/封存，以及跨非 root UID 的两个记录域。所有编译、格式化与运行验收仅在 Actions。

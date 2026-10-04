# Gateway 显式恢复回放命令

## 背景

ADR-0020/0027 的仓库与全局 Queue 已支持可靠回放、精确确认与只回放恢复，但 Gateway command 尚无入口。崩溃后的未确认审计/token 需要可操作的回写路径，不能通过重启旧数据面或再次交付材料完成恢复。

## 决定

- 增加显式单次 `restfleet-gateway replay --config-file`。每次只处理一个既有仓库或全局 Queue，复用 Recover/RecoverGlobalAudit、RFGR、精确 peer UID 与签名回执；整体 context 最多 1min，每帧原有 5s 限制保持。失败不自动重试，原始密文与不确定文件继续保留。
- 版本 1 配置只包含完整对应 binding、来源/中心接收公钥、独立中心验证 pin 文件、Queue 路径/原始上限、中央 socket 和显式 UID/GID。配置来自 Gateway 服务所有的 0700 目录，文件 0400/0600、单链接、无 symlink；规范字段及顺序严格重新编码核验，允许排版空白，拒绝重复/未知/缺失/null/混域等输入。配置不包含来源 seed、中心私钥、rclone config、Restic/gateway password、master key 或数据库连接。
- 恢复只读取来源公钥和独立中心公钥 pin，不读取来源私钥；本地 Queue 身份必须与可信配置完全一致。活跃 flock、损坏/不确定证据、换来源/recipient/pin/binding/上限全部拒绝。新建 Queue、修复损坏/不确定文件、追加、授权安装/续期、启动 rclone 或公网 listener 不属于此命令；可靠回执已经精确确认的遗留文件继续按既有恢复规则回收。
- 成功输出版本、对应 binding、已精确确认的 sequence/wire_hash。输出失败仍保留已有可靠回执；再次显式运行可读取同一 tail。tail 不证明旧请求/进程/追加者停止，也不授予封存、fence release 或 Repository READY；可信清理仍须独立协调。
- 旧 wire、DB schema、source-init/source-public 与默认 version 行为保持兼容，不新增 HTTP/gRPC 接口、依赖或生产 root 权限。生产使用两个非 root UID 及专用共享组；同 UID 私有模式继续只用于明确的测试/既有私有协调。

## 验证

REP-052–053 仅在 Actions 验证：规范配置与文件/peer/域/容量负向，公开密钥恢复两域、原 wire 的丢 ACK 重放、取消/活跃 producer/篡改/不确定文件保留、命令输出脱敏，以及跨非 root UID 的生产配置装载→恢复→RFGR 确认。中心幂等事务仍由 REP-031–034/051 的真实 PostgreSQL 测试验证。完整 daemon、可信进程清理恢复、自动授权与 Agent 会话能力、readiness/rotation/READY、真实云和 AGT-005 仍待完成。

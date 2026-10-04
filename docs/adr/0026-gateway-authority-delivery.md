# Gateway 授权续期与明确吊销的独立本地交付

## 背景

ADR-0018–0025 已提供有界授权、持久决定、单次材料交付和本地会话 owner。续期及吊销仍需交付到同一已初始化 owner；不能重走初始化来重发云端材料，也不能把传输错误当作撤销或续期。

## 决定

- 增加 metadata-only `RFGA` Unix 通道，复用受保护目录/socket、精确 SO_PEERCRED UID 和有界帧。中心与 Gateway MUST 使用独立配置的完整 binding、来源公钥与中心验签 pin；共享模式 MUST 沿用不同非 root 服务 UID 和专用共享组。来源私钥仍只由 Gateway 本地产生并装载。
- Gateway 每连接 MUST 使用新的 CSPRNG 32-byte nonce，签署完整 binding 的挑战。中心 MUST 验证来源、peer UID 和注册来源匹配后，才调用既有授权事务。挑战仅证明配置来源持钥，不证明进程新鲜性、材料安装或 READY。
- `ControlPlane.DeliverGatewayAuthorization` MUST 由可信中心协调器调用，且与同 admission 的初始化、所有决定及清理串行。要求 expected revision ≥ 1；来源未封存、占用未释放、完整 binding 与注册公钥一致。普通续期继续核验当前身份/ACK/凭据/配置；明确吊销 MUST 能在身份或凭据禁用后交付，不增加普通授权。该入口不注册来源、不解密或重发材料、不封存或释放 fence。
- Gateway MUST 验证中心签名及完整 binding，并将 statement 交给同一已初始化 `AuthorizedBackup.AcceptAuthorization`。owner MUST 保留原 admission 到期上限；失败、关闭或失效 owner 不得借高版本续期复活。明确吊销触发生命周期 watchdog 和每操作 guard 的停止、取消、join、明文清零和 Queue 冻结。
- 来源签名回执 MUST 绑定本次 fresh challenge 与精确 signed statement 的 SHA-256。回执只证明 statement 已接受，不证明数据面/子进程已退出、持久回写已排空、READY 或 fence 可释放。
- 传输失败 MUST 保留已提交决定和原 fence，不修改授权状态。回执丢失代表接收不确定；可信协调器 MAY 显式重放同一最新已提交决定及原幂等键，MUST NOT 自动签发新 grant、重新交付材料或延长原到期。已经失败的 owner MAY 拒绝重放；收到吊销不恢复 owner。
- 不增加 DB schema、公共 HTTP/gRPC API、依赖或生产特权。无自动重试/续期调度器；本批提供内部通道与协调入口。全局离线拒绝审计、完整 daemon/readiness、Agent 能力交付及可信清理恢复仍需后续接线。

## 验证

REP-047–048 在 Actions 验证独立签名域、规范编码、帧上限/UID/pin/binding 拒绝、取消 join、跨非 root UID 续期/精确重放/吊销及第三 UID 拒绝。三后端真实 PostgreSQL 从首次初始化开始验证续期、禁用后吊销、回执丢失重放、失效 ACK/来源封存/审计失败的原子拒绝；活动 owner 验证实际交付吊销后的请求/子进程 join、明文清零与不可复活。

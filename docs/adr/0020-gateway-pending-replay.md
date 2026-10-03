# Gateway 待回写使用独立来源签名与中心公钥加密

沿用 ADR-0017，Gateway 将审计和 token-only 刷新写入受保护持久目录。采用已有 `x/crypto/nacl/box` 的匿名公钥封装；解密私钥只留在中心，Gateway 仅持接收公钥。每条密文另用独立 Gateway Ed25519 来源密钥签名。来源公钥 MUST 由可信中心协调器在当前授权和占用核验后注册，不能由回写请求自行指定。中心签名密钥也用于域分离的提交确认，不复用 Agent CA。

## 契约与存储

- v1 记录 MUST 绑定完整授权 binding、授权 revision、UUIDv7 record ID、单调 sequence、前条 wire SHA-256 和 UTC 秒时间。外层规范 JSON + 64-byte Ed25519 签名，内层规范 JSON MUST 重复相同 header；总长上限 512 KiB。规范重编码拒绝未知/重复字段及替代表示。
- 每条记录用独立 0600 文件，受保护 0700 目录 MUST 属于服务用户、无 symlink、独占 flock。文件 fsync、原子 rename 和目录 fsync 全部成功后才 MAY 接受；不确定写入 MUST 使实例停止追加。记录数和总文件字节（包括保留给确认/临时写入的空间）均有硬上限。
- 中心确认 MUST 签名精确 admission/runtime、sequence、record ID 和 wire hash，且只在事务提交后产生。Gateway MUST 先可靠保存确认，再回收对应文件；确认丢失只重放同一密文。重开目录只允许验证/回放旧记录，MUST NOT 恢复追加、授权或数据面。
- schema 13 存储不可替换的来源注册和只追加的接收历史。回放 MUST 按同一 credential → admission → origin 的锁序，校验连续 sequence/hash、唯一 ID 和精确绑定；审计或加密 token revision CAS 与接收历史同事务提交。重放同一 wire 不重复审计或刷新；同 sequence 不同内容、跳序和跨运行实例 MUST 拒绝。
- 过期/吊销后仍需排空既有记录：回放不是新授权。token 更新 MUST 引用历史有效授权、发生在授权区间内、保留未释放 fence、验证原始配置仅 token 改变，并在中心加密后提交；禁用不丢弃先前生成的刷新。审计记录允许授权终止后的清理观察，但不得将其当作清理证明。可信协调器 MUST 先停止并等待数据面，再关闭来源追加、排空并显式封存来源，最后才可释放 fence；封存本身不证明进程退出。
- 本地回写通道仅传密文与签名确认；固定 RFGR v1 长度帧、Unix socket、双向 SO_PEERCRED UID 核验、0600 socket 和有界时间/并发。来源签名和中心确认签名仍是必需的实例身份验证，不能用服务 UID 代替。此通道不交付云端材料、数据库/master key 或 Restic 密码。

## 交付边界

本批接入既有 Gateway 审计白名单与 token-only watcher 回调，并提供中心回放服务、本地通道和故障集成测试。在线 admission/material 入口保持原行为。生产 command、授权/材料及会话能力交付、可信清理恢复、rotation、READY 和 AGT-005 完整验收仍未完成；不得部署这些内部接口作为公网 API。

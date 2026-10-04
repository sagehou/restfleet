# ADR-0031 — Gateway 单次启动 owner 与完整退出接线

状态：已接受（M4 内部生产启动组件；Agent 会话协议和可部署 command 仍待完成）。

## 问题

材料 receiver、本地授权 owner、全局审计 Queue、TLS transport 和 RFGR 回写已有独立实现，但缺少共同启动与退出 owner。可信调用方手工组合这些组件，容易在部分初始化失败时提前开放公网，或在关闭队列后仍有拒绝/结束审计回调。

## 决定

增加 Linux `gateway.Service`，每次运行只持有一个 runtime/supervisor、一个独立全局审计 producer 及每仓库一个来源/owner。`LoadServiceConfig` 从受保护文件读取规范版本元数据；独立中心 pin、来源 seed 和 TLS 文件继续使用既有 protected reader。运行配置格式与上限见 09 §7.23。Gateway MUST NOT 取得中心私钥、DB/master key 或 Restic password。

队列 MUST 位于独立私有持久目录；启动检查明确拒绝 tmpfs/ramfs，管理员仍 MUST 提供持久介质和可信祖先。明文 runtime 继续只允许 0700 tmpfs。来源公钥 MUST 相互独立，所有仓库 runtime MUST 与全局来源一致。旧队列、socket 或元数据不证明来源注册、占用合法或进程新鲜性，MUST NOT 自动恢复 owner。

启动 MUST 先校验文件、建立全局审计及所有单次材料 listener，再并行等待初始化。配置仅含公开 binding、路径和公钥，云端材料仍由中心认证挑战、单次事务与加密交付。材料中的接收公钥 MUST 与配置相同。只有全部交换成功、各 owner 及全局 producer 可用后，才 MAY 建立授权 listener 和绑定公网 TLS。部分失败 MUST 取消并等待所有初始化/rollback，保留已有加密队列和中央 fence。

运行时 RFGA 更新原 owner，不重建或自动续期。一个 worker 每轮每 Queue 最多回写一条记录，再等待最多 1s 开始下一轮；每个交换仍有 5s 上限，轮次耗时包含交换。传输/回执丢失 MUST 保留原 wire 重试；收到不可核验的回执或 Queue 读取/确认失败 MUST 停止整次运行，不能丢弃证据。授权/owner 或全局 producer 失效 MUST 取消全部本地会话，任何高版本 grant、重连或排空都不能复活该运行。

退出 MUST 先关闭入口并取消、等待材料/授权回调、请求、会话、后端进程组和回写 worker；最终拒绝/结束审计完成后才清零来源与云端明文，冻结/关闭 Queue，最后关闭 runtime。重复/并发 Close MUST 等待同一结果。退出不封存来源、不释放中央占用、不修复证据或自动重新启动。

`Service.WithBackup` 仍是可信进程内接缝，MUST NOT 让 Agent 请求选择 binding、云端 config 或运行 callback。当前每次随机 session password 与 Agent 长期 gateway password 不同，生产会话申请/交付协议尚未连接；本批不增加缺少该协议的 daemon command，也不宣称 READY。无公共 API、wire、DB schema 或新服务依赖。

## 验收

REP-058–060 全部在 Actions 执行：严格配置/受保护文件与持久目录拒绝、部分初始化失败不开公网、两个仓库独立会话、控制面失联与精确密文重试、非法回执/全局容量/吊销取消并 join 清零，保留 Queue 并拒绝旧运行接管。跨非 root UID 测试经过生产配置/信任文件与新 Service；root 仅编排子进程，第三 UID 不得产生 effect。固定 Restic/rclone 套件保留原在线/签名路径并增加 Service 路径，验证连续两次备份读回、锁清理、错误 CA 及删除/覆盖/预存锁拒绝。

自动授权分发、Agent 会话能力、可信清理恢复/readiness、rotation/READY、真实三后端和 AGT-005 继续待完成。AGT-005 与签发后 12h 撤销上限的取舍仍未决定，不能通过本批降低验收。

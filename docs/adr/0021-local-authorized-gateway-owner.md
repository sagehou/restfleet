# 签名授权与待回写来源复用同一本地 Gateway owner

ADR-0017 的目标要求控制面失联期间使用已接受授权，实际数据面仍需执行到期/吊销及可靠持久化约束。本批在现有 supervisor 中增加内部 `AuthorizedBackup` 生命周期入口，复用 ADR-0018 的 `Authorization` 和 ADR-0020 的 Queue；在线 `WithAdmittedBackup` 保留原行为，无新增公网入口、数据库或部署进程。

## 决定

- 可信协调器 MUST 先通过受保护认证通道交付来源注册、匹配的原始云端材料/revision 及公钥，才 MAY 构造本地 owner。构造参数不是自证明，MUST NOT 从 Agent 请求反序列化。该交付通道仍未完成，本批不提供生产启动开关。
- owner MUST 核验完整 admission/runtime binding、未释放/未封存状态、原占用期限、已接受有效签名授权、注册来源公钥和中心确认公钥。来源仅 MAY 被 fresh Queue 的一个本地 owner 认领一次；已有追加历史、重开、排空或关闭均不能允许新 owner 重置 token revision、防回滚或清理状态。
- 同一连续占用内的多次本地备份 MUST 复用同一 owner、Authorization、token recorder 与已刷新配置，但每次生成新 session capability 和可信本地 operation ID。owner MUST 复用同一全局 supervisor 的容量/冲突检查。每个会话的审计使用其绑定来源，不能向另一占用的 Queue 回写；未路由和传输限流仍使用独立全局审计，不从请求推断来源。
- 每次路由及每次 backend HEAD/GET/POST/DELETE 前 MUST 检查本地授权和可写待回写区。上传后和探测后必须再次核验，不能因上传开始或 HEAD 成功延长授权。100ms watchdog 负责取消空闲/阻塞工作，退出必须等待所有请求、子进程、token watcher 和结束审计；不承诺取消已在后端开始的原子请求。中心失联本身不取消有效本地授权，也不续期。
- 可写检查保守要求有一个最大 wire（512 KiB）的剩余字节与一个记录位置；这是生产者准入保留空间，实际 Append 仍校验真实大小。已满/关闭/冻结、写入不确定、吊销、到期或时钟不安全 MUST fail closed。审计/刷新失败立即取消；任何运行失败后 owner 不再启动新会话，不能通过回放或续期清除此失败。
- 运行失败或 Close MUST 在取消并 join 本 owner 工作后清除当前明文配置、关闭 recorder 并冻结来源；Queue 保留给可信回放协调器。正常返回或 Close 均 MUST NOT 排空、封存中心来源、证明崩溃清理或释放 fence；中心需分别核验清理、精确回写和历史授权。

## 验证与未完成

REP-035–037 验证绑定/来源与确认密钥、禁止第二 owner/重开、逐次转发核验、吊销/到期/时钟/容量取消和连续刷新版本。固定 Restic 0.19.1/rclone 1.75.1 经现有 TLS transport 运行两次本地授权备份，并验证读回、独立锁归属及负向边界；材料和来源注册使用显式测试夹具，没有中心 DB 调用。它证明此内部接缝可运行，不替代生产材料通道、Agent scheduler、可信恢复、REP-016 真实服务或 AGT-005 的完整 12h 时序证据。

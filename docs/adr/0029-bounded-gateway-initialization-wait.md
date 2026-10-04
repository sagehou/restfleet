# ADR-0029 — 分离 Gateway 初始化等待与材料交换时限

状态：已接受（M4 启动前置修复；不表示 daemon 或 READY 已完成）。

## 问题

单次材料接收器原先在 Accept 前启动 5s context。中心逐个初始化仓库时，后续接收器可能尚未连接就耗尽交换预算。材料失败审计还使用同一已取消 context，导致超时/关闭时观察无法可靠记录。

## 决定

- 保留 `MaterialReceiver.Receive` 的等待/交换合计 5s 上限；失败时另给拒绝审计最多 3s。
- 新增内部 `ReceiveWaiting`，由可信启动者显式指定大于零且最多 5min 的连接等待时间；Accept 成功后另给最多 5s 的交换预算。父 context 及 Close MUST 同时约束两个阶段。
- 超时、无效等待、取消或任何无效 peer/交换 MUST 消费接收器、关闭 listener 并清零私钥；MUST NOT 自动重试、重发材料或释放 fence。
- 接收和发送失败的已观察拒绝 MUST 使用独立最多 3s 的审计 context，保留父 context 的值但不继承其取消。退出和 Close MUST 等待审计、rollback 和清零；回调仍 MUST 遵守 context，不能反向调用等待自己的 Close。

wire、签名域、单次意图、来源/UID/pin 验证、安装 rollback 及共享组策略均继续使用原契约。不新增 API、schema、后台协调器或服务依赖；等待时间不延长已签发授权或原占用。

## 验收

REP-054–055 在 GitHub Actions 验证中心延后超过原 5s 仍可初始化、交换仍不超过 5s、等待边界及超时消费、关闭等待审计、发送取消后的拒绝审计。既有负向、跨 UID、真实数据库及固定引擎测试 MUST 继续通过。生产 daemon、Agent 会话能力、可信恢复、rotation/READY、真实云和 AGT-005 继续待完成。

# Gateway 运行轮换与受控恢复

## 问题

一次 Gateway 运行受原备份占用期限约束。正常运行结束后必须取得签名清理证明并由中心原子封存、释放，才允许建立下一次运行；硬崩溃、SIGKILL、队列满/损坏或最后刷新不确定时没有该证明，中心既不能把到期当作清理，也不能让新运行接管旧 owner。现有命令要求部署者手工完成停止、等待、预留、注册和初始化，缺少可重试的正常轮换状态机，也没有明确的无证明受控恢复入口。

## 决定

### 正常轮换

中心 MUST 提供单一 `gateway-rotate` 协调入口，并遵守以下顺序：

1. 读取当前运行的最新 committed 授权决定，核验完整 binding、来源、socket/UID/组和当前 Agent。
2. 仅在原占用仍有效且剩余时间足以完成停止与下一运行初始化时开始；不足时拒绝，不用新 ID、重新初始化或延长期限绕过。
3. 向当前运行投递明确的撤回决定。撤回只终止该 runtime 的授权，不吊销 Agent 配对、长期密钥或已接受配置。
4. 只在收到已注册来源签名的 `runtime_cleanup/joined` 终结记录、且中心事务已原子封存来源并释放原占用后，才 MAY 为下一运行预留新占用。等待超时、回执丢失或只有到期/排空/进程不可达时 MUST 保留原 fence。
5. 下一运行 MUST 使用新 runtime、独立来源和独立占用；MUST NOT 复用旧 binding、旧材料或旧 owner。中心 MAY 自动完成预留、全局来源注册、单次初始化和授权 worker 接续，但本地进程仍由可信部署启动。

轮换命令 MUST 可安全重试：已 committed 的撤回只 MAY 按原 revision 精确重放；已释放但尚未准备下一运行时 MAY 继续准备；已初始化的下一运行 MUST NOT 再次初始化，改用既有 `gateway-start --follow` 或 authority-file worker 恢复投递。不得通过新 plan 接管已初始化或已释放的占用。

### 受控恢复

没有终结记录时，中心 MUST 保留占用，直到管理员显式执行 `gateway-recover`。恢复 MUST 同时满足：

- 原占用已经到期，且不存在任何仍有效的普通授权；
- 该 runtime 没有 `GATEWAY_RUNTIME_CLEANUP_COMPLETE` 记录；
- 若存在已注册来源，受保护配置提供的来源公钥必须精确匹配；若从未注册来源，则不得提供来源公钥；
- 管理员已在中心之外停止旧进程并隔离其 Host/数据面。

满足后，中心 MUST 在同一事务中撤回该 runtime 的剩余授权、关闭其待回写来源、写固定审计并释放原占用。审计 MUST 使用 `GATEWAY_BACKUP_RECOVERY` / `RECOVERY_CONFIRMED`，MUST NOT 伪造 `runtime_cleanup/joined` 或 `CLEANUP_CONFIRMED`。恢复 MUST NOT 删除 Queue、重放/补造记录、重建 owner、重新交付材料、延长原占用或恢复旧授权。恢复不是自动过期回收；未到期、已有终结记录、来源不匹配或没有操作者决定时必须拒绝。

### 状态与审计

运行周期为：`PREPARED → INITIALIZED → AUTHORIZED → CLEANED_OR_RECOVERED → RELEASED`。只有终结记录或显式恢复可以到达 `RELEASED`；到期、回执、队列排空和进程退出不可单独触发。轮换与恢复均 MUST 序列化同一 admission 的决定、材料、回写和释放；`gateway-rotate` 运行期间不得同时启用同一 admission 的其他 authority worker。

## 边界

本决定不新增数据库表、wire 字段、服务依赖或 Agent 能力，复用现有 admission/decision/pending 事务。`restfleet-gateway provision` 在 Gateway UID 下生成本机 runtime/来源 seed、私有 Queue/runtime 目录、durable state 与公开 preparation/注册 metadata；`finalize` 只按中心 plan/注册结果生成受保护 service.json。中心 MUST NOT 生成或读取 Gateway 私钥，也不启动 Gateway 进程；TLS 证书签发、进程管理和离线冷启动仍由可信部署和既有运行配置负责。`gateway-rotate` 成功只表示中心完成了状态转换和初始化投递，不表示 Repository READY、真实备份成功或部署验收通过。

## 验证

按用户要求本轮暂不新增/改写测试；编译、既有测试、race、lint 与运行验证 MUST 仅在 GitHub Actions 执行。新增轮换/恢复行为在 M4 退出前仍需专门验收：正常撤回→终结记录→释放→下一运行初始化；超时/丢失回执保留 fence；到期前拒绝；恢复只在到期后生效且不产生清理记录；旧 owner 不可复活；并发 worker 拒绝。绿色既有套件不能替代这些负向和运行证据。

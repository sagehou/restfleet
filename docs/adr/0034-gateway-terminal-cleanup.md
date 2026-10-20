# Gateway 停止后的可信清理与占用释放

## 问题

生产 Gateway 已能取消并 join 工作，但退出时回放循环随运行 context 停止，最后的审计/刷新可能尚未送达；中心也没有可核验的清理完成证据。队列排空、回执、授权过期或进程不可达均不能替代清理证明，直接释放会与旧数据面或刷新写入竞争。

## 决定

MUST 复用现有加密签名 Queue、RFGR、注册来源与 PostgreSQL 事务，不新增身份、凭据、通道、服务依赖或 schema。生产 `RunService` 承担可信 Gateway 停止协调；嵌入式 `StartService` 的调用方继续负责显式清理/封存，不自动生成证明。

- Gateway MUST 先关闭所有入口，取消并 join 会话、HTTP handler、子进程组、授权通道、刷新/审计 callback 与回放循环；owner MUST 清零云端配置并冻结队列。
- rclone 退出后 MUST 在独立最多 3s context 下同步最后配置，只允许原有 token-only、provider 与授权校验。同步不确定、子进程组清理失败、临时材料未移除或 runtime 关闭失败 MUST 阻止清理证明。
- `Queue.Finish` 仅 MAY 在可信 daemon 已验证上述事实后调用。MUST 要求 fresh producer 已认领、队列已冻结、来源私钥仍在且未失败；只追加一次固定 `runtime_cleanup/joined` 记录。普通 Append/审计 callback MUST 拒绝生成该记录，Recover 只有公钥 MUST 无法生成。队列容量不足或不确定写入 MUST 保留 fence，不能丢弃前序记录为证明腾空间。
- 记录沿用既有 `audit` 或 `global_audit` kind、完整 header、签名域及密文编码，无新字段。仓库记录 MUST 使用最后已接受的普通 grant revision；不能借清理事件扩大访问权限。升级 MUST 先中心后 Gateway；旧 reader 拒绝新事件并保留原 wire，不降级为成功。
- daemon MUST 在清理证明可靠落盘后，以独立最多 1min context 先排空全局、再排空仓库队列；每帧继续最多 5s。失败 MUST 保留原密文和回执供既有 replay 命令精确重送，不重建 owner、重发材料或延长 grant。
- 中心 MUST 先验证预注册来源、完整 binding、签名/解密、连续 sequence、previous hash 与时间。仅固定终结记录 MAY 触发收尾；普通审计、到期、排空、回执或 Agent ACK 均不能触发。
- 仓库终结事务 MUST 在原 credential/admission/source 锁下，原子提交最后记录、明确撤回该 runtime 的剩余授权、来源封存、原占用释放、审计与 outbox。所有先前刷新必须已经按序提交。精确重复 MUST 返回原回执，不重复 effect；封存后新记录 MUST 拒绝。
- 全局终结事务只 MAY 封存自己的来源，不推断 Host/Repository、不释放仓库占用。Agent 配对、长期密钥和已接受配置 MUST 保留，runtime 撤回不等于吊销 Agent 身份。

中心信任的是已注册 Gateway 服务执行停止顺序后生成的签名终结记录；Gateway 本身仍在安全模型的中心高信任区域。此机制不抵御 Gateway 服务身份/来源私钥被攻陷，也不将 Agent 视为清理证明来源。

## 边界与验证

本变更接通已有 daemon 的可证明停止及可靠最终回写。崩溃、SIGKILL、磁盘损坏、队列已满或刷新不确定且没有终结记录时 MUST 继续保留占用，不能用 replay 输出补造证明。受控故障恢复、新 runtime 预置/初始化、期限前换运行与 Repository READY 仍需接通；禁止以旧文件自动重启。

按用户要求未新增或改写测试用例。编译、既有测试/vet/race/lint、镜像与其他运行检查 MUST 仅在 GitHub Actions 执行；现有旧入口夹具通过不证明新增终结流程已专门验收。REP-047–060 的相关边界与后续完整产品验收要求保留。

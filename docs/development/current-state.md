# 当前开发状态

更新：2026-11-06。执行范围、唯一 ACTIVE 和退出条件见[开发计划](../../IMPLEMENTATION_PLAN.md)。[仓库复核](repository-review-2026-11-06.md)记录本轮开发前的静态缺口，不代表当前分支仍缺少这些实现。

## 当前执行入口

**继续“备份可靠” #96。** 当前分支为 `feat/reliable-backup-delivery`，集中交付于[草稿 PR #127](https://github.com/sagehou/restfleet/pull/127)，使用 `Refs #96`。后续依赖仍为 **#96 → #98（恢复可用）→ #15（长期运行与发布）**；阶段尚未退出。

## 已合入基线与分支实现

`main` 的本轮核对基线为 `1700198`（#117，最近功能 #116），schema 27。原配置、调度、取消、崩溃对账、基础健康和快照缓存实现继续复用。下列改动位于 PR #127，尚未合入；schema 28 随该 PR 一起迁移。

| 链路 | 当前分支实现 |
|---|---|
| 配置与执行 | bbolt 原子激活配置、接受结果与 runtime；变更/恢复重算 next-run，未变计划保留时刻；手动/定时共用持久 I/O 容量；执行配置接入本机 Hook allowlist、资源限制和有界持久重试链 |
| 结果与健康 | 报告核验 Agent/Plan/revision/重试链与计数边界；终态不可覆盖；完整 summary、exit 3、无变化结果、健康时间单调更新及 Web 展示 |
| 日志与补报 | 固定字段脱敏事件、累计 10MiB 预算与关键事件预留；持久 ACK、精确重放、有界背压与重连补报；中心事务保存连续日志，Web 分页轮询 |
| 请求与审计 | Backup Now/Retry/Cancel 事务幂等及业务审计；同 key 同请求重放原响应，冲突拒绝；取消原因不持久化任意输入 |
| 中心快照读取 | 周期索引复用动态 READY 并遍历所有页；有 Gateway 占用时复用当前 owner 的短期独立 mTLS reader，不启动第二个 rclone；读任务核验持久 binding，失效后失败收尾；拒绝畸形索引和冲突 managed tags |

关键设计见 ADR-0038–0042。真实贯通测试推动修复了 Agent 仓库地址缺少 `rest:`、stderr 警告漏计和取消上传误停 Gateway owner 的问题。请求审计使用可信服务/会话生命周期，客户端断线不取消可靠写入，服务关闭仍取消并 join；真实落盘故障继续停止授权。

安全重试接入固定的网络不可用/限流分类：未准入响应或会话清理 204 才能提供可信提示，数据路径只观察后端只读故障，未知/权限错误、写入/删除不确定及已提交 snapshot 会抑制提示。Agent 仅对没有 summary/snapshot/源读取错误的 exit 1 采用该分类，沿既有次数、退避、parent/root、deadline 和下一调度窗口重试；不解析任意 stderr，不改变 fence 或锁权限。新增行为的验证以 PR 当前提交检查为准。

## Actions 证据

所有编译、格式化、代码生成、测试和运行验证只在 GitHub Actions 执行，工作区只做源码/文档编辑和静态检查。

- 基线 [run 39619788725](https://github.com/sagehou/restfleet/actions/runs/39619788725) 成功，仅证明旧基线。
- `defc3d6` 的 [run 42170110931](https://github.com/sagehou/restfleet/actions/runs/42170110931) 完整通过：lint、迁移、普通/race 测试、构建/交叉构建、六个应用镜像、双架构 migrator、服务 UID 隔离、依赖审计、gitleaks 和生成产物检查。
- 安全重试提交 `670ffd4` 的 [run 42171751669](https://github.com/sagehou/restfleet/actions/runs/42171751669) 通过以上功能、race、构建、镜像和扫描检查，仅最后的生成产物一致性因 Go 格式补丁未提交而失败；Actions 补丁已回填到同一 PR。当前 HEAD 的完整结果以 [PR 检查](https://github.com/sagehou/restfleet/pull/127/checks) 为准，不将该轮称为完整绿色。
- 该 run 的真实贯通测试使用本地替代云 remote、固定 Restic/rclone 和生产 Agent/Gateway 路径，覆盖离线调度、无变化、exit 3、传输中取消、bbolt 重开、丢失结果/日志 ACK 后的精确补报，以及同一 owner 下的中心 index/list。真实 TLS 客户端断线及可信服务关闭的审计回归也通过。
- gRPC 故障 peer 只验证传输与补报；生产授权、数据库事务和撤回收尾由独立 PostgreSQL 集成测试覆盖，不能宣称运行过完整部署态 API/DB 故障演练。

## 剩余工作

| 归属 | 仍需完成 |
|---|---|
| #96 备份可靠 | 核对配置/手动与定时执行、页面及阶段验收证据；补完整部署态 API/DB 中断证据；阶段退出前确认 PR 当前 HEAD 全部门禁 |
| #98 恢复可用 | 快照列表分页/filter、cache tree/目录资源边界、单文件下载和 staging restore；中心 snapshot index 输出仍有上限，超限失败而非假装完整索引 |
| #15 长期运行与发布 | policy CRUD/revision diff、完整 Attention Queue/指标、Gateway 运维收敛、保留维护、通知、灾备、部署及剩余发布验收 |

## GitHub 跟踪

复用 #96、#98、#15 为三个阶段主 issue，并建立原生 blocked-by 依赖。#95（原 M5）、#97（原 M7）的剩余范围转入 #96/#15，以 not_planned 关闭，表示跟踪合并而非功能完成。历史 issue/PR 保留，不再为原 M9–M12 或内部组件创建追踪 issue。

## 保留的边界与阻塞

- REP-016 真实三后端、部署态轮换/恢复及 DEP-005/006 继续按既有决定暂缓，不在 GitHub 保存真实云凭据；集中留在 #15，不阻塞源码推进，也不视为通过或允许 V1 发布。
- Gateway 占用 fence、OAuth 刷新串行化与可信清理后释放 MUST 保留，不能通过删除检查接通中心读。授权、清理和备份成功仍分别验证。
- 自动重试只接受明确锁冲突、未创建会话的 busy，以及可信分类的 `GATEWAY_UNAVAILABLE` / `GATEWAY_RATE_LIMITED`。清理不确定 MUST 失败并保留 fence；rclone 折叠的 provider 错误不能伪称已精确分类。
- 新 capability 不下发 Gateway password；生产 daemon 无密码回退，旧协议兼容窗口仍需复核。
- 9 个 Dependabot PR（#118–#126）继续独立评审；spec 旧进度说明与历史例外仍按相关实现同批修订，不能据此宣称完成。
- 所有编译、测试、认证及运行验证只在 GitHub Actions 执行；开发工作区只做源码/文档编辑和只读检查。

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

关键设计见 ADR-0038–0041。Agent 的 Restic 仓库地址已补齐 `rest:` 前缀；该问题由真实贯通测试发现，不能用旧单元测试代替修复后的验证。

## Actions 证据

所有编译、格式化、代码生成、测试和运行验证只在 GitHub Actions 执行，工作区只做源码/文档编辑和静态检查。

- 基线 [run 39619788725](https://github.com/sagehou/restfleet/actions/runs/39619788725) 成功，仅证明旧基线。
- `2553da5` 的 [run 42164921535](https://github.com/sagehou/restfleet/actions/runs/42164921535) 中，lint、迁移、普通测试、race、构建/交叉构建、镜像和服务 UID 隔离通过。整轮失败于 gitleaks 读取了之后推送的 PR head、checkout 中不存在相应 revision，不能称整轮绿色。
- `916936e` 的真实 Agent 测试发现仓库地址错误。[run 42165511184](https://github.com/sagehou/restfleet/actions/runs/42165511184) 的普通测试失败，后续 race/build 未执行。
- 修复后的贯通测试使用本地替代云 remote、固定 Restic/rclone 和生产 Agent/Gateway 路径，覆盖离线调度、无变化、exit 3、传输中取消、bbolt 重开、丢失结果/日志 ACK 后的精确补报，以及同一 owner 下的中心 index/list。最终结果与完整 CI 待回填，尚不能作为通过证据。
- gRPC 故障 peer 只验证传输与补报；生产授权、数据库事务和撤回收尾由独立 PostgreSQL 集成测试覆盖，不能宣称运行过完整部署态 API/DB 故障演练。

## 剩余工作

| 归属 | 仍需完成 |
|---|---|
| #96 备份可靠 | 修复并完成最新 HEAD 的真实贯通测试、格式产物回填和完整 CI；补齐网络/429/5xx/provider 限流的安全重试分类；核对配置/手动与定时执行、页面及阶段验收证据 |
| #98 恢复可用 | 快照列表分页/filter、cache tree/目录资源边界、单文件下载和 staging restore；中心 snapshot index 输出仍有上限，超限失败而非假装完整索引 |
| #15 长期运行与发布 | policy CRUD/revision diff、完整 Attention Queue/指标、Gateway 运维收敛、保留维护、通知、灾备、部署及剩余发布验收 |

## GitHub 跟踪

复用 #96、#98、#15 为三个阶段主 issue，并建立原生 blocked-by 依赖。#95（原 M5）、#97（原 M7）的剩余范围转入 #96/#15，以 not_planned 关闭，表示跟踪合并而非功能完成。历史 issue/PR 保留，不再为原 M9–M12 或内部组件创建追踪 issue。

## 保留的边界与阻塞

- REP-016 真实三后端、部署态轮换/恢复及 DEP-005/006 继续按既有决定暂缓，不在 GitHub 保存真实云凭据；集中留在 #15，不阻塞源码推进，也不视为通过或允许 V1 发布。
- Gateway 占用 fence、OAuth 刷新串行化与可信清理后释放 MUST 保留，不能通过删除检查接通中心读。授权、清理和备份成功仍分别验证。
- 自动重试当前只接受明确 `REPOSITORY_LOCKED` 与确认未创建会话的 `GATEWAY_BUSY`；清理不确定 MUST 失败并保留 fence，不能从任意 stderr 猜测错误后重跑。
- 新 capability 不下发 Gateway password；生产 daemon 无密码回退，旧协议兼容窗口仍需复核。
- 9 个 Dependabot PR（#118–#126）继续独立评审；spec 旧进度说明与历史例外仍按相关实现同批修订，不能据此宣称完成。
- 所有编译、测试、认证及运行验证只在 GitHub Actions 执行；开发工作区只做源码/文档编辑和只读检查。

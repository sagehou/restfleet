# RestFleet 开发计划

更新：2026-11-06。目标是让管理员持续获得安全隔离、状态可信、能够恢复的 VPS 备份，完整要求见[产品规范](docs/spec/00-product.md)和[V1 范围](docs/spec/11-v1-scope.md)。

本计划把剩余开发合并为三个产品交付阶段，即当前的三个 Milestone。M0–M12 改为历史范围标识，不再逐个排队开工或退出；原范围及验收引用保存在[历史计划](docs/development/implementation-plan-m0-m12.md)。已实现的能力直接复用，未完成的能力按下表归并，V1 范围和安全要求继续有效。

## 执行总览

| 阶段 | 状态 | 一次完整交付 | 唯一主 issue |
|---|---|---|---|
| 备份可靠 | **ACTIVE** | 接入、配置接受、本地调度、备份执行、结果/日志/健康、快照元数据查询贯通 | [#96](https://github.com/sagehou/restfleet/issues/96) |
| 恢复可用 | QUEUED，依赖 #96 | 从历史快照找到文件，完成单文件下载及 Agent staging restore | [#98](https://github.com/sagehou/restfleet/issues/98) |
| 长期运行与发布 | QUEUED，依赖 #98 | 保留/维护、Gateway 运维、看板/通知、灾备、部署与完整 V1 验收 | [#15](https://github.com/sagehou/restfleet/issues/15) |

## 工作方式

- MUST 每次只推进一个 ACTIVE 阶段。阶段内跨 Server、Gateway、Agent、Web、契约和测试连续完成整条链路，内部组件不另立阶段或 issue。
- 一个阶段 MAY 包含多个 PR，按改动的完整性和可审阅性拆分；不预设逐文件、逐接口、逐原语的串行 PR 队列。中间检查点用于报告进度，不要求用户逐项批准后才继续。
- PR MUST 用 `Refs #阶段主issue` 关联，并说明用户行为、适用的原 Acceptance IDs、Actions 证据及剩余项。只有完成阶段退出条件的收尾 PR 才使用 `Closes #阶段主issue`。
- 发现影响当前交付的缺口 MUST 在本阶段修复；不影响当前结果的既有范围归入后续阶段。安全/协议/API/schema 变化所需契约、ADR 和正负测试 MUST 与实现一起完成，不另开设计或基础原语里程碑。
- 编译、测试、认证及运行验证 MUST 仅在 GitHub Actions 执行；开发工作区只编辑源码/文档并做只读检查。已合入源码、旧 CI 绿色、专项验收通过 MUST 分开记录。
- 一个阶段只保留一个主 issue，维护交付范围、退出证据和真实阻塞。阶段仅在实现及对应验收完成后关闭为 completed；跟踪合并使用 not_planned 并注明承接 issue。

## 备份可靠

**ACTIVE · #96。** 复用现有配对、配置、runner、任务表和 Gateway 安全协议，完成从配置到可核对备份结果的产品链路。

交付范围：

- 配置与调度一致：Plan revision/accepted revision、本地 runtime 和 next-run 对账，cron/时区、暂停恢复、misfire、去重；Hooks、重试与资源/I/O 限制真实进入执行链路。
- 执行与反馈可信：定时/手动备份、取消/重试、崩溃恢复、Operation 终态、统计、部分成功、有界脱敏日志、结果/日志背压及离线补报；业务与敏感操作审计保持原子性。
- 后端与界面可用：统一 READY 查询入口，接通周期索引与中心读取的可信协调，展示快照元数据和基础 Backup Health，保留 Agent Health 与备份健康的区别。

退出证据：

- 配置变更后按新计划执行；手动及定时备份的 Operation、统计、快照和页面状态一致，exit 3 显示警告。
- Actions 中以真实 Agent/Gateway 和固定 Restic/rclone 验证控制 API/DB 中断后的授权窗口内备份、重连补报、重复投递、取消及进程重启。
- 跨 Host、历史对象删除/覆盖、错误 TLS、秘密泄漏及日志资源越界被拒绝或受控处理。Gateway 占用 fence、OAuth 刷新单写者和可信清理后释放继续有效。

验收沿用 SCH-001–006、AGT-003–008、BAK-001–012、API-001/002/003/006、DR-001–003、OBS-001/007/008、PERF-004，以及本链路适用的 REP 安全验收；REP-016 的真实后端部署验收留在发布阶段。

## 恢复可用

**QUEUED · #98，blocked by #96。** 将快照浏览、单文件下载和 staging restore 作为同一个恢复交付完成。

交付范围：

- 快照列表/filter/分页、managed/unmanaged 与 missing 语义、目录 cursor/cache tree 校验、受限扫描和大目录页面；复用前阶段的中心读协调。
- regular-file download：短期一次性 intent、路径/权限校验、流式下载、取消、并发限制、Operation 与 fail-closed 审计。
- Agent staging restore：preview/confirm、快照和 Host 绑定、受控 staging、overwrite NEVER、进度/取消及结果核对；Docker 保持备份挂载只读。

退出证据：

- 从前阶段实际产生的备份中找到文件，完成下载与 staging restore，并核对内容/hash；运行中取消及 partial staging 的状态可解释。
- 跨 Host、越界路径、symlink 逃逸、过期/重放 intent、陈旧 preview 和审计失败的负向验收通过。
- 10k 快照分页及 1M entries 扫描保持有界，页面可用；不能用展示前若干条冒充完整浏览。

验收沿用 SNP-001–009、RST-001–008、API-004、OBS-005、PERF-003/005 和既定 1M-entry 资源要求。

## 长期运行与发布

**QUEUED · #15，blocked by #98。** 将长期运维能力与发布验收集中收尾，当前阻断备份/读取的问题仍归 #96，不能等待本阶段。

交付范围：

- Policy CRUD、retention preview/安全 forget、check/prune/stale unlock、调度与仓库互斥；维护仅在中心执行，保护其他 Plan 和 unmanaged 快照。
- Gateway 日常生命周期收敛：复用既有服务身份/协议，由单一生产协调路径管理准备、材料交接、授权续期、正常停止/轮换；无法证明清理的故障继续受控恢复。明确升级兼容窗口，不新增通用编排系统。
- 完整 Attention Queue/分项指标、revision diff、Gotify 与 signed Webhook、告警去重/恢复、审计验证/导出、诊断脱敏，以及生产部署、升级、控制面灾备与发布产物。

退出证据：

- 保留与维护的预览、确认、冲突、中断恢复及敏感操作审计通过；管理员有可执行的故障处理路径。
- OneDrive、Google Drive、HTTPS WebDAV，以及 amd64/arm64、Native/Docker 完成适用验收；控制面灾备、升级兼容、资源/性能和安全审查具有证据。
- 按验收规范生成完整 V1 报告，包含每个 P0/P1 的结果和证据；P0 skipped/failed 阻止发布，P1 例外遵循规范的明确风险接受要求。

验收覆盖 MNT-001–010、OBS-001–009、DR-004–006、DEP-001–008、PERF-001–005、REP-001–060 的剩余/回归项及全部 V1 发布门禁。

REP-016、部署态轮换/恢复及 DEP-005/006 继续按既有决定暂缓，不在 GitHub 保存真实云凭据；不阻塞前两阶段源码交付，也不视为通过。未来运行验证仍只在 GitHub Actions 的受控环境执行，回填脱敏证据。未解除的发布门禁 MUST 留在 #15。

## 原范围与 GitHub 迁移

| 原范围 | 新归属 |
|---|---|
| M0–M3 | 已合入基线；相关变更执行回归，最终发布复核 |
| M4 | 备份/中心读阻塞归 #96；生命周期易用性、兼容与剩余部署验收归 #15 |
| M5 | 配置与调度归 #96；policy CRUD、revision diff 归 #15 |
| M6 | 全部归 #96 |
| M7 | 基础健康与正确性归 #96；完整看板/Attention Queue 和规模验收归 #15 |
| M8 | 元数据索引和读协调归 #96；文件浏览、分页、缓存及大目录验收归 #98 |
| M9–M10 | 全部归 #98 |
| M11–M12 | 全部归 #15 |

复用并重命名 #96、#98、#15，建立原生依赖 **#96 → #98 → #15**；#95 的配置/调度由 #96 承接，其余管理体验由 #15 承接；#97 的基础健康由 #96 承接、完整看板由 #15 承接。#95、#97 以跟踪合并关闭为 not_planned，关闭不表示功能完成。旧编号不删除、不重建，不再为 M9–M12 另建主 issue。

当前基线与静态缺口见[接手入口](docs/development/current-state.md)和[仓库复核](docs/development/repository-review-2026-11-06.md)。规范和原 Acceptance IDs 是功能要求来源，历史批次只用于追溯。

# 仓库复核（2026-11-06）

本文保存计划重排前的静态盘点事实。执行队列已收敛为[三个交付阶段](../../IMPLEMENTATION_PLAN.md)，唯一 ACTIVE 为 #96；下文 M0–M12 用于历史范围定位，不再代表独立执行阶段。

## 结论与依据

当前处于备份与快照主链路的实现、集成收敛阶段，尚未达到 V1 发布条件。M0–M3 沿用历史 COMPLETE；M4 的主要源码已合入、部分部署验收暂缓；M5–M8 均有实现，但仍有功能缺口，不能只描述为“等待补测试”；M9–M12 未开工。本次未重新验收 M0–M3。

- 只读核对源码、迁移、OpenAPI/protobuf、测试源码、规范、计划及 GitHub 元数据；未在开发工作区编译、运行测试、启动服务或执行认证。
- 本地 HEAD 与 GitHub `main` 均为 `1700198c9431ec70a64d5d1fd43d152565ea90ff`（#117，2026-10-21）；最近功能提交为 `dc1633f`（#116）。盘点开始时工作区干净。
- 当前 HEAD 的项目 [CI run 39619788725](https://github.com/sagehou/restfleet/actions/runs/39619788725) 于 2026-10-21 触发，10 个 job 全部 success，包含质量门禁、跨 UID 隔离、依赖审计、六个架构/程序镜像及 migrator 镜像。2026-11-05 的 Dependabot 更新任务不是新的项目功能验收。
- 数据库有 27 个 migration，`postgres.ExpectedSchemaVersion = 27`。
- 盘点开始时 GitHub 有 5 个 open 里程碑 issue（#15、#95–#98）及 9 个 open Dependabot PR（#118–#126）；远端为 `main` 加 9 个依赖分支。本地远端引用不完整，不能据此宣称远端只有 `main`。

CI 绿色证明已有门禁通过，不能替代未覆盖的生产链路、真实后端或资源上限验收。下文实现问题是静态复核结论，未作运行复现。

## 能力与完成度

| 阶段 | 已有实现 | 仍需交付或验证 |
|---|---|---|
| M0–M3 | 工程骨架、Web 认证、Enrollment/mTLS、心跳/库存/配置同步 | 本次沿用既有完成记录 |
| M4 | 三后端配置校验、凭据管理、仓库初始化、mTLS Gateway、授权/可靠回写、轮换恢复命令、READY 读模型 | 真实三后端、部署态轮换/恢复及 DEP-005/006 继续暂缓；旧 capability 凭据交付兼容需评审 |
| M5 | Template/Plan revision、状态机、配置下发、本地 cron/时区/jitter/misfire | 配置变更与调度 runtime 对账、策略 CRUD、revision diff 等既定交付及 SCH/AGT/API 验收；不能仅以调度核心存在认定完成 |
| M6 | Restic runner、手动/定时备份、结果确认、取消、崩溃恢复 | Hooks、自动重试、资源限制、结果背压、日志流及敏感动作审计原子性；完整离线备份验收 |
| M7 | Plan 健康、next-run/last-success、汇总计数 | Attention Queue、分项指标、健康转换与性能验收 |
| M8 | 索引适配器/jobs、managed/UNMANAGED、missing marker、目录扫描缓存和条目 cursor | 生产索引入口与读窗口接线、快照列表分页/filter、tree 校验、目录虚拟化和资源上限验收 |
| M9–M12 | 尚无对应完整交付 | 下载、staging restore、维护/保留、通知、审计强化与发布闭环 |

## 优先处理的实现缺口

### M8：周期索引没有接上生产可用状态与中心读窗口（P1）

`internal/server/repository.go` 的 `repositoryAvailability` 与 `internal/domain/repository.go` 的 `WithGatewayObservation` 在读取时计算 READY。[初始化成功路径](../../internal/persistence/postgres/repository_jobs.go)只写初始化 metadata，不把持久状态改为 READY；[ReadyRepositoryIDs](../../internal/persistence/postgres/snapshot_index.go) 却只选择数据库 `status='READY'`。因此，正常创建并初始化的仓库即使页面显示 READY，也不会被当前周期查询选中。

另一个独立前置条件是：`EnqueueSnapshotIndex` 与 `EnqueueSnapshotList` 都调用 `ensureNoBackupAdmission`，而 READY 的有效 Gateway 观察要求未释放占用。生产 Gateway 持续运行期间，即使没有活动备份会话，冷缓存浏览/索引仍会被占用拒绝。当前尚无把这些中心读任务接入可信停止、清理、释放窗口的产品流程。

现有 `TestSnapshotIndexMarksManagedAndMissing`、`TestSnapshotListCompletesForInitializedRepository` 测试源码直接把仓库状态写成 READY，不能证明上述生产入口可达。后续 MUST 保留占用 fence 和 token refresh 串行化，不能通过直接删除占用检查、手改仓库状态或把到期视作清理成功修复。若调整既有信任/并发边界，MUST 同批更新规范与 ADR。

### M5：新调度配置继续使用旧 next_run_at（P1）

[ApplyDesiredState](../../internal/agent/config.go) 切换配置时没有对账 Plan runtime；`PlanRuntime` 只保存 Plan ID 与运行时间，不记录配置 revision。[advancePlan](../../internal/agent/scheduler.go) 只在 runtime 缺失或 next-run 为空时重算首次执行时间。

例如，已有每日计划的未来 next-run 改成每分钟执行，接受新 revision 后仍会等待原来的 next-run。暂停再恢复也会复用旧 runtime。后续 MUST 明确变更生效与 misfire 语义，保留重启去重和已运行任务，不以清空全部本地状态解决。归属 SCH-001–006、AGT-003/004、API-003。

### M5/M6：存档配置不等于执行能力（P1）

`internal/domain/template.go` 接受并保存 `RetryPolicy`、`ResourceLimits`、pre/post hook IDs；`PlanSchedule` 与 protobuf 的调度子集没有携带这些执行配置。当前 `BackupExecutor.Execute` 直接运行 Restic，未接本地 Hook allowlist 或自动退避重试；调度器也没有使用下发的 `MaxParallelIOJobs` 限制跨 Plan 执行。Gateway 的单仓库会话互斥不能代替完整调度策略。

后续 MUST 落实规范要求的执行行为，或在能力校验处明确拒绝未支持的非默认配置，不能静默接收后忽略。M5 的 retention/maintenance policy CRUD、Web revision diff 也不能从剩余范围中消失；完整范围仍见实施计划。

### M6：结果/日志与审计尚未闭环（P1）

- `internal/agent/report.go` 每轮取出并发送全部可报告记录，等待 ACK 的结果会再次全量发送；Hello 的 100 个 pending ID 上限不等于发送背压。
- protobuf 尚无 `JobLogChunk`；离线日志缓冲、sequence ACK/去重、双层脱敏、每 Operation 10 MiB 限额与 truncation marker 尚未接入。归属 AGT-006/008、OBS-007 与 M6 日志交付。
- `internal/server/operations.go` 的 Backup Now、Retry、Cancel 先提交业务变更，再单独调用 `RecordAudit`；第二次提交失败时，动作可能已经可执行或生效。后续 MUST 覆盖审计失败路径并将相应业务变更与审计原子提交。

### M8：分页存在，但列表与大目录交付仍不完整（P1）

- #116 已实现目录条目 cursor/continuation；`ListSnapshots` 和 `Store.Snapshots` 仍只有 limit，没有快照列表 cursor/filter，Web 固定请求前 100 条。不能把“条目分页”写成“所有快照列表分页已完成”。
- `SnapshotListResult` 按 repository/snapshot/path/cursor/TTL 选缓存，未与当前 snapshot tree 校验。
- `parseListJSON` 每页重新从头扫描，再过滤 cursor；超过 node 预算后的条目不能据此保证可继续到达。前端“加载更多”持续追加 DOM，无虚拟化。
- 已有完整成功索引后写 `missing_at`、重新出现清除 marker、列表不立即排除 missing 行的基础；尚无完整 two-pass 专项验收，不能既称未实现，又称已全部验收。

## 纠正旧盘点中的判断

| 旧说法 | 本次核对 |
|---|---|
| DB/OpenAPI 不支持 NEVER_RUN、PAUSED | 已由 migration 25 与 OpenAPI 枚举补齐；不是当前缺陷 |
| 不健康计数只包含 FAILED | 当前已覆盖 FAILED/OVERDUE/NEVER_RUN；缺口是分项指标与 Attention Queue |
| Plans 页面仍说等待调度接入 | 该提示已经移除 |
| 周期索引、UNMANAGED、条目 cursor 尚待开始 | #114/#116 已有实现；剩余是接线、边界与验收 |
| 启动 metadata 零值把实际授权固定为 72h | `GatewayStartupConfig.Validate` 中的默认值仅用于临时校验；实际 `decision()` 保留零值，由 `DecideGatewayAuthorization` 使用配置窗口 |
| 占用长于离线授权窗口本身违反规范 | ADR-0032 与数据库规范 §5.4.4 允许原占用最长 30d，授权受配置及原占用限制；占用不是授权有效期，二者不同不能直接判为漏洞 |
| 旧模式下发 Gateway password 直接违反 AGENTS 明文禁令 | 当前 AGENTS 没有该项明文禁令；协议 §13.1 明确保留 legacy 两密码交付，新 capability 不下发密码，生产 daemon 无密码回退。兼容策略仍需评审，但不能据此声称已证明云凭据泄露或生产认证回退 |

## 计划、规范与约定的漂移（盘点开始时）

1. 计划顶部与末尾执行模板仍要求只推进 M4 delivery 2，与已合入的 M5–M8 不符；也没有贡献指南要求的唯一 ACTIVE 标记。
2. 历史“暂不新增测试”被写成当前指令，与现行 AGENTS 的测试要求冲突。历史记录可保留，但不能继续作为新改动跳过测试的依据；执行环境仍仅限 GitHub Actions。
3. README 的 schema v16 已落后于代码 v27；贡献指南的“提交前运行 make”没有说明仅在 Actions 中执行。
4. 安全模型 §8.3/8.5/8.7/9 与部署规范 §7 的部分“尚未接线”说明属于旧批次进度，后续 ADR 已有实现。规范 SHOULD 保留稳定要求，当前实现与验收状态由 `current-state.md` 记录；不能因删掉“待实现”文字就标记验收通过。
5. AGENTS 的公共 ID 全用 UUIDv7 与 ADR-0037/HTTP API 规范的定时 `operation_id` UUIDv5 例外尚未统一；ADR-0037 还误将 Snapshot 列为 UUIDv7。真实 Snapshot ID 始终是 Restic SHA-256。本次仅记录冲突，不修改身份/ID 契约。
6. M8 issue #98 的开头仍引用 `88c8625` 并称扫描 P0 未修复，而勾选项已记 #99/#114/#116 完成。GitHub issue 正文后续需要同步；本次只读取，没有发布或修改 issue/PR。

初次盘点修正第 1–3 项；随后按用户要求重新制定三个阶段的开发计划，并同步修正第 6 项的 GitHub 跟踪正文、合并范围及依赖。第 4–5 项在相关实现中继续处理，本轮没有修改安全/ID 契约。

## 重排后的交付归属

- #96“备份可靠”承接配置/调度、执行/反馈、基础健康和索引/中心读协调，当前连续完成这一交付。
- #98“恢复可用”承接目录浏览、下载和 staging restore，依赖 #96；#15“长期运行与发布”承接维护、完整运维和发布，依赖 #98。
- #95/#97 的未完范围转入上述主 issue，以 not_planned 关闭跟踪，不视为功能完成；原验收 ID 与范围继续有效。
- 真实云端/部署验收按既有决定继续暂缓，不在 GitHub 保存真实云凭据；剩余发布门禁集中留在 #15。9 个依赖 PR 单独评审，本次不合并、不升级工具链。

本轮只修订状态和开发入口，没有修复以上实现问题。后续能力完成 MUST 同时具备源码、契约/规范、适用正负测试及对应 Actions/验收证据。

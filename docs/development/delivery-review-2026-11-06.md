# RestFleet 三轮交付审查

审查日期：2026-11-06。初审源码基准：`a0cceb9054a87e4068c502f98da48918ce2095ed`，分支 `feat/reliable-backup-delivery`，[PR #127](https://github.com/sagehou/restfleet/pull/127)。下文问题描述和源码行号对应初审基准；后续源码修正与编译交付仍在同一 PR。

**结论：尚未达到交付标准，当前“备份可靠”阶段也不能退出。** 可以继续在 GitHub Actions 的受控环境做研发联调和故障测试；不能据此宣称全部功能完成、V1 验收通过或生产可用。

## 后续源码修正

R1–R10 已按本次审查完成源码修正：原子发布与陈旧状态校验、执行者先于 ACK 注册、手动任务与控制流解耦、重连接受结果补报、IANA 时区内嵌、WARNING 健康迁移、正常轮询与 misfire 区分、详情及分页轮询、共享路径校验。schema 29、OpenAPI 与 ADR-0043 同步更新；生成代码和编译由 Actions 执行，结果以 PR 当前提交为准。本记录保留缺陷的初审证据，源码修正不等于全部阶段验收已经通过。

API-006 已在 ADR-0043 和规范中明确为完整状态轮询恢复，SSE 留作可选扩展；原验收口径差异已记录，不能据此宣称运行验收完成。

## 三轮复核范围

| 轮次 | 复核内容 | 结果 |
|---|---|---|
| 第一轮：需求与实现 | 产品目标、安全模型、当前阶段、REST/protobuf 契约、生产入口、页面和部署文件 | 区分当前备份链路与尚未开发的恢复、维护及发布范围，核对实现和规范是否一致 |
| 第二轮：运行逻辑 | 配置下发、调度、手动任务、重试/取消、结果/日志补报、数据库事务、Gateway 隔离与中心读取 | 沿调用链检查提交窗口、上下文生命周期、幂等、资源占用和故障恢复，形成候选问题 |
| 第三轮：反证与验收 | 搜索补偿路径，回查现有测试、Actions、容器产物和页面状态更新 | 排除已有安全设计及兼容逻辑可以解释的候选项，确认下列问题，区分静态结论与运行证据 |

覆盖 Server、Agent、Gateway、持久层、Restic/rclone 适配、Web、契约、部署与验收文档的主要入口和交付链路；不以三轮审查代替逐项动态验收，也不声称每行代码均经形式化验证。

审查等级：P1 表示必须优先修复的交付阻断；P2 表示需要修正的配置边界或用户状态问题。它们与验收规范中的 P0/P1 是不同的分级，下文保留相关原验收编号。

## P1：当前备份交付阻断

### R1：Plan 变更与配置发布分两次提交，可能丢失下发

- 触发：Plan 创建、修改、应用、暂停或恢复的业务事务提交后，进程退出、请求上下文取消，或第二次数据库事务失败。
- 代码：例如 [planStatus](../../internal/server/plan.go#L232) 先保存状态，再调用 `republishPlanState`；[PublishAgentDesiredState](../../internal/persistence/postgres/plan.go#L242) 另开事务，才更新 DesiredState/revision 并插入 outbox。所有发布调用都来自变更请求，没有发现后台重建缺失发布的补偿路径。
- 后果：中心已经显示 PAUSED，Agent 仍持有包含 ACTIVE 计划的旧快照并继续调度；普通重连不会修复未递增的 desired revision。重复暂停也会被状态校验拒绝。
- 验收：架构 §5.1 要求业务 revision、审计和 outbox 同事务；对应 **DR-001、SCH-006**。
- 修正与回归：在业务变更事务内持久保存配置发布意图，使重启可恢复。Actions 注入提交后故障，验证重启后正确暂停/应用，且业务审计和发布不脱节；现有成功路径测试不能证明这个窗口安全。

### R2：JobAccepted 发送失败，留下没有执行者的 RUNNING

- 触发：Agent 接收手动任务后，本地 RUNNING 已落盘，但发送 `JobAccepted` 返回错误。
- 代码：[handleJobDispatch](../../internal/agent/client_stream.go#L485) 在错误分支直接返回，尚未调用 `executeOperation`；[重复投递分支](../../internal/agent/client_stream.go#L406) 只重发确认，不创建执行者。[RunningOperations](../../internal/agent/operations.go#L216) 持续计算该记录；恢复清理只在 [Run 启动](../../internal/agent/client.go#L175) 执行。
- 后果：任务没有启动却持续报告运行中。默认一个 I/O 槽位会被长期占用，后续手动和定时备份都受阻；普通断线重连不能恢复，需 Agent 进程重启等干预。
- 验收：**AGT-007** 的重复投递应只执行一次而非漏执行；现有 **DR-003** 启动恢复没有覆盖这个“进程未退出”的窗口。
- 修正与回归：协调持久准入与执行者注册，执行不能依赖确认消息成功发送；重复投递必须能处理无执行者的记录而不重复启动。Actions 用失败发送器覆盖该窗口，验证只执行一次、最终释放槽位，并验证取消能够结束它。现有 Agent 状态测试只覆盖显式启动恢复和取消标记。

### R3：发布镜像缺少 IANA 时区数据，默认时区配置不可用

- 触发：使用仓库提供的 scratch 镜像，创建或接受 `Asia/Shanghai` 等非 UTC 计划，且没有额外挂载时区数据。
- 代码：[runtime-base](../../deploy/Containerfile#L57) 只复制程序、Restic、CA 与 Web 文件，没有 zoneinfo；构建未启用 `timetzdata`，仓库也没有导入 `time/tzdata`。[模板验证](../../internal/domain/template.go#L132) 和 [Agent scheduler](../../internal/agent/schedule/schedule.go#L50) 依赖 `time.LoadLocation`。Web 模板默认值正是 [Asia/Shanghai](../../web/src/Templates.tsx#L125)。
- 后果：Server 镜像拒绝有效 IANA 时区；Docker Agent 也无法接受或调度相应配置。宿主机 Ubuntu 的测试通过不能证明 scratch 镜像具备时区数据库。
- 验收：**SCH-001、DEP-003/004**。
- 修正与回归：为交付程序内嵌时区数据或在运行镜像提供它；在 Actions 直接用最终镜像验证非 UTC 计划及跨日调度。本项由构建内容和标准库加载路径静态确认，尚未运行容器复现；现有镜像 job 只做 build。

### R4：配置接受消息在中心提交前丢失，Plan 接受状态不能对账

- 触发：Agent 已激活新配置，`stream.Send(ConfigAccepted)` 返回成功，但 Server 在持久化该消息前中断或事务失败。
- 代码：Agent [发送成功后立即清 pending](../../internal/agent/client_stream.go#L198)，重连 Hello 报告已经生效的 revision。Server 仅在 [Agent 报告 revision 落后](../../internal/server/agentgrpc/connect.go#L94) 时重发配置；[MarkAgentConnected](../../internal/persistence/postgres/fleet.go#L476) 和 [heartbeat](../../internal/persistence/postgres/agentstate.go#L91) 只对账 agents.accepted_revision。plans.accepted_revision 只在 [AcceptAgentConfig](../../internal/persistence/postgres/agentstate.go#L219) 更新。
- 后果：Agent 实际按新配置执行，中心 Plan 仍长期显示旧 accepted revision，配置错误字段也可能残留。后续普通重连和心跳不会补齐，不能可靠判断配置是否生效。
- 验收：**AGT-003** 及本阶段 desired/accepted/实际执行一致性。
- 修正与回归：每次重连重放当前已接受配置的 revision/hash，由中心核验对应持久快照并原子更新 Agent 与 Plans。Actions 覆盖发送成功但中心提交失败后重连。现有 `TestDesiredStateIsReplayedAfterServerRestart` 直接调用发送函数两次，pending 测试只验证本地存取，均不覆盖这个窗口。

### R5：控制连接结束会隐式取消已运行的手动备份

- 触发：手动备份正在执行，管理连接断线、Server 重启，或 Agent 正常完成证书轮换并重建连接；Gateway 数据路径仍可用。
- 代码：[connectOnce](../../internal/agent/client_stream.go#L20) 的连接级 ctx 在返回时取消；[手动 dispatch](../../internal/agent/client_stream.go#L500) 将它交给执行器；[BeginOperation](../../internal/agent/operations.go#L102) 继承该取消。证书轮换响应也会 [直接结束连接](../../internal/agent/client_stream.go#L238)。[BackupExecutor](../../internal/agent/backup.go#L178) 将取消记为 CANCELED，且重试执行链在 ctx 已取消时不继续。
- 后果：短暂管理连接故障会终止数据备份；管理员没有发送 cancel，也会收到 CANCELED。**本项只影响手动 dispatch，定时任务使用独立 scheduler ctx，不能混为一谈。**
- 修正与回归：手动作业使用 Agent 服务生命周期上下文，同时保留显式 cancel、deadline 和服务停止时的进程回收。在 Actions 用真实生产 gRPC 服务验证重连/轮换期间手动任务继续，而用户取消和 Agent 关闭仍有效。

### R6：部分成功被计为完全健康，Dashboard 隐藏告警

- 触发：Restic exit 3，生成快照但有文件未读成功。
- 代码：[结果入库](../../internal/persistence/postgres/operations.go#L164) 把 `SUCCEEDED_WITH_WARNINGS` 和 `SUCCEEDED` 一起投影为 HEALTHY；[看板聚合](../../internal/persistence/postgres/operations.go#L247) 不统计 warnings；[Dashboard](../../web/src/App.tsx#L393) 只有失败和不健康指标。
- 后果：操作列表可以正确显示“成功（有告警）”，但只有该次部分成功时，计划显示健康，看板失败/不健康计数均为零。管理员可能误以为备份完整。
- 验收：直接违反 **BAK-004（P0）** 的“Dashboard 不显示全绿”。
- 修正与回归：保留有快照的成功时间语义，同时把部分成功/告警贯穿聚合和页面。Actions 覆盖 exit 3 → 持久层 → Dashboard，而不只断言 runner 分类或操作详情文案。

## P2：配置边界与页面正确性

### R7：合法的零 misfire 宽限会把正常调度判为 MISSED

- API 和 Web 均允许 `misfire_grace_seconds=0`；[本地 scheduler](../../internal/agent/scheduler.go#L144) 对正常到点也使用 misfire 判断。
- [DecideMisfire](../../internal/agent/schedule/schedule.go#L144) 只要 `now > scheduled` 就在零宽限下判 MISSED；生产循环每 30s 唤醒，健康进程通常也会晚于计划时刻，因而可能持续跳过全部备份。
- 修正与回归：区分正常调度唤醒误差与停机补跑，明确零宽限表示的行为并同步契约。Actions 覆盖健康进程在计划时刻后少量延迟、停机超时以及零/小宽限；现有 scheduler 用 120s 宽限，不能覆盖此边界。

### R8：计划详情的“刷新详情”保留旧状态

- [Plans.load](../../web/src/Plans.tsx#L52) 只更新列表和 cursor；[reload](../../web/src/Plans.tsx#L142) 不重取或同步 selected，详情仍渲染旧对象。
- Agent ACK 后点刷新，accepted revision、健康和状态仍可能保持旧值；这与 R4 的后端对账问题是两条独立路径。
- 修正与回归：刷新选中的 Plan 详情并保留分页语义。Actions Web 测试验证服务端变化后按钮确实更新详情；目前没有 Plans 页面专项测试。

### R9：操作轮询重置分页，后页详情可能永远显示运行中

- [Operations.load](../../web/src/Operations.tsx#L51) 无 cursor 时替换成第一页，selected 仅在该页存在对应记录时更新；[定时刷新](../../web/src/Operations.tsx#L106) 只调用 `load()`，并根据当前列表是否有运行项决定继续。
- 加载第二页并打开运行任务后，轮询清掉第二页；若第一页均为终态，轮询停止，详情仍保留旧 RUNNING，也没有独立详情轮询。
- 修正与回归：按 ID 刷新已打开的未完成任务，保留已加载分页。Actions 覆盖两页、后页运行项完成、第一页全终态。现有 Operations 测试只覆盖单页结果和日志分页。
- 相关假设也需修正：中心当前按 operation UUID 倒序分页，定时任务使用 UUIDv5；第一页不能作为“最新任务或所有运行任务”的保证。

### R10：Agent 在激活配置前没有完成路径校验

- [PlanSchedule.Validate](../../internal/domain/agentstate.go#L212) 对 paths 仅检查非空，不检查每项绝对/规范路径及控制字符，也未校验 exclude patterns；[ApplyDesiredState](../../internal/agent/config.go#L59) 随后可以激活并确认该配置。
- 非法路径直到 [备份 Prepare](../../internal/agent/backup.go#L116) 才被拒绝，此时 last-known-good 已被替换，不满足 **AGT-004** 的激活前拒绝要求。
- 边界：正常 HTTP 模板/覆盖配置经过 [TemplateConfig.Validate](../../internal/domain/template.go#L81)，已有路径校验。本项是 Agent 协议输入防御缺口，不声称普通管理员可绕过 HTTP 校验，也没有据此确认跨 Host 数据暴露。
- 修正与回归：在共享配置验证中复用路径规则，非法新快照保留旧配置/runtime。Actions 加入 hash 正确但路径非法的 protobuf 负向用例，确认返回 ConfigRejected。

## 完整 V1 仍有未完成范围

| 归属 | 交付现状 |
|---|---|
| [#96 备份可靠](https://github.com/sagehou/restfleet/issues/96) | 主要备份链路已接入，但上述问题未修；完整部署态 API/DB 中断与 10 并发备份证据仍未闭合，不能关闭阶段 |
| [#98 恢复可用](https://github.com/sagehou/restfleet/issues/98) | 单文件下载、Agent staging restore 尚未实现；快照 filter/分页、目录 cursor/cache tree 和大目录完整浏览仍有缺口 |
| [#15 长期运行与发布](https://github.com/sagehou/restfleet/issues/15) | 保留/维护、完整 Attention Queue、通知、审计运维、升级/灾备、三种真实后端与多架构部署验收仍未完成 |

还有一处验收口径需要收敛：计划把 **API-006（SSE event ID 断线恢复）** 列入 #96，而 Web 规范允许退化 polling，当前 OpenAPI/handler 没有对应 events/SSE 接口。不能把 API-006 直接记为通过；应明确 polling 的完整状态恢复契约及验收映射，再据此决定是否确实需要 SSE，避免为验收编号增加不必要实现。

## Actions 证据及其边界

- 基准提交的 [run 42172707476](https://github.com/sagehou/restfleet/actions/runs/42172707476) 为 success。该证据对应审查的代码提交，并非旧分支结果。
- 现有 CI 覆盖 Go lint/vet、迁移、普通/race 测试、Web lint/typecheck/unit/build、交叉构建、应用/migrator 镜像构建、服务 UID 隔离、依赖审计、生成一致性、gitleaks 与 SBOM。
- 固定 Restic/rclone 的 [Agent/Gateway 贯通测试](../../internal/gateway/agent_conformance_linux_test.go#L41) 覆盖离线调度、无变化、exit 3、取消、bbolt 重开、结果/日志丢 ACK 后补报与中心读取。但云 remote 是本地 fixture，重连端是测试 gRPC peer；生产授权/数据库路径另有集成测试，不能合称完整部署态 API/DB 停机演练。
- [镜像 job](../../.github/workflows/ci.yml#L164) 只构建，Compose 检查只解析配置，均不等于 Native/Docker 的 enroll/config/backup/restore smoke。真实三后端、轮换/恢复、DEP-005/006 的既有暂缓也没有变成通过。
- 本次没有在工作区编译、测试、运行服务或启动数据库，也没有新增动态复现证据；以上缺陷来自三轮静态追踪和测试覆盖核对。CI 绿色证明现有检查通过，不能证明未覆盖的故障窗口正确。

## 修复顺序与退出条件

继续沿用 **#96 → #98 → #15**，不新增逐文件或逐函数的串行 issue。

1. 在 #96 修复 R1–R6，并同时完成 R7–R10 影响当前交付的边界与页面问题；相关回归直接随修复进入 Actions。
2. 用生产入口和最终镜像完成当前阶段故障验收，逐项回填配置接受、手动/定时、暂停/取消、重复投递、进程重启、API/DB 中断、exit 3、10 并发及页面一致性证据；API-006 的口径同步收敛。
3. #96 退出后完成 #98 的实际下载/恢复，再完成 #15。发布仍依据全部适用 P0/P1 及规定的风险接受流程，不能以 PR 绿色或“可以投入测试”替代交付验收。

本次保留的安全设计包括 Gateway fence、有限会话容量、可信清理后释放和旧结果字段兼容；没有把这些设计限制误列成故障修复任务。审查未覆盖真实云凭据环境、部署实机运行及性能实测，这些仍是发布风险和待完成验收。

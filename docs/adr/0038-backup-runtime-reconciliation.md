# 配置激活时原子对账 Agent 调度状态

## 决定

配置接受 MUST 在同一个 bbolt 事务内激活 DesiredState、保存接受结果并对账 PlanRuntime。PlanRuntime 新增可选 `plan_revision` 和 `config_hash`；新计划、配置变更和暂停后恢复 MUST 从接受时刻按新 cron/时区/jitter 计算下一次执行。未改变的计划保留已保存的时刻，移除计划删除待调度 runtime，已运行 Operation 和历史去重记录保留。

配置激活、本地调度和手动派发的本地准入 MUST 串行协调。它们共用持久 RUNNING 记录计算 `max_parallel_io_jobs`；达到容量时不启动新进程，手动作业保留中心 durable queue 等待重投，计划保留待执行时刻并继续遵守 misfire grace。每 Plan 的 FORBID 语义保持。

手动备份、重试和取消 MUST 在同一 PostgreSQL 事务中提交业务变更与审计。入队时重新锁定并核对 ACTIVE Plan、Agent、revision 和 config hash，禁止在读取后发生配置变化时排入旧配置作业。

Agent 报告和 JobAccepted/JobRejected MUST 使用 mTLS 导出的 Agent 身份校验 Operation 归属。已有 Operation 的 Agent/Plan/revision 绑定不可通过冲突 upsert 修改，INSERT 的冲突条件与行锁检查共同覆盖并发首次报告；跨绑定请求不能修改状态、事件或快照。

## 兼容与验证

旧 bbolt runtime 缺少新增字段时，读取仍兼容；接受下一份配置时由旧的已接受计划核对并补齐字段，不清空 Operation，不恢复已终结作业。本决定不改变 Gateway 占用、授权期限、清理证明或身份边界。

验收归 #96，沿用 SCH-001–006、AGT-003/004、API-003 和审计失败负向要求。覆盖新 cron 接受、未变计划保留时刻、移除计划、运行中作业保留及审计失败回滚；编译与运行验证只在 GitHub Actions。

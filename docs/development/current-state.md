# 当前开发状态

更新：2026-10-20。本文档是会话中断或记录丢失后的接手入口；状态以仓库提交、分支和 GitHub Actions 为准，不依赖聊天记录。

## 工作区

- 当前分支：`feat/m8-snapshot-index`
- HEAD：`973d195 feat: index backup snapshots and list them`（快照索引加固补丁在工作区，提交前）
- 与 `origin/main` 关系：领先 1 个提交；分支已推送，尚无 PR
- 工作区：干净（除本轮待提交改动）
- 本地构建/测试不在开发工作区执行，全部以 GitHub Actions 结果为准

## 里程碑

| 阶段 | 状态 | 说明 |
|---|---|---|
| M0–M3 | COMPLETE | 骨架、认证、Host/enrollment、心跳/期望状态 |
| M4 | IN PROGRESS | 三项交付源码全部合入（PR #67、#76）；生产 mTLS、轮换/恢复、续证/吊销、READY 降级、真实三后端运行验收未做 |
| M5 | IN PROGRESS | 模板、Plan、状态机、本地调度均已接入；计划仍将 Backup Health 记为待完成 |
| M6 | IN PROGRESS | restic argv/JSONL、本地调度与执行、Operation 持久化与页面已合入（#86–#89）；取消/重试、离线补报保序未完成 |
| M7 | IN PROGRESS | Plan 健康与仪表盘计数已合入（#90）；SLA/overdue、Attention Queue 等未完成 |
| M8 | IN PROGRESS | 成功备份的终态结果写入快照索引，`GET /api/v1/snapshots` 与快照列表页已实现；`restic ls` 文件树、cursor、虚拟化未完成 |
| M9–M12 | 未开工 | 下载、恢复、保留/维护、通知与发布 |

## 2026-10-20 已完成

- `#83`–`#90` 合入 `main`：模板/Plan、既定调度、restic runner、Job 协议、本地执行、Operation 持久化、Plan 健康与仪表盘计数。
- `973d195`：快照索引表、终态结果写索引、快照列表 API/页面。
- 快照写路径加固：终态结果与索引写入同一事务；索引失败不再被忽略，整体回滚等待 Agent 重放（见 `internal/persistence/postgres/operations.go`）。

## 待完成与接手顺序

1. **C（本轮）**：提交快照写路径加固；保持本文件为最新状态检查点。
2. **A**：为 `feat/m8-snapshot-index` 开 PR 并合并（合并需由维护者在 GitHub 操作）；随后实现 `restic ls --json` 适配、受限流式解析、快照目录 API 与浏览页面（SNP-001–005、API-004、PERF-005）。
3. **B**：M6 取消/重试语义与离线结果补报；先修正 Agent 在服务端确认前把结果标记为已上传的问题，再补保序与去重。

## 已知风险

- 生产运行验收全部欠账，M4 不能因源码合入标记完成。
- 当前快照索引只来自成功终态结果，尚未实现 `restic snapshots --json` 周期索引、UNMANAGED 映射与 two-pass missing 语义。
- 本环境无 `gh` 且禁止向 `main` 推送，PR 合并必须由维护者执行。

# 当前开发状态

更新：2026-10-20。本文档是会话中断或记录丢失后的接手入口；状态以仓库提交、分支和 GitHub Actions 为准，不依赖聊天记录。

## 工作区

- 当前分支：`fix/snapshot-index-atomic`
- HEAD：`0669ffd docs: record current development checkpoint`
- `main`：`45ae695 feat: index backup snapshots and list them (#91)`（快照索引已合并）
- 与 `origin/main` 关系：领先 2 个提交（快照写路径加固、本检查点）；尚未推送
- 工作区：干净
- 本地构建/测试不在开发工作区执行，全部以 GitHub Actions 结果为准

## 并行会话提示

- 2026-10-20 08:15 另一个会话在 `feat/m6-manual-dispatch` 上提交了 `adc711a feat: dispatch manual backups to agents`。该分支与本检查点无关，已恢复为远端状态。
- 本会话的加固提交曾误落到该分支，现已隔离到 `fix/snapshot-index-atomic`，没有混入 M6 改动。
- 提交前必须重新确认 `git branch --show-current` 与 `git log -1`，不要依赖会话开始时的分支。

## 里程碑

| 阶段 | 状态 | 说明 |
|---|---|---|
| M0–M3 | COMPLETE | 骨架、认证、Host/enrollment、心跳/期望状态 |
| M4 | IN PROGRESS | 三项交付源码全部合入（PR #67、#76）；生产 mTLS、轮换/恢复、续证/吊销、READY 降级、真实三后端运行验收未做 |
| M5 | IN PROGRESS | 模板、Plan、状态机、本地调度均已接入；计划仍将 Backup Health 记为待完成 |
| M6 | IN PROGRESS | restic argv/JSONL、本地调度与执行、Operation 持久化与页面已合入（#86–#89）；manual dispatch 在 `feat/m6-manual-dispatch` 进行中；取消/重试、离线补报保序未完成 |
| M7 | IN PROGRESS | Plan 健康与仪表盘计数已合入（#90）；SLA/overdue、Attention Queue 等未完成 |
| M8 | IN PROGRESS | 快照索引与列表已合入（#91）；`restic ls` 文件树、cursor、虚拟化未完成 |
| M9–M12 | 未开工 | 下载、恢复、保留/维护、通知与发布 |

## 2026-10-20 已完成

- `#83`–`#91` 合入 `main`：模板/Plan、既定调度、restic runner、Job 协议、本地执行、Operation 持久化、Plan 健康、快照索引与列表。
- `4fe9f6d`：终态结果与快照索引写入同一事务；索引失败不再被忽略，整体回滚等待 Agent 重放（见 `internal/persistence/postgres/operations.go`）。
- `0669ffd`：本检查点文档。

## 待完成与接手顺序

1. **A**：合并 `fix/snapshot-index-atomic`（维护者在 GitHub 操作）；随后实现 `restic ls --json` 适配、受限流式解析、快照目录 API 与浏览页面（SNP-001–005、API-004、PERF-005）。
2. **B**：M6 取消/重试语义与离线结果补报。`feat/m6-manual-dispatch` 已占用相邻代码，开工前先与维护者确认由哪个会话继续，避免重复实现。先修正 Agent 在服务端确认前把结果标记为已上传的问题，再补保序与去重。

## 已知风险

- 生产运行验收全部欠账，M4 不能因源码合入标记完成。
- 当前快照索引只来自成功终态结果，尚未实现 `restic snapshots --json` 周期索引、UNMANAGED 映射与 two-pass missing 语义。
- 本环境无 `gh` 且禁止向 `main` 推送，PR 合并必须由维护者执行。
- 同一工作区可能有并行会话切换分支；任何提交前都要重新核对分支与工作区。

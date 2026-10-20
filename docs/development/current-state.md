# 当前开发状态

更新：2026-10-20。本文档是会话中断或记录丢失后的接手入口；状态以仓库提交、分支和 GitHub Actions 为准，不依赖聊天记录。

## 工作区

- 当前分支：`feat/m6-operation-control`
- HEAD：`8b81f1e feat: cancel and retry backup operations with result acknowledgement`
- 分支包含三个叠加提交组：C 快照写路径加固 → A 快照浏览 → B M6 取消/重试与结果确认
- `main`：`45ae695 feat: index backup snapshots and list them (#91)`
- 工作区：干净
- 本地构建/测试不在开发工作区执行，全部以 GitHub Actions 结果为准

## 合并方式

只需为 `feat/m6-operation-control` 开一个 PR 合并到 `main`，即可同时带入 C、A、B。独立分支 `fix/snapshot-index-atomic`、`feat/m8-snapshot-lister` 是同一提交栈的前缀，可忽略或删除。

## 并行会话提示

- 2026-10-20 08:15 另一个会话提交了 `adc711a feat: dispatch manual backups to agents`，已作为 B 的第一部分合入本分支。
- 本会话的加固提交曾误落到该分支，现已隔离并叠加，没有代码丢失。
- 提交前必须重新确认 `git branch --show-current` 与 `git log -1`；同一工作区可能有并行会话。

## 里程碑

| 阶段 | 状态 | 说明 |
|---|---|---|
| M0–M3 | COMPLETE | 骨架、认证、Host/enrollment、心跳/期望状态 |
| M4 | IN PROGRESS | 三项交付源码全部合入（PR #67、#76）；生产 mTLS、轮换/恢复、续证/吊销、READY 降级、真实三后端运行验收未做 |
| M5 | IN PROGRESS | 模板、Plan、状态机、本地调度均已接入；计划仍将 Backup Health 记为待完成 |
| M6 | IN PROGRESS | 本地调度执行、Operation 持久化/页面、手动备份、取消/重试、终态先补报与 `JobResultAccepted` 已接入；Actions 验收待完成 |
| M7 | IN PROGRESS | Plan 健康与仪表盘计数已合入（#90）；SLA/overdue、Attention Queue 等未完成 |
| M8 | IN PROGRESS | 快照索引/列表（#91）、`restic ls --json` 有界扫描、`SNAPSHOT_LIST` 后台任务、目录 API 与浏览页面已接入；大规模虚拟化与 Actions 验收待完成 |
| M9–M12 | 未开工 | 下载、恢复、保留/维护、通知与发布 |

## 2026-10-20 已完成

- `#83`–`#91` 合入 `main`：模板/Plan、既定调度、restic runner、Job 协议、本地执行、Operation 持久化、Plan 健康、快照索引与列表。
- 快照写路径加固：终态结果与快照索引写入同一事务，索引失败整体回滚等待 Agent 重放。
- 快照浏览：`restic ls --json` 固定 argv 有界扫描；`SNAPSHOT_LIST` 复用 credential worker 的 lease 与凭据刷新；目录 API 支持 200 缓存 / 202 待扫描；快照页面支持目录导航。
- M6 控制：手动备份、取消（`JobCancel`）、重试（新 Operation 关联 parent/root）、终态优先补报、`JobResultAccepted` 后才标记上传；新旧 Agent/Server 通过 capability 保持兼容。

## 待完成

1. 开 PR 并让 GitHub Actions 验证本分支（本工作区不执行编译/测试）。
2. M8：大规模目录虚拟化、`restic snapshots --json` 周期索引、UNMANAGED 与 two-pass missing。
3. M6：Actions 下的离线补报、重复投递、取消竞态与真实 Restic 集成验收。
4. M4 生产运行验收仍未完成，不能因源码合入标记完成。

## 已知风险

- 所有新增行为的运行验收仍欠账；源码完成不等于验收通过。
- 取消请求在 Agent 离线时保持 `CANCEL_REQUESTED`，重连后由心跳补发；旧 Agent 不支持取消。
- 重试暂无幂等键，重复点击会创建多个新 Operation。
- 本环境无 `gh` 且禁止向 `main` 推送，PR 合并必须由维护者执行。

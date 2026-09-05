# 05 — 备份、快照与恢复

## 1. Restic 执行基线

RestFleet 通过外部 Restic binary 执行数据操作，不重写 Restic repository format。所有调用使用 argv API，禁止 shell 拼接。

V1 baseline：

- Repository format v2；
- Restic 0.19.x 契约；
- `--json` 解析长任务；
- `RESTIC_REPOSITORY_FILE`、`RESTIC_PASSWORD_FILE`、`RESTIC_CACERT` 或等价受保护文件；
- 显式 `--cache-dir`；
- `--cleanup-cache` 由维护策略运行；
- 生产禁止 `--insecure-tls`、空密码与无边界 `--no-lock`。

## 2. 有效 Plan

示例：

```yaml
id: 019...
revision: 7
paths:
  - /data
  - /etc
exclude_patterns:
  - "**/cache/**"
  - "**/.cache/**"
  - "**/tmp/**"
exclude_caches: true
one_file_system: false
schedule:
  cron: "0 3 * * *"
  timezone: Asia/Shanghai
  jitter_seconds: 300
  misfire_grace_seconds: 900
retry:
  max_attempts: 3
  initial_backoff_seconds: 60
  max_backoff_seconds: 1800
retention:
  keep_daily: 7
  keep_weekly: 4
  keep_monthly: 12
hooks:
  pre: [postgres-dump]
```

Agent 必须把有效配置写成受控临时文件：paths 使用 `--files-from-verbatim` 或逐 argv；exclude 使用 mode 0600 exclude file，避免参数长度和日志泄露。

## 3. Backup 命令语义

概念调用：

```text
restic backup
  --json
  --host <immutable-host-id-or-stable-name>
  --group-by tags
  --tag restfleet:managed
  --tag restfleet:host:<host-id>
  --tag restfleet:plan:<plan-id>
  --exclude-file <generated-file>
  --files-from-verbatim <generated-file>
  [--exclude-caches]
  [--one-file-system]
  [--skip-if-unchanged]
```

V1 统一 `--group-by tags`，并在 retention 中使用相同分组逻辑。用于分组的 RestFleet tags 必须稳定；Plan revision 不写入 tag，而由 Operation/Snapshot 索引关联。不要依赖可变化的 hostname 作为唯一分组键。

是否默认 `--skip-if-unchanged`：

- Template 默认开启；
- 若未生成新 snapshot，Operation 仍为 SUCCEEDED，summary 的 snapshot_id 为空；
- Backup Health 可由最近一次成功 Operation 维持，但 UI 必须显示“无变化，未创建快照”。

## 4. Pre/Post Hook

执行顺序：

```text
acquire plan lock
→ pre-hooks
→ restic backup
→ post-hooks
→ release lock
```

规则：

- Hook 只能是 Agent 本地 allowlist 的 absolute executable；
- pre-hook `ABORT` 失败时不运行 backup；
- post-hook 失败默认将成功 backup 降为 SUCCEEDED_WITH_WARNINGS；
- Hook 有独立 timeout、exit code、log section；
- Dump 产物必须位于 Plan 明确包含的路径；
- Agent 不自动猜测数据库一致性方案。

## 5. Backup 结果

至少保存：

```text
files_new / changed / unmodified
dirs_new / changed / unmodified
data_added / data_added_packed
total_files_processed / total_bytes_processed
backup_start / backup_end / total_duration
snapshot_id (optional when unchanged)
warning_count
exit_code
```

Restic JSON `summary` 是成功输出的必要证据。未知字段忽略；缺失关键字段按 engine output error 处理。

## 6. Retry

自动 retry 仅用于暂态错误：

- network unavailable/timeout；
- gateway 5xx/429；
- repository lock exit `11`；
- rclone provider throttling。

默认不自动高频重试：

- wrong password exit `12`；
- invalid config/path；
- permission denied reading source；
- certificate revoked/expired；
- repository missing exit `10`（除 provisioning race 外）。

每次 retry 创建新 Operation，关联 parent/root operation；遵守 Plan deadline，避免下一计划窗口重叠。

## 7. Snapshot 索引

### 7.1 Metadata

中心定时或 backup 完成后运行：

```text
restic snapshots --json
```

解析完整 snapshot ID、tags、paths、summary 和 program_version。Managed tags 用于映射 Host/Plan。无法映射的 snapshot 作为 `UNMANAGED` 展示，绝不能自动 retention。

### 7.2 文件树

快照浏览按需运行：

```text
restic ls --json <full-snapshot-id> [path]
```

JSONL 可包含 `snapshot` 与 `node` 消息。实现必须忽略未知字段，使用完整 path/parent path 构建目录树，并设置：

- 最大运行时间；
- 最大 node 数；
- 客户端取消；
- 缓存 key = repository + snapshot + tree ID + requested path；
- bounded cache；
- UI 流式/分页输出。

对超大目录，若 Restic CLI 无法提供真正浅层遍历，Server 可以流式扫描并只返回目标目录的直接子项，但必须设置 operation status 和资源限制，不假装是廉价 DB 查询。

## 8. 单文件下载

中心执行：

```text
restic dump <full-snapshot-id> <snapshot-path>
```

并将 stdout 流式写入 HTTP response。要求：

- 先通过 `ls`/cached metadata 确认目标为 regular file；
- 完整 snapshot ID 与精确 path；
- 不将文件写入中心磁盘，除非超过策略需要受控 spool；
- 设置 `Content-Type: application/octet-stream`；
- 安全编码 `Content-Disposition`；
- 用户断开时取消 Restic process；
- 记录 bytes streamed、duration、user 和结果；
- 并发与带宽限制；
- V1 不保证 HTTP Range/断点续传。

目录下载 MAY 使用 `restic dump --archive tar|zip`，但列为 V1.1；V1 只要求单文件。

## 9. Agent Restore

### 9.1 V1 目标

V1 支持把指定 Snapshot 的全部或部分路径恢复到 Agent 的 staging root，不支持原地覆盖。

```text
/var/lib/restfleet/restores/<restore-job-id>/
```

Docker Agent 对应受控挂载，例如 `/restores/<job-id>`。

### 9.2 流程

1. 用户选择 Snapshot、paths、target Host；
2. Server 校验 snapshot 属于 target Host 的 Repository；
3. 创建 preview，显示文件数/估算大小（可获得时）、目标路径和冲突策略；
4. 用户填写 reason 并确认；
5. Server dispatch RESTORE job；
6. Agent 验证 plan/repo binding 与 staging target；
7. Agent 运行 Restic restore；
8. 流式上报 progress/log；
9. 结束后回报 staging path、统计和 warnings；
10. UI 提示管理员自行验证/搬运文件。

### 9.3 命令语义

```text
restic restore
  --json
  --target <agent-controlled-staging-path>
  [--include <path> ...]
  <full-snapshot-id>
```

V1：

- overwrite policy 固定 NEVER；job target 是新建空目录；
- 禁止 `--delete`；
- 禁止 Server 下发任意 target absolute path；
- source path 作为独立 argv 或受控 include file；
- 检查 staging filesystem free space；
- 取消后保留 partial 目录并标记，提供本地手动清理说明；中心不远程递归删除。

## 10. “临时 Restore Token”的定位

Agent 为执行 backup 已持有本 Repository 的读能力和 Restic password，因此 V1 不宣称临时 token 能提供仓库内容的密码学隔离。

控制台仍需要一次性 Restore Job authorization，用于：

- 防止过期/重放的远程命令；
- 把 restore 限定到一个 Agent、Snapshot、path、target 和 deadline；
- 提供审计与 UI 确认；
- 让 Agent 拒绝不在 signed job payload 中的恢复。

它是控制平面授权，不是底层 Repository read credential。

## 11. Retention

Retention 在中心执行，按 Plan tag 限定：

```text
restic forget
  --dry-run
  --json
  --tag restfleet:managed,restfleet:plan:<plan-id>
  --group-by tags
  --keep-last ...
  --keep-daily ...
  --keep-weekly ...
  --keep-monthly ...
  --keep-yearly ...
```

安全流程：

1. 完整成功索引当前 snapshots；
2. 运行 dry-run；
3. 验证只包含目标 Plan managed snapshots；
4. 验证保留数 >= `minimum_snapshots`；
5. 保存 preview snapshot set hash；
6. 自动 schedule 可按已批准 policy 执行；手动危险执行需确认；
7. 执行前再次比较 snapshot set hash；
8. 执行 forget，不与 prune 合并；
9. 重新索引；
10. 按 Repository schedule 另行 prune。

V1 禁止 `--unsafe-allow-remove-all` 与空 group-by。Unmanaged snapshots 永不自动删除。

## 12. Prune

- Repository scoped；
- 只在 successful forget 后的计划窗口或独立 schedule 运行；
- 不与 backup/check/restore/download 并发；
- 设置 max duration 与 cancel policy；
- rclone/OneDrive 可能存在 throttling，默认低并发；
- prune JSON 支持不完整时，保存结构化 exit/result + redacted raw logs；
- 中断后 Repository 标记 `MAINTENANCE_REQUIRED`，下一步先 check，不盲目重复 prune。

## 13. Check

分两级：

- metadata check：频率较高，`restic check --json`；
- data subset/full read：频率较低，按 policy 执行。

UI 必须区分“结构一致性通过”与“读取了多少数据”。不能把 metadata check 显示为已验证全部数据。

## 14. Unlock

- 默认只允许清理 stale lock；
- preview 显示 lock age 与当前 active operations；
- 存在活动 lease 时拒绝；
- 手动 unlock 需要 reason 与确认；
- 执行后建议/自动运行 metadata check；
- 不提供“一键强制删除所有锁而不检查”的 V1 操作。

## 15. Repository 初始化

Server 创建 Repository 时：

1. 生成 repo UUID/path、Restic password、gateway identity；
2. provision path/auth；
3. 中心通过 Maintenance access 运行 `restic init --json`；
4. 验证 repository ID/format；
5. 运行 snapshots/index smoke test；
6. 下发 Agent credential revision；
7. Agent ACK 后 Repository/Plan 才进入 READY/ACTIVE。

任一步失败都保留可恢复 provisioning state，不能把半初始化 Repo 标为 READY。

中心初始化适配器 MUST 先执行 `cat config`：只有明确的缺失退出码 10 且任务没有已记录 Restic ID 时才执行 `init --json --repository-version 2`；认证失败、锁冲突、网络错误或未知退出码 MUST NOT 触发 init。初始化输出的完整 ID MUST 与再次读取的 config 匹配；format MUST 为 2，snapshots MUST 是空数组。本阶段已有快照的目标 MUST 拒绝继续 provisioning，不自动接管已有仓库。

重试 MUST 复用已持久化的密码、UUID scope 和已知 ID；MUST NOT 删除远端对象或重新生成密码来绕过失败。适配器成功只表示中心初始化验证通过，不代表 Agent credential ACK 或 READY。

## 16. 容量语义

Dashboard 分开显示：

- `restore_size_bytes`：恢复选定 snapshots 的逻辑大小；
- `raw_data_bytes`：Restic repository 内 unique blobs 的统计估算；
- `provider_used_bytes`：OneDrive/provider 报告的整体或 remote 占用，可能不等于 Repo；
- `provider_free_bytes`：若 API 可用；
- `data_added_packed`：单次 backup 新增压缩后数据。

所有值必须带 source 和 collected_at，避免误导。

## 17. 官方能力依据

- [Restic JSON/JSONL 与 exit codes](https://restic.readthedocs.io/en/stable/075_scripting.html)
- [Restic Snapshot/Repository 操作](https://restic.readthedocs.io/en/stable/045_working_with_repos.html)
- [Restic dump/restore](https://restic.readthedocs.io/en/stable/050_restore.html)
- [Restic retention 与 group-by/tag](https://restic.readthedocs.io/en/stable/060_forget.html)
- [rclone serve restic](https://rclone.org/commands/rclone_serve_restic/)

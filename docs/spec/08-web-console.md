# 08 — Web Console

## 1. 设计目标

Web Console 服务于日常运维判断，不只是 CRUD：

- 5 秒内回答“哪些 Host/Plan 需要处理”；
- 明确区分 Agent Online 与 Backup Healthy；
- 所有状态可追溯到原始 Operation、采集时间和配置 revision；
- 危险操作先展示影响范围、再确认并审计；
- Secret 永不回显；
- 超大日志/快照目录采用流式或分页，不锁死浏览器。

## 2. 技术基线

- React + TypeScript + Vite；
- 路由按资源组织；
- Server 提供静态产物，同源访问 API；
- 使用生成自 OpenAPI 的 request/response types；
- 数据请求缓存必须以 resource revision/filters 作为 key；
- SSE 用于 Operation 实时状态，可退化为 polling；
- WCAG 2.2 AA 作为可访问性目标；
- Desktop-first，但核心状态/确认流程在窄屏可用。

## 3. 信息架构

主导航：

```text
Overview
Hosts
Repositories
Templates
Plans
Snapshots
Operations
Notifications
Audit
Settings
```

Maintenance 不做独立孤岛页面，而在 Repository detail 下提供；全局 Operations 可统一查看。

## 4. Overview

### 4.1 顶部指标

- Hosts：total / online / offline / degraded；
- Backup：healthy / failed / overdue / never run / paused；
- Storage：repositories / snapshots / restore size / provider quota；
- Maintenance：last check / last prune / due/failed；
- Credentials：healthy / degraded / expired。

每张卡显示数据采集时间。`unknown` 不能渲染为 0。

### 4.2 Attention Queue

按 severity 展示：

1. Repository corruption/check failure；
2. credential expired/refresh failure；
3. failed/overdue backup；
4. Agent offline/revoked connection attempt；
5. maintenance overdue；
6. Plan pending/rejected config。

每条可直接进入对应 detail/operation，不允许只有红色数字无上下文。

### 4.3 Recent Operations

显示 type、Host/Repo/Plan、status、started/duration、initiator。`SUCCEEDED_WITH_WARNINGS` 使用独立黄色状态，不能归入成功绿色。

## 5. Hosts

### 5.1 List

列：

- Name；
- Agent Health；
- Backup Health；
- active Plan；
- last successful backup；
- next run；
- Agent/restic version；
- labels。

支持按 health、label、version、text filter。默认把需处理对象置前，但用户选择的 sort 可持久化到本地偏好。

### 5.2 Add Host wizard

步骤：

1. Host name/timezone/labels；
2. Enrollment token TTL；
3. 选择 Native 或 Docker；
4. 仅一次显示 token/install command；
5. 等待 Agent 连接并显示 fingerprint/hostname/arch；
6. 创建或绑定 Repository；
7. 选择 Template 创建 Plan；
8. 等待 config accepted，可选 Backup Now。

离开 token 页面后不再显示 token；只提供 revoke + generate new。

### 5.3 Host detail

Tabs：Summary、Plans、Operations、Inventory、Agent、Audit。

Summary 必须同时显示：

- Control connection；
- last heartbeat；
- accepted vs desired revision；
- last/next backup；
- Repository status；
- warnings。

## 6. Repositories

### 6.1 List

列：status、Host、snapshot count、restore size、raw data、last index/check/prune、credential health。

### 6.2 Detail

Sections：

- Overview / backend metadata；
- Snapshot summary；
- Maintenance schedule/history；
- Gateway credential revision/last rotation；
- Storage credential status/last refresh；
- Operations；
- Danger zone（disable，不直接 delete data）。

Actions：Test、Refresh Index、Check、Retention Preview、Prune、Unlock Preview、Rotate Gateway Credential。

UI 必须解释 `restore size`、`raw data`、`provider used` 三者差异。

## 7. Templates

- 展示 current revision、paths count、exclude count、schedule、retention、dependent Plans；
- 编辑前显示影响摘要；
- 保存创建新 revision；
- 不自动应用到 Plans；
- 保存后显示哪些 Plans 有 update available；
- Batch Apply 列出每个 Plan 的 current/target revision 和冲突结果。

Path/exclude 编辑器提供逐项输入和 YAML preview，但 Server 仍是最终 validator。

## 8. Plans

### 8.1 List

列：status、Host、Template revision、schedule/timezone、last result、next run、accepted revision。

### 8.2 Detail

- Effective config 与 Template/override 差异；
- desired/accepted revision；
- schedule timeline；
- retention policy；
- local hook capability；
- latest Operations/Snapshots；
- Backup Now / Pause / Resume / Retry。

Backup Now 是有副作用操作：按钮打开简短确认，展示 Host、Repo、paths、是否已有 active backup。重复点击必须复用 Idempotency-Key 或禁用按钮直到响应。

## 9. Snapshot Browser

### 9.1 Snapshot List

Filter：Repository、Host、Plan、time、managed/unmanaged、tags。显示完整 ID 的短展示与复制完整 ID 动作。

### 9.2 文件树

布局：左侧 breadcrumbs/tree，右侧条目表；条目列 name/type/size/mtime/permissions。

加载语义：

- 缓存命中显示 collected time；
- 实时扫描显示 Operation progress/cancel；
- 大目录逐批渲染；
- error 提供 request/operation ID；
- symlink 明确标识，不按链接自动导航到任意目标。

### 9.3 Download

仅 regular file 显示 Download。点击后先请求短期 download intent，再导航到 opaque URL。开始后 toast/Operation 可追踪；不得把原始 snapshot path 放在可共享 URL query 中。

### 9.4 Restore

流程：选择 paths → target Host → staging preview → 确认。

确认页必须显示：

- Snapshot time/full ID；
- source paths；
- target Host；
- staging root；
- overwrite = NEVER；
- estimated size/files（若未知则写未知）；
- Agent online/capability；
- reason 输入；
- “这不会原地覆盖服务文件”的说明。

## 10. Operations

### 10.1 List

Filter type/status/source/Host/Repo/Plan/time。URL 保留 filters，便于分享内部链接。

### 10.2 Detail

- 状态时间线；
- config revision/hash；
- structured summary；
- progress；
- stdout/stderr/system logs；
- Restic exit code 与 RestFleet error code；
- related snapshot；
- parent/retry chain；
- initiator；
- Cancel/Retry 条件。

日志默认自动跟随仅在用户位于底部时启用；用户向上滚动后不抢焦点。Secret redaction marker 明确显示 `[REDACTED]`。

## 11. Maintenance 危险操作

### 11.1 Retention/Forget

必须先显示 dry-run preview：将删除/保留的 Snapshot 数和列表、每个 Plan、policy、preview expiry/hash。

手动执行要求：

- 勾选理解影响；
- 输入 reason；
- 若超过安全阈值（例如删除 >20% 或最后 N 个）输入 Repository name 二次确认；
- snapshot set 改变时 UI 显示 stale，不自动重试执行。

### 11.2 Prune

显示可能耗时、云端 API/throttling、最近 backup 状态和预估范围。若有 active backup，按钮禁用并解释原因。

### 11.3 Unlock

仅显示 stale lock preview。Active lock 不能被 UI 强制选择。执行后引导运行 Check。

## 12. Credentials

页面可显示：

- Provider/remote/account 的非 secret metadata；
- status；
- last test/refresh；
- expiry（若 provider 提供）；
- Test、Replace/Reauthenticate、Disable。

永不显示：token、client secret、crypt password、完整 rclone config、Restic password、gateway password、ciphertext。

Replace 操作成功后不能通过浏览器 history/cache 再看到 secret。表单使用 `autocomplete=off/new-password` 的合理设置并在提交后清空内存状态。

## 13. Notifications

- Channel list 显示 type/status/last test；
- 创建/替换 token 只可输入，不可回显；
- Test 发送明确标注的测试事件；
- Deliveries 显示 attempt、状态、HTTP code 和截断后的非敏感错误；
- 告警规则使用事件类型与 severity，不在 V1 做任意表达式语言。

## 14. Audit

可按 actor/action/resource/result/time 搜索。Change diff 对 secret 字段只显示 `changed: true`/revision，不显示原值、密文或 hash。下载、恢复、credential view attempt 都进入审计。

## 15. Settings

- instance name/base URL/time display；
- backup SLA grace defaults；
- session policy；
- CA/trust bundle metadata；
- version/build info；
- diagnostics export（redacted）；
- feature flags 只读或受控。

## 16. 状态文案

避免模糊状态：

| 内部状态 | UI 文案示例 |
|---|---|
| ONLINE + OVERDUE | Agent 在线，但备份已逾期 |
| OFFLINE + HEALTHY | Agent 离线；最近备份仍在 SLA 内 |
| SUCCEEDED_WITH_WARNINGS | 已创建快照，但有文件未读取 |
| PENDING_APPLY | 配置已保存，Agent 尚未确认 |
| ConfigRejected | Agent 拒绝配置：显示字段与错误码 |
| credential DEGRADED | 当前仍可访问，但刷新/测试失败 |
| UNMANAGED snapshot | 非 RestFleet 管理，不会自动 Retention |

颜色不是唯一信息载体，必须配图标和文本。

## 17. 错误与恢复

- 所有错误展示 request ID；长任务同时展示 operation ID；
- 401 跳登录但保留安全 return path；
- 412 revision conflict 显示当前值与用户改动，不自动覆盖；
- 409 stale preview 重新获取 preview，不自动执行；
- 网络断开时状态修改不盲目重发，使用 idempotency key 查询结果；
- SSE 断开退化为 polling；
- 页面刷新不丢失 Operation 跟踪。

## 18. 可访问性与本地化

- 键盘完成主要流程与 dialog；
- focus trap/restore 正确；
- status/进度使用 ARIA live 的低频更新，避免噪声；
- table 在窄屏提供语义化卡片或横向滚动；
- 时间同时显示相对值与绝对 timezone tooltip；
- V1 界面中文，代码/协议 identifier 保留英文；文案集中管理，为未来 i18n 留接口。

## 19. 前端验收

- Dashboard 不混淆 Online/Healthy；
- Secret 从不进入 React Query/devtools 可缓存 response；
- 所有危险动作均经过规定 preview/confirm；
- 10 MiB 日志和 10k 行目录不会导致页面无响应；
- loading/empty/error/stale/offline 状态均有明确 UI；
- E2E 覆盖 enrollment wizard、Plan apply、Backup Now、snapshot browse/download、restore、retention preview。

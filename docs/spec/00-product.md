# 00 — 产品规范

## 1. 产品定义

RestFleet 的目标是：让管理少量到数十台 Linux VPS 的管理员，通过一个自托管中心，持续获得受隔离保护、状态可核对、能够恢复的备份。

管理员在 Web Console 中配置备份、识别风险并取回数据；Agent 在各 Host 本地调度和执行 Restic；中心管理云存储凭据、仓库生命周期、查询、维护、审计与告警。Restic 提供备份与仓库能力，rclone + Crypt 对接 V1 支持的云端后端。

一句话原则：

> Agent 负责读取本机数据并产生备份；中心负责身份、策略、云存储凭据、仓库生命周期、可视化与审计。

所有目标共同受以下安全前提约束：

- Agent MUST 只持有自身 Host 所需的身份与仓库凭据，云存储、rclone/Crypt 和中心维护凭据 MUST 留在中心信任区域；
- 被攻陷的 Host MUST 不能通过 Agent 权限删除或覆盖历史仓库对象，MUST 不能访问其他 Host 的仓库；正常备份临时锁清理的唯一例外见安全模型；
- Agent 对自身仓库具有读取与新建权限；Host root 能读取该 Host 已持有密码对应的历史数据。产品 MUST 准确表达这一保护边界。

具体信任与授权要求见 [安全模型](02-security-model.md)，V1 范围与发布门禁见 [范围规范](11-v1-scope.md)。

## 2. 目标用户

V1 面向管理少量到数十台 VPS 的单个管理员或小型运维团队：

- 多数服务由 Docker Compose 部署；
- 主机分散在不同云厂商、NAT 或防火墙之后；
- 希望统一配置备份而不在每台 VPS 存放云后端/rclone 凭据；
- 需要快速回答“哪台机器没有按时备份”；
- 需要集中浏览、下载与恢复历史快照；
- 需要将勒索或单机入侵后的备份删除风险降至最低。

## 3. 成功标准

### 3.1 用户结果

| 目标 | 管理员能够确认的结果 | 主要交付 |
|---|---|---|
| 持续产生备份 | 哪些路径按哪个已接受的计划备份，最近一次是否成功，下一次何时执行 | 一次配对与身份续期、配置确认、本地调度、备份执行、重试/取消、离线结果补报 |
| 及时识别风险 | 哪台 Host 离线、哪个 Plan 失败或逾期、是否有部分读取失败、下一步处理什么 | Agent/Backup 健康分离、Operation 日志与统计、Attention Queue、Gotify/Webhook |
| 取回所需数据 | 找到指定时间的快照与文件，下载单个文件或恢复到受控 staging，并获得可核对的结果 | 快照索引/浏览、单文件下载、Agent staging restore、进度/取消与审计 |
| 长期安全运行 | 保留策略、仓库检查与维护可控，凭据可续期，中心故障后有恢复路径 | 中心 check/forget/prune/unlock、危险操作预览、凭据管理、控制面灾备与部署文档 |

管理员 MUST 能从界面状态追溯到接受的配置、实际 Operation 与快照/恢复结果。Agent 在线、仓库 READY、任务已派发均不能单独作为备份成功证据；部分成功 MUST 显示警告。

### 3.2 完整使用闭环

V1 MUST 完成以下端到端流程：

1. 管理员创建一次性 Enrollment Token；
2. 新 Agent 在 amd64/arm64 Linux 上完成配对并获得本机证书；
3. 管理员创建 Repository、Template 与 Plan，Agent 原子接受有效配置并回报 revision；
4. 控制 API/DB 暂时不可用时，Agent 仍本地调度已接受计划；Gateway 持续在线且预授权有效时，在剩余授权窗口内继续备份。Gateway 不可达或授权不足时暂停并报错，保留身份与配置，正常故障不要求重新配对；
5. Dashboard 正确显示 Agent Health 与 Backup Health；
6. 管理员能查看 Operation 日志与 Restic 统计；
7. 管理员能浏览快照并安全下载单个文件；
8. 管理员能预览、确认并触发恢复到 Agent 的 staging 目录，核对恢复内容与结果；
9. Retention、check、forget、prune、unlock 只在中心执行；
10. Gotify/Webhook 能对关键失败去重告警；
11. 所有敏感动作存在可追溯审计记录。

离线授权默认从最后一次中心签发起 72h，可配置且受原占用截止限制；单次备份会话最多 24h。离线窗口不承诺控制中心与 Gateway 同时不可达时仍能写入，也不允许离线延长授权。完整要求见安全模型 §8.3。

### 3.3 交付优先级

安全隔离、秘密保护、权限与审计 MUST 贯穿所有交付。功能按以下用户结果收敛：

1. **备份可靠**：接入 → 配置接受 → 定时/手动备份 → 结果/日志 → 健康与快照查询，覆盖断连、重启、重复投递、部分失败和取消。
2. **恢复可用**：在可信备份基础上完成单文件下载与 staging restore，以实际取回的数据验证备份价值。
3. **运维完整**：补齐保留/维护、通知、控制面灾备、部署升级与发布验收，形成可长期运行的 V1。

[实施计划](../../IMPLEMENTATION_PLAN.md)按上述三个用户结果组织当前 Milestone，每次只推进一个 ACTIVE 阶段；旧 M0–M12 保留为范围与验收追溯，不再分别串行实施。阶段重排不删除 V1 能力或发布门禁，原 Acceptance IDs 和要求 MUST 保留。

## 4. 核心角色

V1 是单租户产品，但仍定义两个逻辑角色：

| 角色 | 权限 |
|---|---|
| Administrator | 全部配置、下载、恢复、维护、凭据与审计权限 |
| Viewer | 只读 Dashboard、Hosts、Plans、Snapshots、Operations；V1.1 实现 |

V1 可以只实现 Administrator，但授权中间件必须以显式权限检查组织，不能把“已登录”直接等同于所有未来权限。

## 5. 核心用户旅程

### 5.1 初次设置

1. 启动中心 Docker Compose；
2. 使用一次性 bootstrap secret 创建首个管理员；
3. 导入 OneDrive、Google Drive 或 HTTPS WebDAV 的 rclone credential；
4. Test Connection；
5. 创建默认 Repository policy 与通知渠道。

### 5.2 接入 Host

1. 在 Console 选择 **Add Host**；
2. 输入显示名称、标签与 enrollment token TTL；
3. Console 显示一次性 token 和 Native/Docker 安装命令；
4. Agent 本地生成密钥并提交 CSR；
5. 中心签发证书，Host 状态变为 Online / Unconfigured；
6. 管理员绑定 Repository 与 Plan；
7. Agent 接收配置并回报 Accepted revision。

### 5.3 日常运维

- 从 Dashboard 识别 Failed、Overdue、Offline；
- 查看具体 Operation 的结构化摘要和原始日志；
- 运行 Backup Now 或 Retry；
- 浏览快照，下载文件或发起 staging restore；
- 查看 Repository 容量、check/prune 状态与 credential health；
- 接收 Gotify/Webhook 告警。

## 6. 产品对象

| 对象 | 产品含义 |
|---|---|
| Host | 被备份的 VPS 身份与健康状态 |
| Agent | Host 上与中心通信、调度并执行 Restic 的软件实例 |
| StorageCredential | 中心持有的 rclone/provider/Crypt secret 集合 |
| Repository | 一个 Restic 仓库及其网关、密码、维护策略和容量状态 |
| BackupTemplate | 可复用的路径、排除、周期、Retention 与 Hook 模板 |
| Plan | Template + Host + Repository 的实际绑定和 override |
| Snapshot | Restic 快照元数据 |
| Operation | Backup、Restore、Check、Forget、Prune、Unlock 等一次执行 |
| RestoreJob | 一次明确目标和安全策略的恢复动作 |
| NotificationChannel | Gotify 或 Webhook 配置 |
| AuditEvent | 谁在何时对什么对象做了什么 |

## 7. 健康状态定义

Agent Health 与 Backup Health 必须分开显示：

### Agent Health

- `ONLINE`：最近两个 heartbeat interval 内收到心跳；
- `DEGRADED`：已连接但报告磁盘、Restic、时钟或配置错误；
- `OFFLINE`：超过 offline threshold 未收到心跳；
- `REVOKED`：管理员撤销，所有控制连接被拒绝；
- `PENDING`：Enrollment 尚未完成。

### Backup Health

- `HEALTHY`：最近一次应执行窗口内存在成功或允许的 partial snapshot；
- `FAILED`：最近一次应执行的 Operation 失败；
- `OVERDUE`：超过 `next_run + grace_period` 未出现成功结果；
- `NEVER_RUN`：Plan 已启用但从未完成备份；
- `PAUSED`：Plan 被暂停；
- `UNKNOWN`：计划或时间数据不足，不能推断。

Restic exit code `3` 表示存在无法读取的源数据但可能已创建不完整快照，必须映射为 `SUCCEEDED_WITH_WARNINGS`，不能显示为完全成功。

## 8. Dashboard 指标

必须显示：

- Hosts 总数、Online、Offline、Degraded；
- Plans 的 Healthy、Failed、Overdue、Never Run、Paused；
- Repositories 数量、逻辑恢复大小、后端占用（若 provider 可提供）、Snapshot 数；
- 最近一次 check、prune 与 credential test；
- 最近失败 Operations；
- 需要处理的安全事件，如 revoked Agent 重连或 token refresh failure。

指标必须标注采集时间。Provider quota 与 Restic `stats --mode raw-data` 是不同概念，UI 不得混为“真实云盘占用”。

## 9. 非目标

V1 明确不做：

- Windows/macOS Agent；
- Kubernetes Operator；
- 多租户 SaaS；
- 高可用 Server；
- 任意远程 Shell；
- 中心向 Host 主动 SSH；
- Agent 自动升级与任意 Restic 版本切换；
- Shared Repository；
- 原地覆盖式恢复；
- Bare-metal 系统镜像；
- 数据库应用一致性的自动发现；
- 将备份数据通过 Control API 中转；
- 以 rclone mount 作为默认仓库通路。

## 10. 产品原则

- **安全边界真实可解释**：UI 不使用“write-only”描述标准 Restic append-only。
- **离线可执行**：控制面不可用时保留本地调度；Gateway 持续在线且预授权有效时继续备份，期限与失败行为遵循安全模型。
- **默认安全**：一 Host 一 Repo、staging restore、危险动作预览。
- **操作可审计**：配置变更和数据读取同样需要审计。
- **状态可证明**：健康状态必须能追溯到计划窗口与 Operation，而不是仅看 Agent 在线。
- **渐进复杂度**：V1 用模块化单体和 PostgreSQL，不引入消息中间件。

## 11. V1 体验性能目标

- 50 Hosts 下，Dashboard 缓存查询 P95 小于 500 ms；
- 普通列表 API P95 小于 300 ms（不含实时 Restic/rclone 子进程）；
- Agent heartbeat 到 UI 状态更新小于 15 s；
- 在线 Agent 的命令 dispatch P95 小于 5 s；
- 10,000 个 Snapshot 的分页列表不一次性返回全部数据；
- 单个日志流不得无限增长，必须分块、限额和归档；
- 中心重启后未完成 Operation 可恢复为明确状态，不得永久卡在 RUNNING。

## 12. 完成定义

产品功能只有同时满足以下条件才算完成：

- API、UI 与 Agent 行为符合规范；
- 正向与负向自动化测试通过；
- 权限、审计、Secret redaction 已验证；
- 中心故障与 Agent 重连场景已验证；
- 文档、OpenAPI/protobuf、数据库迁移同步更新；
- `12-acceptance-tests.md` 对应条目通过。

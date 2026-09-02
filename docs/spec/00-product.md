# 00 — 产品规范

## 1. 产品定义

RestFleet 是一个管理多台 Linux VPS 备份的自托管控制平面。管理员在中心 Web Console 中管理 Agent、Repository、Backup Template 与 Plan；Agent 在各 Host 本地可靠执行 Restic；中心负责仓库后端凭据、快照查询、维护、审计与告警。

一句话原则：

> Agent 负责读取本机数据并产生备份；中心负责身份、策略、云存储凭据、仓库生命周期、可视化与审计。

## 2. 目标用户

V1 面向管理少量到数十台 VPS 的单个管理员或小型运维团队：

- 多数服务由 Docker Compose 部署；
- 主机分散在不同云厂商、NAT 或防火墙之后；
- 希望统一配置备份而不在每台 VPS 存放 OneDrive/rclone 凭据；
- 需要快速回答“哪台机器没有按时备份”；
- 需要集中浏览、下载与恢复历史快照；
- 需要将勒索或单机入侵后的备份删除风险降至最低。

## 3. 成功标准

V1 成功不是“有一个 Restic Web UI”，而是以下端到端闭环均可用：

1. 管理员创建一次性 Enrollment Token；
2. 新 Agent 在 amd64/arm64 Linux 上完成配对并获得本机证书；
3. 管理员创建 Repository、Template 与 Plan；
4. Plan 下发后，即使中心暂时离线，Agent 仍按计划执行；
5. Dashboard 正确显示 Agent Health 与 Backup Health；
6. 管理员能查看 Operation 日志与 Restic 统计；
7. 管理员能浏览快照并安全下载单个文件；
8. 管理员能触发恢复到 Agent 的 staging 目录；
9. Retention、check、forget、prune、unlock 只在中心执行；
10. Gotify/Webhook 能对关键失败去重告警；
11. 所有敏感动作存在可追溯审计记录。

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
3. 导入或创建 OneDrive rclone credential；
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
| StorageCredential | 中心持有的 rclone/OneDrive/Crypt secret 集合 |
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
- **离线可执行**：中心不可用不应导致已配置备份停止。
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

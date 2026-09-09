# 07 — PostgreSQL 数据库规范

## 1. 选择 PostgreSQL

V1 使用 PostgreSQL，而非生产 SQLite，原因：

- Agent 连接、Operation、日志、通知与维护任务存在并发写入；
- 需要 row lock、`SKIP LOCKED`、advisory lock、事务 outbox 和可靠租约；
- 需要 JSONB、partial index、约束和后续扩展能力；
- 中心 Docker Compose 增加一个数据库服务的成本可控；
- 避免 V1 后期从 SQLite 迁移关键状态机。

Agent 本地状态仍使用 bbolt，与控制面数据库选择无关。

## 2. 通用约定

- PostgreSQL 16+；
- 所有时间 `timestamptz`，应用写 UTC；
- 公共 ID `uuid`，应用生成 UUIDv7；
- enum 优先 `text + CHECK` 或 lookup constraints，便于滚动迁移；
- 金额/字节/计数使用 `bigint` 并检查非负；
- secret plaintext 禁止入库；
- migration 只前向自动执行；破坏性 rollback 使用修复迁移；
- 每次 migration 在 CI 从空库和上一 release snapshot 测试；
- SQL query 使用参数绑定。

## 3. 核心关系

```mermaid
erDiagram
    HOSTS ||--o{ AGENTS : installs
    HOSTS ||--o{ PLANS : runs
    HOSTS ||--|| REPOSITORIES : owns
    TEMPLATES ||--o{ PLANS : instantiates
    REPOSITORIES ||--o{ SNAPSHOTS : contains
    PLANS ||--o{ SNAPSHOTS : tags
    PLANS ||--o{ OPERATIONS : executes
    REPOSITORIES ||--o{ OPERATIONS : maintains
```

## 4. 表清单

### 4.1 Identity/Auth

- `users`
- `sessions`
- `bootstrap_state`
- `enrollment_tokens`
- `agent_certificates`

### 4.2 Inventory/Config

- `hosts`
- `agents`
- `agent_inventories`
- `storage_credentials`
- `secrets`
- `repositories`
- `repository_credential_revisions`
- `retention_policies`
- `maintenance_policies`
- `templates`
- `template_revisions`
- `plans`
- `plan_revisions`

### 4.3 Execution/Data index

- `jobs`
- `operations`
- `operation_events`
- `operation_log_chunks`
- `snapshots`
- `snapshot_entry_cache`
- `restore_previews`
- `restore_jobs`
- `maintenance_previews`
- `repository_leases`

### 4.4 Delivery/Audit

- `outbox_events`
- `notification_channels`
- `notification_deliveries`
- `audit_events`
- `idempotency_records`

## 5. 关键表定义（逻辑）

### 5.1 hosts

```sql
create table hosts (
  id uuid primary key,
  display_name text not null,
  description text not null default '',
  labels jsonb not null default '{}',
  timezone text not null,
  status text not null,
  revision bigint not null default 1,
  created_at timestamptz not null,
  updated_at timestamptz not null,
  archived_at timestamptz,
  check (status in ('PENDING','ACTIVE','DISABLED','REVOKED')),
  check (revision > 0)
);
```

`display_name` 在 active rows 中大小写不敏感唯一；可使用 functional partial unique index。

### 5.2 agents

```sql
create table agents (
  id uuid primary key,
  host_id uuid not null references hosts(id),
  install_id uuid not null unique,
  public_key_fingerprint text not null unique,
  certificate_serial text not null unique,
  certificate_not_after timestamptz not null,
  status text not null,
  version text not null,
  protocol_version text not null,
  os text not null,
  arch text not null,
  hostname text not null,
  boot_id text,
  restic_version text,
  uptime_seconds bigint not null default 0,
  state_free_bytes bigint not null default 0,
  clock_offset_ms bigint not null default 0,
  last_seen_at timestamptz,
  last_connected_at timestamptz,
  desired_revision bigint not null default 0,
  accepted_revision bigint not null default 0,
  heartbeat_error_code text not null default '',
  config_error_code text not null default '',
  config_error_field text not null default '',
  created_at timestamptz not null,
  updated_at timestamptz not null,
  check (status in ('ACTIVE','REVOKED')),
  check (accepted_revision <= desired_revision)
);
```

Partial unique index：每个 Host 最多一个 `status = 'ACTIVE'` Agent。运行 health 不持久化，并按 ADR-0006 在读取时推导。

`agent_desired_states` MUST 以 `(agent_id, revision)` 为主键保存完整规范化 `config_json` 与 `config_hash`。创建或改变 desired revision 的事务 MUST 同时写 `outbox_events`。

`agent_inventories` MUST 保存不可变快照，并在 `(agent_id, captured_at desc)` 上建立索引。清单字段和 JSON map/array MUST 有长度、类型和枚举边界；不得包含环境变量、用户列表、进程列表或文件名。

### 5.3 secrets

```sql
create table secrets (
  id uuid primary key,
  secret_type text not null,
  ciphertext bytea not null,
  nonce bytea not null,
  wrapped_dek bytea not null,
  wrap_nonce bytea not null,
  kek_version integer not null,
  aad_version integer not null,
  created_at timestamptz not null,
  rotated_at timestamptz,
  destroyed_at timestamptz,
  check (octet_length(nonce) = 12),
  check (octet_length(wrap_nonce) = 12)
);
```

普通 repository/API query 不 join/decrypt secrets。Secret access 通过独立 store interface，并生成 audit/security metric。

### 5.3.1 storage_credentials / storage_credential_revisions（schema 5）

M4 第一批 MUST 复用 migration 00003 的实际 secrets 字段（kind、algorithm、key_id、ciphertext、nonce、wrapped_data_key、wrap_nonce、aad、created_at），不得以逻辑 schema 示例另建不兼容的加密格式。

storage_credentials 保存 id、name、provider、remote_name、status、secret_ref、secret_revision、revision、created_at、updated_at。名称大小写不敏感唯一，schema 5 的 provider 仅 RCLONE_ONEDRIVE（schema 8 扩展见下），状态使用领域模型枚举；尚未连接测试的记录为 UNTESTED。

storage_credential_revisions 使用 (credential_id, revision) 主键，将版本映射到不可变的 secrets 记录。create/replace 的两表更新、密文 insert 与 AuditEvent MUST 同事务；运行时角色只能对密文历史 SELECT/INSERT。替换使用行锁与 revision CAS，失败事务不得残留孤立的秘密版本。

测试/刷新状态字段、runtime dispatch 和 repository 外键在后续 M4 migration 增补。第一批不得伪造 last_tested_at 或 HEALTHY。

### 5.3.2 多后端 provider（schema 8）

迁移 00008 MUST 仅扩展 provider CHECK，加入 RCLONE_GDRIVE 与 RCLONE_WEBDAV；不得重写旧 provider、密文、AAD、secret revision 或 Repository 外键。密文载荷继续保存 canonical rclone config，不新增明文端点/认证列。Down 在已有新后端记录时 MUST 失败并整体回滚，不得删除数据或将其改标 OneDrive；生产使用前向修复迁移。

### 5.4 repositories

```sql
create table repositories (
  id uuid primary key,
  host_id uuid not null references hosts(id),
  storage_credential_id uuid not null references storage_credentials(id),
  name text not null,
  backend_path text not null unique,
  gateway_username text not null unique,
  gateway_secret_ref uuid not null references secrets(id),
  restic_secret_ref uuid not null references secrets(id),
  format_version integer,
  status text not null,
  maintenance_policy_id uuid references maintenance_policies(id),
  snapshot_count bigint not null default 0,
  restore_size_bytes bigint,
  raw_data_bytes bigint,
  provider_used_bytes bigint,
  last_indexed_at timestamptz,
  last_check_at timestamptz,
  last_prune_at timestamptz,
  revision bigint not null default 1,
  created_at timestamptz not null,
  updated_at timestamptz not null,
  archived_at timestamptz,
  check (status in ('PROVISIONING','READY','DEGRADED','LOCKED','DISABLED','ERROR')),
  check (snapshot_count >= 0)
);
```

Partial unique index：每 Host 仅一个未 archive Repository。V2 共享 Repo 需要显式迁移，不提前削弱 V1 约束。

### 5.4.1 M4 实际仓库记录（schema 7）

迁移 00007 创建 repositories 与 repository_credential_revisions，复用实际 secrets 信封字段。gateway_username 使用生成的 UUIDv7（SQL uuid），backend_path MUST 精确为 restfleet/agents/{gateway_username}/{repository_id}；身份和相对路径不接受客户端输入。本批不创建尚未使用的维护策略、统计或 Agent ACK 字段。

Host 行锁后依次锁定 StorageCredential、写入两份独立随机密码的密文、Repository、两条初始 revision 与 REPOSITORY_CREATE 审计，MUST 在同一事务提交。任意失败 MUST 全部回滚。唯一索引 repositories_host_idx 以 archived_at is null 为条件，不排除 DISABLED/ERROR，防止错误状态绕过 per-Host 隔离。两个 secret_ref MUST 不同，历史使用 unique(repository_id,kind,revision)，每个历史 secret_ref 唯一。

新仓库 MUST 为 PROVISIONING，format_version MUST 为 NULL，凭据与记录 revision 为 1。信封 AAD MUST 绑定 Host、Repository、kind、revision、secret ID 和 master key ID。运行时角色仅获两张新表的 SELECT/INSERT；后续初始化或轮换所需更新权限 MUST 在对应迁移中显式增加。记录创建不需要 outbox；后续 initialize MUST 使用持久化 Operation/jobs，不能复用 HTTP 请求寿命执行。

### 5.4.2 M4 初始化（schema 9）

迁移 00009 MUST 添加 repositories 的原生 restic_id（不返回 API）、initialized_at 和 last_initialize_operation_id；扩展 operations/jobs 的 REPOSITORY_INITIALIZE 类型和 repository_id FK。已有密文/AAD/密码 MUST 不变。成功结果与 Operation 终态、事件、outbox、审计及租约释放 MUST 同事务提交，initialized_at MUST 对应 format v2 与有效原生 ID。失败不设置这些字段，不将 Repository 改为 READY。

repository_leases 以 repository_id 为主键，保存 operation_id、owner、kind（MAINTENANCE/BACKUP）和 expires_at。当前初始化只取得 MAINTENANCE；备份与维护 MUST 复用此 admission 边界，不得旁路；schema 11 的数据面占用是额外必须检查的持久化 fence，见 §5.4.4。领取按 job→operation→Host→Repository→credential→lease→audit 锁定，仓库 advisory xact lock 协调 admission；job/repository 两份租约 MUST 原子续租。任何有效备份/维护 lease 阻止领取。runtime MUST 单实例并等待子进程退出，DB lease 不等价于云端写入 fencing。

运行角色只增加租约 SELECT/INSERT/UPDATE 和仓库初始化 metadata 列 UPDATE，不授予 DELETE 或密码字段 UPDATE。有初始化 Operation 时 Down MUST 在删除字段前失败；生产使用前向修复。

### 5.4.3 Agent 凭据交付（schema 10）

`repository_agent_deliveries` MUST 以 agent_id 为主键，保存当前交付 UUID、Repository、单调 revision、Gateway origin+CA 指纹、两个加密 secret_ref、created_at 和可空 accepted_at，不存明文。首次交付/变更 MUST 与脱敏 outbox、秘密访问审计同事务；旧 secret/AAD 不变。锁顺序 Agent→Host→Repository→StorageCredential→delivery→audit，与吊销兼容。

ACK MUST 重验 ACTIVE Agent/Host、未禁用的仓库/存储凭据、当前交付 ID/revision/配置指纹/秘密引用；只确认当前交付并完成旧 outbox，不改变仓库状态。应用角色仅有 SELECT/INSERT/UPDATE，MUST NOT 获得 DELETE；已有交付记录时 Down MUST 拒绝删除历史。

### 5.4.4 备份占用（schema 11，ADR-0016）

`gateway_backup_admissions` MUST 保存中心生成的准入 ID/owner、认证 Agent 的 Host/Repository/Gateway/StorageCredential 绑定、当前已 ACK 的 delivery ID/配置指纹/仓库秘密引用、创建/到期/释放时间。MUST NOT 保存秘密明文、可执行配置或虚构 Backup Operation。delivery ID 是不可变快照，不 FK 到会被替换的当前交付行。

首次准入 MUST 按 Agent→Host→Repository→独占 StorageCredential→admission→audit 锁定，复用 Repository advisory xact lock；凭据锁必须直接取得 FOR UPDATE，MUST NOT 先共享再升级造成跨 Host 死锁。必须验证 Agent/Host ACTIVE、仓库已初始化且为 PROVISIONING/READY/DEGRADED、凭据未 DISABLED、精确当前凭据 ACK。同凭据任何未终态 Operation 或有效 repository lease MUST 阻止准入。

准入至少 1min、最多 24h，使用 DB UTC 时间；并发精确重放返回原始 ID/截止时间，不续期或重复 outbox。改变 owner/Agent/delivery/配置/时长、重开过期或已释放 ID MUST 拒绝。创建和释放 MUST 与脱敏 outbox/审计同事务；审计等待后必须再次检查截止时间。

未释放占用对 Host、Agent、Repository、Gateway identity、StorageCredential 分别有 partial unique index，条件只能是 released_at IS NULL，MUST NOT 将到期当作清理成功。现有 enqueue、claim、refresh/completion、初始化租约取得和凭据替换 MUST 在相同凭据锁下检查此 fence。禁用凭据仍可执行，后续在线使用校验拒绝它，但不隐式释放占用。

只有可信中心 owner 在完成请求/会话/进程/刷新清理后 MAY 幂等释放；Agent 无释放权。在线 Check MUST 重验当前身份、ACK、配置、未释放与未到期；不解密材料、不改变 Repository 状态。应用角色只获得 SELECT/INSERT 和 released_at 列 UPDATE，不授予 DELETE、重绑 owner 或延长截止时间。Down 在任何历史记录存在时 MUST 整体拒绝。

此批没有公网或 gRPC 申请入口，也不自动消费准入 outbox；进程接线、崩溃 owner 恢复及 AGT-005 离线授权/审计/OAuth 刷新仍待完成。生产升级接线前 MUST 停止所有旧版中心 writer，再运行 schema 11 与新版 writer；不了解占用表的旧二进制 MUST NOT 和新版准入并行。

占用 owner 的云端材料读取和 token-only 刷新 MUST 复用同一在线校验/锁顺序；材料只返回当前 StorageCredential revision 对应的密文，MUST 与秘密访问审计提交后才借出解密结果。刷新 MUST 在中心重新解析配置、拒绝目标/Crypt/client/非 token 变化，以预期 secret revision 做 CAS，密文版本、metadata、last_refreshed_at 和脱敏审计原子提交。此 owner 专用入口不绕过其他 writer 的占用 fence；到期/释放/撤销/禁用/ACK 或配置变化 MUST 拒绝。审计等待后 MUST 再检查 DB 截止时间，失败不返回材料、不留下部分版本。无新增 schema 或公开协议。

### 5.5 template_revisions / plan_revisions

每次变更保存不可变 snapshot：

```text
template_revisions(template_id, revision, config_json, config_hash, created_by, created_at)
plan_revisions(plan_id, revision, effective_config_json, config_hash, created_by, created_at)
```

Primary key 为 `(id, revision)`；`config_hash` 使用 canonical JSON SHA-256。`plans` 保存 current desired/accepted revision 和当前索引字段。

### 5.6 plans

约束：

- `host_id`、`repository_id`、`agent_id` 必须在应用事务中验证同一 Host；
- `desired_revision >= accepted_revision`；
- enabled plan 必须有完整 effective config；
- `(host_id, lower(name))` 对 active plans 唯一；
- `backup_health` 与 `status` 使用 CHECK；
- archived plan 不允许 ACTIVE。

### 5.7 operations

```sql
create table operations (
  id uuid primary key,
  type text not null,
  status text not null,
  source text not null,
  host_id uuid references hosts(id),
  agent_id uuid references agents(id),
  repository_id uuid references repositories(id),
  plan_id uuid references plans(id),
  plan_revision bigint,
  config_hash text,
  requested_by_user_id uuid references users(id),
  parent_operation_id uuid references operations(id),
  idempotency_key text,
  attempt integer not null default 1,
  created_at timestamptz not null,
  dispatch_deadline timestamptz,
  dispatched_at timestamptz,
  acknowledged_at timestamptz,
  started_at timestamptz,
  finished_at timestamptz,
  lease_owner text,
  lease_expires_at timestamptz,
  exit_code integer,
  error_code text,
  error_summary text,
  statistics jsonb not null default '{}',
  snapshot_id text,
  cancel_requested_at timestamptz,
  check (attempt > 0),
  check (snapshot_id is null or snapshot_id ~ '^[0-9a-f]{64}$')
);
```

状态转换只能通过 repository method：

```text
transition_operation(id, expected_statuses, new_status, metadata)
```

它在事务中 row lock、验证允许边、写 operation_event 与 outbox。禁止通用 PATCH status。

索引：

- `(status, created_at)` 对未终态；
- `(agent_id, status)`；
- `(repository_id, created_at desc)`；
- `(plan_id, created_at desc)`；
- `(created_at desc, id desc)` 用于 cursor；
- source/idempotency unique partial indexes。

M4 迁移 00006 首先开放 CREDENTIAL_TEST Operation：通过 storage_credential_id 与 secret_revision 绑定被测试版本。尚不存在的 Repository/Plan 资源列及其他操作类型随对应里程碑迁移加入。Operation/events/jobs/idempotency 的事务、租约和恢复语义见 [ADR-0009](../adr/0009-credential-test-jobs.md)。

### 5.8 jobs

```text
id
operation_id unique
queue
payload jsonb
status READY | LEASED | DONE | DEAD
available_at
lease_owner
lease_expires_at
attempt
max_attempts
last_error_code
created_at / updated_at
```

Worker claim：

```sql
select ...
from jobs
where status = 'READY' and available_at <= now()
order by available_at, id
for update skip locked
limit $n;
```

同一事务更新 LEASED。Worker 定期续租；过期 lease 可重新 claim。Job handler 必须幂等。

### 5.9 operation_log_chunks

Primary key `(operation_id, sequence)`，保证重复上报幂等。Content 使用 `bytea` 或 text；只存 redacted 内容。保留 byte_count/truncated。大规模部署可按月 partition，V1 可暂不 partition 但 Schema/migration 测试需评估。

### 5.10 snapshots

Primary key `(repository_id, id)`，其中 id 是完整 snapshot SHA。索引：

- `(repository_id, time desc, id)`；
- `(host_id, time desc)`；
- `(plan_id, time desc)`；
- GIN tags 仅在查询需要时增加；
- `missing_at` partial index。

Snapshot 同步使用 staging table 或 transaction upsert；只有完整成功的 `restic snapshots --json` 才更新 missing markers。

### 5.11 snapshot_entry_cache

Cache table 可以删除/rebuild。Primary key 包含 repository、snapshot、cache generation、path。必须有 `expires_at` 索引与总量清理。不要让 cache FK cascade 意外触发 repository object 删除；它仅是 DB rows。

### 5.12 audit_events

Append-only：应用 DB role 不授予 UPDATE/DELETE。字段见领域规范。

Hash chain：

```text
event_hash = SHA-256(canonical_event_without_hash || previous_hash)
```

Hash chain 用于发现 DB 内篡改，不替代外部 WORM/日志导出。并发写通过单独 audit sequence/row lock 或分 shard chain 实现；V1 可用单链。

## 6. Transactional Outbox

任何需要异步后续处理的事务同时写 `outbox_events`：

```text
id
event_type
aggregate_type / aggregate_id
payload jsonb (redacted)
created_at
available_at
published_at
attempt
lease_owner / lease_expires_at
```

示例：Plan revision + `DESIRED_STATE_CHANGED` outbox 必须同一事务。Publisher 至少一次处理，消费者幂等。

## 7. Idempotency Records

```text
scope_hash
idempotency_key_hash
request_hash
response_status
resource_type / resource_id
created_at / expires_at
```

Primary key `(scope_hash, idempotency_key_hash)`。不保存原始 key。并发首次请求用 unique constraint 决定 winner。

## 8. Leases 与 Advisory Locks

- durable worker/job lease 存表；
- repository destructive operation 同时使用 transaction advisory lock，key 从 repo UUID 稳定派生；
- Agent backup active lease 由 heartbeat/operation 更新；数据面已委托的占用还必须经可信 Gateway 清理确认才释放，不能由 Agent 心跳或超时单独释放；
- maintenance 开始前检查 active leases，并在一个 transaction 中取得 maintenance lease；
- advisory lock 只作为进程并发保护，业务可见状态仍存表。

## 9. Secret 与权限分离

建议数据库角色：

- `restfleet_app`：普通业务表、secret ciphertext；不能读取 migration metadata 之外的管理对象；
- `restfleet_migrator`：schema DDL，仅启动 migration job 使用；
- `restfleet_audit_writer`：audit INSERT/SELECT，无 UPDATE/DELETE；
- 运维只读角色可选。

即使 V1 单进程共用连接池，迁移与 runtime credential 必须分开。

## 10. 数据保留

- Operations metadata：默认无限或管理员策略；
- Raw logs：默认 90 天，终态 summary 永久；
- Agent inventory history：默认 30 天，保留最新；
- Notification deliveries：90 天；
- Sessions：过期后 30 天清理；
- Token hashes：used/expired 后 30 天保留审计关联；
- AuditEvents：V1 不自动删除；
- Snapshot cache：TTL/LRU，可随时重建。

这些 DB 保留策略与 Restic Snapshot Retention 完全分开。

## 11. 备份控制面数据库

RestFleet 管理自己的备份不能形成循环依赖：

- PostgreSQL 使用 `pg_dump`/volume snapshot 的外部管理方案；
- master key 必须与 DB backup 分开安全备份；
- 丢失 master key 会使 rclone/Repo/CA secret 不可恢复；
- 恢复演练需同时验证 DB、master key、CA 与 rclone credential；
- 不允许把 RestFleet 唯一控制面备份只存入一个无法在 RestFleet 外访问的 Repository。

## 12. Migration 安全

- Server 启动时默认检查 migration，不在多个副本并发跑 DDL；
- Docker Compose 可用独立 one-shot migrate command；
- expand/contract 模式支持 N/N-1 Server；
- enum 增加先扩 schema，再发布 reader/writer；
- 大表索引使用并发创建或维护窗口；
- migration 日志禁止打印 secret ciphertext/metadata payload。

## 13. 数据库验收

- 所有 FK、unique、CHECK 与 partial indexes 有测试；
- 非法 Operation transition 在 DB/service 层均被拒绝；
- 两 worker 不会同时 claim 同一 job；
- lease expiry 后可恢复；
- Plan 与 outbox 原子提交；
- secret nonce 唯一性与 AAD mismatch 解密失败；
- audit role 无 UPDATE/DELETE 权限；
- full snapshot index 失败不标记 snapshots missing；
- cursor pagination 在并发插入下无重复/大面积遗漏。

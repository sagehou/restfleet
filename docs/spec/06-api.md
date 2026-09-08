# 06 — HTTP API

## 1. API 原则

- Base path：`/api/v1`；
- JSON：UTF-8，字段 `snake_case`；
- 契约来源：`api/openapi/restfleet-v1.yaml`；
- 错误：`application/problem+json`，遵循 RFC 9457；
- 时间：RFC 3339 UTC；
- ID：UUIDv7；Snapshot 使用完整 SHA-256 hex；
- 分页：opaque cursor，不使用不稳定的大 offset；
- 写入幂等：适用端点要求 `Idempotency-Key`；
- 并发更新：ETag + `If-Match`；
- API 不返回任何可复用 secret，除创建/轮换时的一次性响应；
- 长任务返回 `202 Accepted + Operation resource`，不在 HTTP handler 中阻塞执行。

## 2. 认证与 Session

### 2.1 Bootstrap

```http
GET  /api/v1/bootstrap/status
POST /api/v1/bootstrap
```

`status` 仅返回是否需要初始化，不返回 secret 或用户信息。创建首个 Admin 仅在无 User 且 bootstrap secret 有效时可用。请求使用 `X-RestFleet-Bootstrap-Token` 专用 header 提交，成功后 bootstrap 永久关闭。

### 2.2 Login/Logout

```http
POST   /api/v1/auth/login
POST   /api/v1/auth/logout
GET    /api/v1/auth/session
```

Session 使用 HttpOnly cookie。所有状态修改请求需 CSRF header。登录失败返回统一错误，不泄露用户名是否存在。

## 3. 通用响应

### 3.1 Resource

```json
{
  "id": "019...",
  "revision": 3,
  "created_at": "2026-09-02T12:00:00Z",
  "updated_at": "2026-09-02T12:30:00Z"
}
```

Header：

```http
ETag: "3"
X-Request-ID: 019...
```

### 3.2 List

```json
{
  "items": [],
  "next_cursor": "opaque-or-null"
}
```

常用 query：`limit` 默认 50，最大 200；`cursor`；资源特定 filter/sort。未知 sort/filter 返回 400，不能静默忽略。

### 3.3 Problem

```json
{
  "type": "https://restfleet.dev/problems/revision-conflict",
  "title": "Revision conflict",
  "status": 412,
  "detail": "The resource changed after it was loaded.",
  "instance": "/api/v1/plans/019...",
  "code": "REVISION_CONFLICT",
  "request_id": "019...",
  "errors": [
    {"field": "schedule.cron", "code": "INVALID_CRON"}
  ]
}
```

`detail` 不包含 secret、内部路径、完整 subprocess command 或 provider token。

## 4. Dashboard

```http
GET /api/v1/dashboard/summary
GET /api/v1/dashboard/attention
```

`summary` 返回带 `collected_at` 的聚合计数。`attention` 返回 failed/overdue/offline/credential/maintenance 事项，按 severity 和时间排序。

### 4.1 Build Version

```http
GET /api/v1/version
```

`version` 返回 Server version、commit、build time 与兼容 schema version，不包含运行环境或依赖 credential。

## 5. Hosts 与 Agents

```http
GET    /api/v1/hosts
POST   /api/v1/hosts
GET    /api/v1/hosts/{host_id}
PATCH  /api/v1/hosts/{host_id}
POST   /api/v1/hosts/{host_id}/disable
POST   /api/v1/hosts/{host_id}/enable
GET    /api/v1/hosts/{host_id}/inventory
GET    /api/v1/hosts/{host_id}/agents

GET    /api/v1/agents/{agent_id}
POST   /api/v1/agents/{agent_id}/revoke
POST   /api/v1/agents/{agent_id}/rotate-gateway-credential
```

Host 创建不自动创建 Repository/Plan，初始 `PENDING`。

Agent revoke：

- 要求 `Idempotency-Key`；
- 请求含 `reason`；
- 返回 202 Operation 或即时 revoke result；
- 立刻拒绝新 stream 并关闭现有 stream；
- 不删除历史数据。

## 6. Enrollment Tokens

```http
POST   /api/v1/hosts/{host_id}/enrollment-tokens
GET    /api/v1/hosts/{host_id}/enrollment-tokens
DELETE /api/v1/enrollment-tokens/{token_id}
```

创建请求：

```json
{"expires_in_seconds": 600}
```

创建响应仅一次包含：

```json
{
  "id": "019...",
  "token": "rfe_<secret>",
  "expires_at": "...",
  "install": {
    "native": "...",
    "docker": "..."
  }
}
```

List 永不返回 token，只返回 fingerprint/status。`DELETE` 表示 revoke 未使用 token，是允许的安全删除操作，但必须审计。

Public enrollment endpoint：

```http
POST /api/v1/agent-enrollment
```

不使用 Web session，按 `04-agent-protocol.md` 验证 token/CSR。

## 7. Storage Credentials

```http
GET    /api/v1/storage-credentials
POST   /api/v1/storage-credentials
GET    /api/v1/storage-credentials/{id}
PATCH  /api/v1/storage-credentials/{id}
POST   /api/v1/storage-credentials/{id}/test
POST   /api/v1/storage-credentials/{id}/replace-secret
POST   /api/v1/storage-credentials/{id}/reauthenticate
POST   /api/v1/storage-credentials/{id}/disable
```

V1 `POST`/`replace-secret` 支持导入受限的 rclone config/provider credential bundle。Response 返回 metadata 和 status，不返回原 Secret。

`test`、`reauthenticate` 返回 Operation。OAuth redirect/callback 若进入 V1.1，使用短期 state、PKCE，并与发起 Session 绑定。

### 7.1 M4 第一批已实现的凭据 API

已实现 GET/POST collection、GET detail、POST replace-secret/disable；metadata list 支持 limit（1–200）与 opaque cursor，未知或重复 query 参数返回 400。导入与替换请求的 rclone_config 为 writeOnly，原始字段白名单见 [ADR-0007](../adr/0007-storage-credential-import.md)，多后端扩展以 [ADR-0012](../adr/0012-rclone-multiple-backends.md) 为准。

replace-secret/disable MUST 使用 If-Match；发生并发变更返回 412。替换 MUST 保持存储目标和 Crypt 设置，违反返回 409 STORAGE_TARGET_CHANGED；禁用后替换返回 409 CREDENTIAL_DISABLED。导入与替换仅返回 UNTESTED metadata，不证明远端可用。未配置 master key 时修改返回 503 STORAGE_UNAVAILABLE。

POST /api/v1/storage-credentials/{id}/test 已实现，要求 ADMIN、CSRF 与 Idempotency-Key，禁止 body/query。返回 202 Operation 与 Location；GET /api/v1/operations/{id} 允许 ADMIN/VIEWER 查询进度。未配置 runtime 返回 503；已有未终态测试返回 409 CREDENTIAL_TEST_BUSY。metadata 新增 last_test_operation_id、last_tested_at、last_test_result、last_refreshed_at。成功表示 Crypt 根目录读取成功，不证明写权限。详细事务与错误语义见 [ADR-0009](../adr/0009-credential-test-jobs.md)。

provider 枚举扩展为 RCLONE_ONEDRIVE、RCLONE_GDRIVE、RCLONE_WEBDAV，由服务端从配置派生；创建请求仍只提交 name/remote_name/rclone_config。替换 MUST 保持后端与固定目标，不得通过更换 provider、Google folder/team ID、WebDAV URL/vendor/user 或 Crypt key 完成迁移。WebDAV 仅静态认证，不支持 token command；所有失败仍返回固定错误，不暴露目标 URL 或 provider 诊断。

reauthenticate 与 metadata PATCH 尚未交付；UI MUST NOT 将这些能力展示为可用。仓库 API 当前范围见 8.1。

## 8. Repositories

```http
GET    /api/v1/repositories
POST   /api/v1/repositories
GET    /api/v1/repositories/{repo_id}
PATCH  /api/v1/repositories/{repo_id}
POST   /api/v1/repositories/{repo_id}/initialize
POST   /api/v1/repositories/{repo_id}/test
POST   /api/v1/repositories/{repo_id}/index
POST   /api/v1/repositories/{repo_id}/disable
GET    /api/v1/repositories/{repo_id}/stats
```

V1 Repository 必须传 `host_id`，且一个 Host 只能有一个 active repo。API 拒绝 shared repository 请求并返回 `SHARED_REPOSITORY_NOT_SUPPORTED`。

Create 只创建 PROVISIONING 记录；initialize 是 202 long operation。也可提供 `initialize=true` convenience，但仍返回 Operation。

任何响应不得包含 backend_path 的完整外部 endpoint、gateway password、Restic password 或 secret ciphertext。

### 8.1 M4 仓库记录批次

已实现 GET/POST collection 与 GET detail。创建要求 ADMIN 与 CSRF，仅接受 name、host_id、storage_credential_id 和可选 shared（默认 false）；shared=true MUST 返回 409 SHARED_REPOSITORY_NOT_SUPPORTED。客户端提交路径、密码、状态或其他未知字段 MUST 返回 400，且不得回显字段值。

Host MUST 为 PENDING/ACTIVE，存储凭据 MUST 未禁用；资源不存在返回 404，Host 不可用返回 409 HOST_UNAVAILABLE，凭据禁用返回 409 CREDENTIAL_DISABLED。UNTESTED 凭据 MAY 用于创建记录，但不得解释为已验证云端读写权限。每个 Host 的唯一性以未归档为界，DISABLED/ERROR 仓库仍占有该 Host。

创建仅返回 201 PROVISIONING metadata、Location 和 ETag；不执行云端操作或派发初始化任务。重复/并发创建同一 Host MUST 返回 409 HOST_REPOSITORY_EXISTS，不生成第二组持久化凭据；请求结果因网络中断不确定时，客户端 SHOULD 刷新列表确认，而不是自动反复创建。未配置 master key 返回 503 STORAGE_UNAVAILABLE。

ADMIN/VIEWER MAY 读取 metadata；列表支持 limit（1–200）与 opaque cursor，拒绝未知/重复 query 参数；detail 禁止 query。API MUST NOT 返回 backend_path、gateway_username、secret_ref、明文或密文。format_version 在中心验证前 MUST 缺省；凭据 revision 仅表示已保存版本，不表示 Agent 已接受。

initialize 已实现，见 8.2；test/index/disable/PATCH/stats 仍未交付；Agent 仓库凭据下发见 04 协议 §13.1，M4 尚未完成。见 [ADR-0011](../adr/0011-repository-records-and-ownership.md)。

### 8.2 中心初始化

POST /api/v1/repositories/{repository_id}/initialize MUST 要求 ADMIN、CSRF 与 Idempotency-Key，拒绝 body/query/重复 key header，返回 202 Operation 与 Location。同 key 返回原任务；新 key 在任务未终态或有有效 repository lease 时返回 409 REPOSITORY_BUSY。共享存储凭据的 test/init 串行；其连接测试入口仍返回 CREDENTIAL_TEST_BUSY。Host 不可用、凭据禁用分别返回 HOST_UNAVAILABLE / CREDENTIAL_DISABLED；非 PROVISIONING 或已初始化返回 REPOSITORY_UNAVAILABLE。缺少初始化 runtime/master key 返回 503。

GET Operation 新增 REPOSITORY_INITIALIZE 类型与 repository_id；Repository metadata 新增 initialized_at 与 last_initialize_operation_id。成功 MUST 保持 PROVISIONING，只设置验证后的 format_version / initialized_at，MUST NOT 返回原生 Restic ID、密码、路径或秘密引用。初始化失败显示固定 error_code，重试必须新 Operation；不得重开终态、自动删除、unlock 或更换密码。Agent 仓库凭据 ACK 已接线，但 READY 仍未交付。租约/恢复详见 [ADR-0013](../adr/0013-repository-initialization-jobs.md)。


### 8.3 M4 Agent 凭据确认只读字段

Repository metadata 的可选 `agent_credential_revision` / `agent_credential_accepted_at` 仅反映当前 ACTIVE Agent、匹配当前秘密引用的交付。ACK 为派生状态，不改变仓库记录 revision/ETag；缺省表示尚未交付/确认，不返回密码、交付 ID、内部路径或 secret_ref，不能推导为备份可用。

## 9. Retention 与 Maintenance Policies

```http
GET/POST       /api/v1/retention-policies
GET/PATCH      /api/v1/retention-policies/{id}
GET/POST       /api/v1/maintenance-policies
GET/PATCH      /api/v1/maintenance-policies/{id}
```

Policy 校验：

- retention 至少一条 keep rule；
- `minimum_snapshots >= 1`；
- cron + timezone 合法；
- maintenance schedule 不允许已知重叠，若可能重叠发 warning；
- V1 不暴露 empty group-by 或 unsafe remove all。

## 10. Templates 与 Plans

```http
GET    /api/v1/templates
POST   /api/v1/templates
GET    /api/v1/templates/{id}
PATCH  /api/v1/templates/{id}
POST   /api/v1/templates/{id}/archive
GET    /api/v1/templates/{id}/dependent-plans
POST   /api/v1/templates/{id}/apply-to-plans

GET    /api/v1/plans
POST   /api/v1/plans
GET    /api/v1/plans/{id}
PATCH  /api/v1/plans/{id}
POST   /api/v1/plans/{id}/pause
POST   /api/v1/plans/{id}/resume
POST   /api/v1/plans/{id}/apply
POST   /api/v1/plans/{id}/backup
POST   /api/v1/plans/{id}/validate
GET    /api/v1/plans/{id}/effective-config
```

Template PATCH 创建新 revision；不自动应用。`apply-to-plans` 必须接受明确 plan IDs 与 expected revisions，返回每个 Plan 的成功/冲突结果。

Plan create/update 返回 normalized effective config、warnings 与 status。Server 必须校验 Agent capabilities、本地 hook keys、Host/Repo binding。

`backup` 需要 Idempotency-Key，返回 202 Operation。

## 11. Operations

```http
GET   /api/v1/operations
GET   /api/v1/operations/{operation_id}
GET   /api/v1/operations/{operation_id}/logs
GET   /api/v1/operations/{operation_id}/events
POST  /api/v1/operations/{operation_id}/cancel
POST  /api/v1/operations/{operation_id}/retry
```

Filter：type、status、host_id、repo_id、plan_id、created_after/before。

Logs：cursor + stream filter，返回 redacted chunks。可选 SSE endpoint：

```http
GET /api/v1/operations/{id}/stream
```

SSE 只传 progress/status/redacted log，不传 secret。断线用 event ID 恢复。

Cancel 对终态 Operation 返回 409 `OPERATION_ALREADY_TERMINAL`。Retry 创建新 Operation，响应含新 ID。

## 12. Snapshots

```http
GET  /api/v1/snapshots
GET  /api/v1/snapshots/{snapshot_id}
GET  /api/v1/snapshots/{snapshot_id}/entries?path=/...
GET  /api/v1/snapshots/{snapshot_id}/download?path=/...
POST /api/v1/snapshots/{snapshot_id}/restore-previews
POST /api/v1/snapshots/{snapshot_id}/restore-jobs
```

Snapshot 必须同时通过 repository context 解析，API 可要求 `repository_id` query 或使用全局唯一复合 lookup。若同一 snapshot ID 在多个 repo 出现，返回 409 让客户端指定 repository。

### Entries

响应：

```json
{
  "snapshot_id": "64hex",
  "path": "/etc",
  "items": [
    {"name":"nginx","path":"/etc/nginx","type":"dir","size":0,"mtime":"..."}
  ],
  "next_cursor": null,
  "source": "cache",
  "collected_at": "...",
  "operation_id": null
}
```

若需要实时扫描，可返回 `202` 和 Operation，或采用受控 streaming endpoint。API 契约必须明确，不允许同一路由随机在 array/stream 间切换。

### Download

- 仅 regular file；
- 使用 user session + CSRF/短期 download intent（GET 本身不得由跨站触发泄露）；
- 设置 no-store；
- 下载开始与结束写 AuditEvent/Operation；
- path 不出现在 access log query，推荐 `POST /download-intents` 后使用一次性 opaque URL。

推荐契约：

```http
POST /api/v1/snapshots/{id}/download-intents
GET  /api/v1/downloads/{opaque-token}
```

token 一次性、5 分钟 TTL、绑定 user/session/snapshot/path，数据库仅存 hash。

## 13. Restore

```http
POST /api/v1/snapshots/{id}/restore-previews
POST /api/v1/restore-jobs
GET  /api/v1/restore-jobs/{id}
POST /api/v1/restore-jobs/{id}/cancel
```

Create 要求：

- `preview_id` 与 `preview_hash`；
- `target_host_id`；
- source paths；
- `target_mode=AGENT_STAGING`；
- `overwrite_policy=NEVER`；
- 非空 `reason`；
- `Idempotency-Key`。

Preview 超过 15 分钟或 snapshot/tree/target capability 变化返回 409 `RESTORE_PREVIEW_STALE`。

## 14. Maintenance Operations

```http
POST /api/v1/repositories/{id}/check
POST /api/v1/repositories/{id}/retention-previews
POST /api/v1/repositories/{id}/forget
POST /api/v1/repositories/{id}/prune
POST /api/v1/repositories/{id}/unlock-previews
POST /api/v1/repositories/{id}/unlock
```

Forget 请求引用 preview：

```json
{
  "preview_id": "019...",
  "preview_hash": "sha256:...",
  "reason": "Scheduled retention / manual reason"
}
```

Server 必须重新校验 snapshot set。`prune` 不接受由用户传入的任意 Restic flags。`unlock` 仅接受 preview 中的 stale lock 集合。

## 15. Notifications

```http
GET/POST   /api/v1/notification-channels
GET/PATCH  /api/v1/notification-channels/{id}
POST       /api/v1/notification-channels/{id}/test
GET        /api/v1/notification-deliveries
```

Webhook：

- HTTPS 默认必需；
- 解析 DNS 后阻止 loopback/link-local/metadata/private ranges，除非管理员显式开启受控 private target；
- 每次 redirect 重新校验目标；
- HMAC signature secret 加密保存；
- response body 只保存截断/清理后的诊断。

Gotify token 属于 secret，只在创建/replace 时接受，不回显。

## 16. Audit

```http
GET /api/v1/audit-events
GET /api/v1/audit-events/{id}
POST /api/v1/audit-exports
```

Filter：actor、action、resource、result、time range。普通 API 不允许 update/delete AuditEvent。Export 是 Operation，产物有 TTL 与访问审计。

## 17. Idempotency

适用于创建 Operation、Enrollment Token、Restore Job、credential rotation、revoke 等副作用端点：

- key scope = authenticated actor + method + canonical route；
- 存请求 body hash、status、response resource ID；
- 同 key 同 body 返回原结果；
- 同 key 不同 body 返回 409 `IDEMPOTENCY_KEY_REUSED`；
- 保留至少 24h；
- Agent schedule 用 deterministic operation key，不使用用户 HTTP key。

## 18. Authorization Matrix

| API 类别 | ADMIN | VIEWER | AGENT mTLS |
|---|---:|---:|---:|
| Dashboard/list/status | ✅ | ✅ | ❌ |
| Host/Plan/Repo 修改 | ✅ | ❌ | ❌ |
| Secret create/replace | ✅ | ❌ | ❌ |
| Download/Restore | ✅ | ❌ | ❌ |
| Maintenance | ✅ | ❌ | ❌ |
| Audit read | ✅ | ❌（V1.1 可细分） | ❌ |
| Enrollment endpoint | ❌ session | ❌ | token + CSR |
| Agent stream | ❌ | ❌ | own certificate only |

## 19. Rate/Size Limits

默认建议：

- login：5/min/IP + account backoff；
- enrollment：10/min/IP；
- state-changing API：60/min/session；
- list：300/min/session；
- concurrent downloads：2/user、4/server；
- request JSON：1 MiB；
- imported credential：256 KiB；
- log chunk：32 KiB；
- reason：1–1000 Unicode chars；
- labels：最多 50 个，key/value 受长度限制。

限额应可配置但必须有安全上限。

## 20. OpenAPI 验收

- 每个 endpoint 定义 auth、request、response、problem codes；
- 生成 client/types 不包含 `additionalProperties: true` 的无界核心对象；
- 示例不得含真实 endpoint/credential；
- contract tests 验证 handler 与 OpenAPI；
- breaking change 需要新 API major 或兼容迁移窗口。

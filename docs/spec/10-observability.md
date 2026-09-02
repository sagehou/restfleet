# 10 — 可观测性、通知与审计

## 1. 目标

可观测性必须帮助回答：

- 哪些 Agent 离线或异常；
- 哪些 Plan 失败、逾期或从未运行；
- 哪个 Repository/credential/maintenance 出问题；
- 一次 Operation 在哪里、为何失败；
- 是否发生了敏感配置、下载、恢复或维护操作；
- 系统是否因自身依赖故障而无法调度/记录。

## 2. Correlation IDs

统一标识：

- `request_id`：每个 HTTP/gRPC request；
- `connection_id`：Agent stream；
- `operation_id`：一次业务执行；
- `job_id`：一次 durable dispatch；
- `host_id/agent_id/repository_id/plan_id`：资源关联；
- `trace_id`：若启用 OpenTelemetry。

不得用 secret/token/完整 credential URL 作为关联字段。

## 3. Structured Logs

JSON log 最低字段：

```text
timestamp
level
component
event
message
request_id / operation_id
resource IDs
error_code
duration_ms
```

禁止字段：

- Enrollment Token；
- Authorization/Cookie/CSRF；
- Agent private key/cert PEM；
- gateway/Restic password；
- rclone config/OAuth/client secret/crypt password；
- URL userinfo；
- subprocess environment；
- 完整 command string。

子进程只记录 executable name、允许的非敏感 flags、operation ID 与 redacted stderr。argv 中的 path 可以按权限显示于 Operation detail，但系统日志默认只显示 count/hash。

## 4. Secret Redaction

两层：

1. Agent 在发送日志前 redaction；
2. Server 在持久化/输出前 redaction。

Redactor 识别：已知 secret exact values（内存安全集合）、URL userinfo、Bearer/Basic header、常见 token JSON、`rfe_` token、PEM private key。不得把“正则没匹配”当唯一保护；首要措施是不记录敏感输入。

CI golden tests 将 canary secrets 经过所有 log/error/audit/metric paths，断言零泄露。

## 5. Metrics

建议 Prometheus names：

### Server

```text
restfleet_http_requests_total{route,method,status_class}
restfleet_http_request_duration_seconds{route,method}
restfleet_agent_connections{status}
restfleet_agent_heartbeats_total{result}
restfleet_agents{health}
restfleet_plans{backup_health}
restfleet_jobs{status,queue}
restfleet_job_latency_seconds{type}
restfleet_operations_total{type,status}
restfleet_operation_duration_seconds{type,status}
restfleet_repository_snapshots{repository_id_hash}
restfleet_repository_last_success_timestamp{operation}
restfleet_credential_status{provider,status}
restfleet_notifications_total{channel_type,status}
restfleet_outbox_lag_seconds
restfleet_db_query_duration_seconds{operation}
restfleet_secret_access_total{type,result}
```

### Agent

Agent 默认通过 heartbeat 上报粗粒度 metrics，不开放公网 `/metrics`：

```text
agent uptime/version/restic version
accepted config revision
active jobs
last scheduled backup result/time
local queue/log buffer size
state/cache/staging free bytes
clock offset
```

禁止 high-cardinality filename、snapshot path、token fingerprint。Resource ID label 应 hash 或仅用于小规模内部部署。

## 6. Health endpoints

### Liveness

只检查 event loop/process，没有 DB/OneDrive 依赖；依赖故障不应触发无意义重启循环。

### Readiness

检查：

- DB connectivity/schema compatible；
- secret master key 可用；
- critical workers heartbeat；
- gateway runtime materialization status（可作为分项）；
- migration 不在失败状态。

Provider/OneDrive 故障可以让 repository/gateway degraded，但不一定让整个 Web/API unready。

## 7. Operation telemetry

每次 Operation 记录：

- queue/dispatch/ack/start/finish 时间；
- executor Agent/worker version；
- config revision/hash；
- exit/error code；
- Restic structured summary；
- retry/root chain；
- cancel actor/reason；
- log truncation；
- snapshot ID 或 restore staging path（受权限）。

状态只从事实推导；不得用日志关键字猜测成功。

## 8. Alert events

### Critical

- repository check corruption/broken packs；
- master key/secret decrypt failure；
- all gateway routes unavailable；
- audit writer failure；
- unexpected maintenance deletion scope mismatch。

### Warning

- Plan failed/overdue/never run；
- Agent offline/degraded；
- credential refresh/test failure；
- config rejected/pending too long；
- prune/check overdue；
- disk/cache/staging low；
- certificate nearing expiry/rotation failure；
- outbox/job lag；
- log buffer truncated。

### Info

- backup recovered after failure；
- restore/download completed；
- Agent enrolled/revoked；
- credential rotated；
- maintenance succeeded。

## 9. 通知去重与恢复

Fingerprint 示例：

```text
event_type + resource_id + error_class
```

规则：

- 同一持续故障在 cooldown 内合并；
- severity 升级立即发送；
- 状态恢复发送 resolved；
- 每个 delivery 有 attempt/backoff/dead-letter；
- 通知失败不能改变原 Operation result；
- 一个坏 webhook 不阻塞其他 channel；
- payload 不包含 secret 或完整敏感日志。

## 10. Gotify

V1 payload：title、severity、resource display name、简短错误、occurred_at、Console deep link、operation/request ID。Token 加密存储。Test 消息明确写 `RestFleet test notification`。

## 11. Webhook

请求：JSON，包含 stable event schema/version、event ID、type、severity、resource refs、summary、timestamp、Console URL。Header 使用 HMAC SHA-256 signature 与 timestamp，receiver 可防重放。

安全：

- endpoint SSRF 防御；
- timeout 默认 10 s；
- response body 截断且 redacted；
- redirect 默认禁用，若启用每跳重新校验；
- TLS 验证；私有 CA 需显式配置；
- secret rotation 支持 overlap。

## 12. Audit Event

必须审计：

- login/logout/failed login；
- Host/Agent create, disable, revoke；
- enrollment token create/revoke/use/failure threshold；
- certificate rotation/revocation；
- Repository/Template/Plan/Policy change；
- Secret create/replace/test/reauth/rotate/disable；
- Backup Now/Retry/Cancel；
- Snapshot browse 的敏感读取阈值、每次 download；
- Restore preview/confirm/cancel；
- Retention preview/forget/prune/check/unlock；
- notification channel changes/tests；
- authorization denied；
- audit export。

Audit `changes` 使用字段级 allowlist，secret 只记 `secret_revision: old→new`。

## 13. 审计完整性

- `audit_events` 仅 INSERT；
- rolling hash chain 发现 DB 内改写；
- 每日 chain head 可选发送到外部 webhook/文件；
- hash chain 不等于 WORM；V2 可接 syslog/对象锁；
- audit writer 失败时高风险操作 fail closed：credential、restore、download intent、forget/prune/unlock 不执行；
- 普通只读 Dashboard 在 audit subsystem 短暂故障时可继续，但明确 degraded。

## 14. Diagnostics bundle

管理员可创建 TTL-bound bundle：

- version/build/schema；
- recent health/metrics summary；
- redacted logs；
- configuration shape（无 secret/真实 token）；
- worker queue summary。

生成与下载均审计。Bundle 创建前运行 canary redaction scan；发现疑似 secret 时拒绝生成并告警。

## 15. 数据保留与隐私

- 系统只采集完成运维目标所需的 Host inventory；
- 默认不采集文件名到中心 Operation logs；Snapshot Browser 是用户显式读取；
- access IP 可 hash/pseudonymize；
- UI 清楚显示 log/audit retention；
- 清理 DB logs 不影响 Restic snapshots；
- Audit 默认不自动删除。

## 16. 可观测性验收

- 每个失败 Operation 可通过 ID 定位结构化错误和日志；
- Agent Online/Backup Healthy 指标独立；
- 同故障通知去重且恢复有 resolved；
- audit writer 故障使危险操作 fail closed；
- canary secret 在所有 telemetry 输出中不可见；
- 高 cardinality 测试不会使 metrics endpoint 爆炸；
- Server/worker restart 后 Operation timeline 保持一致。

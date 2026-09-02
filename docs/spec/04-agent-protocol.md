# 04 — Agent 协议

## 1. 协议目标

Agent 协议必须支持：

- 一次性安全 Enrollment；
- Agent 主动建立并维持 mTLS 连接；
- DesiredState 的版本化下发与确认；
- Agent 离线本地调度；
- 在线远程 Backup Now、Retry、Cancel、Restore；
- 结构化进度、日志与结果补报；
- 至少一次投递下的幂等；
- Server/Agent N 与 N-1 minor 版本滚动升级。

协议不承载备份文件内容、快照下载内容或 rclone credential。

## 2. Transport

- gRPC over HTTP/2；
- Enrollment 前使用 Server-authenticated HTTPS REST；
- Enrollment 后所有 Agent RPC 使用 mTLS；
- Agent 发起 outbound connection；Server 不连接 Agent；
- 默认 keepalive 30 s，heartbeat 15 s，offline threshold 45 s；
- keepalive 参数必须避免触发常见 reverse proxy 的过度 ping 限制；
- 最大单消息 1 MiB；日志使用 chunk 流，不允许任意大消息；
- 使用 protobuf，package 如 `restfleet.agent.v1`。

## 3. Enrollment

### 3.1 请求

`POST /api/v1/agent-enrollment`

```json
{
  "token": "rfe_<secret>",
  "csr_pem": "-----BEGIN CERTIFICATE REQUEST-----...",
  "install_id": "019...",
  "agent_version": "0.1.0",
  "protocol_version": "1.0",
  "hostname": "vps-01",
  "os": "linux",
  "arch": "amd64",
  "capabilities": ["backup", "restore_staging", "hooks_v1"]
}
```

### 3.2 验证顺序

1. 请求大小与速率限制；
2. token 格式校验并计算 keyed hash；
3. 事务锁定 token row；
4. 检查未使用、未撤销、未过期；
5. 验证 CSR signature、Ed25519 key、SAN 请求不被信任；
6. 检查 os/arch/protocol compatibility；
7. 创建 Agent ID，Server 自行构造证书 subject/SAN；
8. 签发证书并原子消费 token；
9. 写 AuditEvent；
10. 返回 identity bundle。

### 3.3 响应

```json
{
  "agent_id": "019...",
  "host_id": "019...",
  "certificate_pem": "-----BEGIN CERTIFICATE-----...",
  "ca_bundle_pem": "-----BEGIN CERTIFICATE-----...",
  "not_after": "2026-10-02T12:00:00Z",
  "server_name": "control.example.com",
  "grpc_endpoint": "control.example.com:443",
  "heartbeat_interval_seconds": 15
}
```

私钥不在响应中。Agent 先以临时文件写入并 `fsync + rename` 原子替换 identity bundle。

## 4. 主连接

```proto
service AgentControlService {
  rpc Connect(stream AgentToServer) returns (stream ServerToAgent);
}
```

连接建立后：

1. TLS 层验证 client cert；
2. Server 从证书解析 agent_id 并查询状态；
3. Agent 发送 `Hello`；
4. Server 返回 `Welcome`，包含 server time、protocol selection、desired revision；
5. 双方开始 heartbeat、config 与 job stream。

### 4.1 通用 Envelope

```text
message_id          UUIDv7
protocol_version    major.minor
sent_at             UTC timestamp
sequence            connection-local uint64
payload             oneof
```

Agent payload 不包含可信 agent_id；Server 使用证书 identity。`sent_at` 用于诊断，不能单独用于授权。

## 5. 消息类型

### Agent → Server

- `Hello`
- `Heartbeat`
- `InventoryReport`
- `ConfigAccepted`
- `ConfigRejected`
- `JobAcknowledged`
- `JobStarted`
- `JobProgress`
- `JobLogChunk`
- `JobFinished`
- `JobRejected`
- `CredentialRevisionAccepted`
- `CertificateRotationRequest`
- `LocalOperationRecovered`

### Server → Agent

- `Welcome`
- `DesiredStateSnapshot`
- `JobDispatch`
- `JobCancel`
- `CredentialRevision`
- `CertificateRotationResponse`
- `ServerDrain`
- `Ping`

## 6. Hello / Welcome

`Hello`：

```text
install_id
boot_id
agent_version
supported_protocol_versions[]
accepted_config_revision
last_acked_server_sequence
capabilities[]
restic_version
local_time
pending_result_ids[] (bounded)
```

`Welcome`：

```text
connection_id
selected_protocol_version
server_time
heartbeat_interval
desired_config_revision
minimum_agent_version
drain_deadline optional
```

若 major 不兼容，Server 返回明确 status 并关闭连接；不得尝试下发未知任务。

## 7. Desired State

Server 下发完整规范化快照，不下发需要 Agent 自行合并的增量：

```json
{
  "revision": 42,
  "generated_at": "2026-09-02T12:00:00Z",
  "config_hash": "sha256:...",
  "plans": [],
  "repositories": [],
  "runtime_policy": {
    "max_parallel_io_jobs": 1,
    "log_limit_bytes": 10485760
  }
}
```

Agent 处理步骤：

1. Schema/version 校验；
2. path、cron、timezone、hook capability 与 secret revision 校验；
3. 写入 bbolt 的 staging bucket；
4. `fsync` 后切换 active revision；
5. 重建 scheduler；
6. 回 `ConfigAccepted`。

若失败，保留 last-known-good，不部分应用，并返回 `ConfigRejected{revision,error_code,field_path}`。Server 显示 Pending/Rejected，不把 desired revision 当 active。

## 8. 本地调度

每个 Plan 包含：

```text
cron
timezone
jitter_seconds
misfire_grace_seconds
concurrency_policy = FORBID
retry_policy
```

规则：

- cron 使用标准 5-field，不含 seconds；
- timezone 必须是 IANA zone；
- Agent 计算并持久化 `next_run_at`；
- jitter 基于 `(plan_id, scheduled_at)` 的稳定 hash，重启不改变；
- 在 misfire grace 内重启可执行一次补偿；超过 grace 跳过并上报 MISSED；
- 同一 scheduled time 只生成一个 deterministic local operation key；
- `FORBID` 时前一 backup 仍运行则跳过并上报 CONCURRENCY_SKIPPED；
- 暂停 Plan 不取消已经 RUNNING 的 Operation，除非另行发送 cancel。

## 9. Job Dispatch

`JobDispatch` 包含：

```text
job_id / operation_id
type
created_at
not_before
deadline
plan_id / plan_revision / config_hash
payload
```

Agent 必须：

- 以 `job_id` 幂等去重；
- 已执行过则重发已保存的最终结果；
- 不支持 type/capability 时 `JobRejected`；
- deadline 已过时不执行；
- config hash 不匹配且 job 依赖 Plan 时拒绝；
- 接受后先持久化，再回 ACK；
- 运行前检查本地资源/并发锁。

Delivery 是 at-least-once。不得假设单次消息只到达一次。

## 10. Backup Operation 报告

Agent 将 Restic `--json` JSONL 映射为：

- `status` → `JobProgress`；
- `error` → bounded structured warning；
- `summary` → final statistics/snapshot ID；
- process exit code → Operation terminal classification。

规则：

- exit `0` + summary → SUCCEEDED；
- exit `3` + snapshot ID → SUCCEEDED_WITH_WARNINGS；
- exit `130` 或收到 cancel → CANCELED；
- exit `11` → FAILED/REPOSITORY_LOCKED，可重试；
- exit `12` → FAILED/WRONG_REPOSITORY_PASSWORD，不自动高频重试；
- 未知 exit code → FAILED/UNKNOWN_EXIT_CODE；
- exit `0` 但缺少必须 summary → FAILED/INVALID_ENGINE_OUTPUT。

未知 JSON field/message type 记录 debug counter 后忽略；不得因为新增非关键字段崩溃。

## 11. 日志流与离线缓冲

- 日志 chunk sequence 每 Operation 单调递增；
- 默认 chunk <= 32 KiB；
- Agent 在发送前做本地 secret redaction；Server 再做第二层 redaction；
- Server ACK highest contiguous sequence；
- 断线时 Agent 本地最多缓存每 Operation 10 MiB；
- 超限丢弃中间 raw logs，但保留首尾、errors、summary 和 truncation marker；
- 重连后先补报 terminal results，再补日志；
- Server 对重复 `(operation_id, sequence)` 做幂等 upsert。

## 12. Heartbeat

```text
boot_id
uptime_seconds
accepted_revision
active_operations[] (IDs and coarse status)
next_runs[] (bounded)
restic_version
disk_free_bytes for Agent state/cache
clock_offset_estimate
health_checks[]
```

不得上报 backup path 中的任意文件名。Heartbeat 超过 1 MiB 或列表异常增长应被拒绝并记录安全事件。

## 13. Credential Revision

Server 只下发该 Repository 的：

- gateway endpoint；
- gateway username/password；
- Restic repository password/key；
- CA bundle；
- revision、valid_from、retire_after。

不下发 rclone remote、OAuth token、crypt password或 admin endpoint。

Agent 将 secret 写到专用 mode 0600 文件/加密 local store，回 ACK 后 Server 才进入旧 secret retirement。Secret 消息不得写入一般 message trace。

## 14. Certificate Rotation

- Agent 在证书剩余 7 天时生成新 CSR；
- 请求通过现有 mTLS stream；
- Server 检查 Agent ACTIVE、旧证书未撤销、key policy 合法；
- 返回新 cert 与 trust bundle；
- Agent 原子安装后用新连接验证成功；
- Server 标记旧 cert superseded，并允许最多 24h overlap；
- 失败时指数退避，但到期前进入 DEGRADED 并告警。

## 15. Reconnect 与恢复

Server 重启或网络断开：

- Agent 使用 exponential backoff + full jitter，1s–5min；
- 本地 scheduler 独立运行；
- 重连 Hello 带 accepted revision 和 pending result IDs；
- Server reconciliation 比较 desired/accepted；
- Server 对本地发起的 schedule operation 以 deterministic key upsert；
- 旧 boot_id 下 RUNNING 且新 Hello 不报告的 Operation 在 grace 后转 LOST；
- Agent 启动时发现本地 RUNNING record，但进程不存在，回 `LocalOperationRecovered` 并标记 LOST/FAILED，不伪造成功。

## 16. Cancel

- Server 发 `JobCancel{job_id,reason,deadline}`；
- Agent 幂等记录 cancel request；
- 对 Restic process group 先 SIGINT，grace 后 SIGTERM，再 SIGKILL；
- pre-hook 未结束时同样取消整个 process group；
- 生成 CANCELED 或 FAILED_CANCEL_TIMEOUT；
- 已终态 job 回报原终态，不改写为 CANCELED。

## 17. 协议兼容

- protobuf field number 永不复用；
- 删除字段先 deprecated 至少两个 minor；
- enum 新值必须有 UNKNOWN=0；
- Server 在 Welcome 选择双方最高共同 minor；
- Agent capabilities 控制 Restore、Hooks 等可选功能；
- 新 Server 必须支持前一 minor Agent 完成 backup/heartbeat；
- 强制最低 Agent 版本只用于明确安全修复，并在 UI 显示原因与 deadline。

## 18. 协议测试

必须有：

- golden protobuf fixtures；
- duplicate/out-of-order log chunk；
- duplicate JobDispatch；
- Server crash before/after ACK；
- Agent crash before/after process start；
- expired enrollment/token replay；
- revoked certificate；
- N/N-1 compatibility；
- unknown JSONL message/Restic exit code；
- offline schedule + later result reconciliation。

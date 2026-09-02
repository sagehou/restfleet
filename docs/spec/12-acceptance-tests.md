# 12 — V1 验收测试

## 1. 规则

- **P0**：发布阻断；安全不变量或数据正确性。
- **P1**：V1 功能阻断。
- **P2**：可在明确记录的限制下延后。
- 每个测试必须自动化，除标记 `MANUAL_DRILL` 的灾备/真实云端场景。
- 测试输出不得包含真实 secret；使用 canary secret。
- 集成测试锁定 Restic/rclone 版本与 checksum/image digest。

## 2. Bootstrap 与 Web Auth

| ID | P | Given / When / Then |
|---|---:|---|
| AUTH-001 | P0 | 空数据库且 bootstrap secret 有效；创建首个 Admin；创建成功且 secret 永久失效。 |
| AUTH-002 | P0 | 已有 User；再次调用 bootstrap；请求被拒绝且审计。 |
| AUTH-003 | P0 | 错误登录连续发生；系统限速/退避，响应不泄露用户名存在性。 |
| AUTH-004 | P0 | 已登录 Session；缺少/错误 CSRF 修改资源；403 且无变更。 |
| AUTH-005 | P1 | Session idle/absolute TTL 到期；后续请求 401，session hash 标记失效。 |
| AUTH-006 | P0 | Canary secret 经过 login/problem/log/audit；所有输出均无明文。 |

## 3. Enrollment 与 PKI

| ID | P | Given / When / Then |
|---|---:|---|
| ENR-001 | P0 | 创建 token；DB 只含 keyed hash/fingerprint，不含原 token。 |
| ENR-002 | P0 | 有效 single-use token + 合法 Ed25519 CSR；签发 cert，Agent ACTIVE，token 原子 USED。 |
| ENR-003 | P0 | 两个并发请求使用同一 token；仅一个成功。 |
| ENR-004 | P0 | 过期/revoked/已用 token；Enrollment 均失败且不签证书。 |
| ENR-005 | P0 | CSR signature 无效或 key type 不允许；失败且 token 未被错误消费。 |
| ENR-006 | P0 | CSR 请求伪造 SAN/agent_id；签发证书只使用 Server 构造身份。 |
| ENR-007 | P0 | Agent private key；Server/DB/network capture 中不存在该 private key。 |
| ENR-008 | P0 | 被 revoke Agent 用仍有效 cert 建连；拒绝并记录安全审计。 |
| ENR-009 | P1 | cert 剩余 7 天；Agent 轮换、验证新连接、旧 cert overlap 后失效。 |
| ENR-010 | P1 | Docker Agent 容器重建且 state volume 保留；install identity 不变，不创建新 Agent。 |

## 4. Agent 连接与配置

| ID | P | Given / When / Then |
|---|---:|---|
| AGT-001 | P0 | Agent 无任何入站端口；仍能通过 outbound gRPC 完成管理。 |
| AGT-002 | P0 | 证书 agent A，payload 自报 agent B；Server 仍只按 A 授权。 |
| AGT-003 | P1 | Plan revision 42；Agent 原子保存后 ACK 42，Server 显示 accepted。 |
| AGT-004 | P0 | 新配置非法；Agent 拒绝并继续使用 last-known-good，不部分应用。 |
| AGT-005 | P0 | 中心控制 API/DB 离线 12h，但 Gateway 在线；Agent 按本地计划成功备份。 |
| AGT-006 | P1 | Agent 离线产生 Operation；重连后幂等补报，Server 无重复 Operation。 |
| AGT-007 | P0 | 同一 JobDispatch 重复 3 次；Restic 仅执行一次，重复请求得到同一结果。 |
| AGT-008 | P1 | 日志消息乱序/重复；Server 以 sequence 去重并正确显示。 |
| AGT-009 | P1 | Agent clock 偏移超过阈值；状态 DEGRADED/告警，但授权不只依赖 agent timestamp。 |
| AGT-010 | P1 | N Server + N-1 Agent；backup、heartbeat、config sync 正常。 |

## 5. Local Scheduler

| ID | P | Given / When / Then |
|---|---:|---|
| SCH-001 | P0 | timezone cron 跨 UTC 日期；Agent 在配置 IANA timezone 的正确本地时刻执行。 |
| SCH-002 | P1 | Agent 在 misfire grace 内重启；同一 scheduled time 只补跑一次。 |
| SCH-003 | P1 | Agent 超过 misfire grace 重启；不补跑，回报 MISSED。 |
| SCH-004 | P0 | 上一 backup 仍运行；FORBID 阻止重叠并回报 CONCURRENCY_SKIPPED。 |
| SCH-005 | P1 | 同 Plan/scheduled time 多次重启；stable jitter 和 deterministic key 防重复。 |
| SCH-006 | P1 | Plan pause；后续 schedule 不执行，已运行任务不被隐式取消。 |

## 6. Repository 与 Secret 隔离

| ID | P | Given / When / Then |
|---|---:|---|
| REP-001 | P0 | 新 Host 建 Repo；得到独立 UUID path、Restic password、gateway identity。 |
| REP-002 | P0 | Agent A credential 请求 Agent B repo path；Gateway 拒绝。 |
| REP-003 | P0 | Agent 对已有 repository object 发 DELETE；Gateway 拒绝，对象仍存在。 |
| REP-004 | P0 | Agent 对已有 object 尝试 overwrite；Gateway 拒绝或不改变原内容。 |
| REP-005 | P0 | Agent 备份需要读 index；正常成功，文档/UI 不标记为 write-only。 |
| REP-006 | P0 | Agent filesystem/process/env inventory；不存在 rclone/OneDrive/Crypt/admin secret。 |
| REP-007 | P0 | 数据库 dump 无 master key；所有 secret plaintext 不可获得。 |
| REP-008 | P0 | 错误 master key 启动；Server fail closed，危险操作不可执行。 |
| REP-009 | P0 | Gateway rclone config materialize；位于 tmpfs、0600、重启后重新生成。 |
| REP-010 | P0 | rclone 自动刷新 OAuth token；新 revision 加密回 DB，重启后仍可访问。 |
| REP-011 | P1 | Gateway password rotation；Agent ACK 新 revision 后旧 secret 在 overlap 后失效，备份不中断。 |
| REP-012 | P0 | 请求 Shared Repository；V1 API 返回明确不支持。 |

## 7. Backup 与 Restic 解析

| ID | P | Given / When / Then |
|---|---:|---|
| BAK-001 | P0 | 有效 Plan；Restic argv 不经 shell，路径/排除按独立文件/argv 传入。 |
| BAK-002 | P0 | 文件名含空格、引号、换行、号 restic-like flags；不造成参数/命令注入。 |
| BAK-003 | P1 | Restic exit 0 + summary；Operation SUCCEEDED，统计和 snapshot ID 正确。 |
| BAK-004 | P0 | Restic exit 3 + snapshot；Operation SUCCEEDED_WITH_WARNINGS，Dashboard 不显示全绿。 |
| BAK-005 | P0 | exit 12；WRONG_REPOSITORY_PASSWORD，不进行无限/高频 retry。 |
| BAK-006 | P1 | exit 11；分类 REPOSITORY_LOCKED，并按策略 retry。 |
| BAK-007 | P0 | 未知 exit code；FAILED/UNKNOWN_EXIT_CODE。 |
| BAK-008 | P0 | exit 0 但无 summary；FAILED/INVALID_ENGINE_OUTPUT。 |
| BAK-009 | P1 | JSONL 含未知字段/message；已知 summary 仍解析成功。 |
| BAK-010 | P1 | `--skip-if-unchanged` 无 snapshot ID；Operation 成功并显示“无变化”。 |
| BAK-011 | P0 | pre-hook 只引用 Agent allowlist；Server 发送任意 command/arg 被拒绝。 |
| BAK-012 | P1 | Cancel running backup；终止 process group，Operation CANCELED，无孤儿 Restic。 |

## 8. Snapshot 浏览与下载

| ID | P | Given / When / Then |
|---|---:|---|
| SNP-001 | P1 | `snapshots --json`；完整 ID/tags/plan 映射正确。 |
| SNP-002 | P0 | 无 managed tag Snapshot；标为 UNMANAGED，不进入自动 retention。 |
| SNP-003 | P1 | 一次 index 暂时缺 Snapshot；先 missing，不因单次失败立即隐藏。 |
| SNP-004 | P1 | `ls --json` 1M nodes；流式受限处理，Server/Browser 内存不无界增长。 |
| SNP-005 | P0 | Snapshot path 含 `../`、NUL、header chars；拒绝且不执行 Restic。 |
| SNP-006 | P0 | 下载目录/symlink；V1 regular-file download 拒绝。 |
| SNP-007 | P0 | download intent；一次性、5min、绑定 user/session/path，重放失败。 |
| SNP-008 | P1 | 用户中止下载；Restic process 取消，Operation 有正确终态和审计。 |
| SNP-009 | P0 | access log/problem/audit；不泄露含敏感文件名的 query token 或 repo password。 |

## 9. Restore

| ID | P | Given / When / Then |
|---|---:|---|
| RST-001 | P0 | 有效 preview + confirm；只恢复到 `/var/lib/restfleet/restores/<job-id>`。 |
| RST-002 | P0 | Server 请求任意 absolute target/in-place/`--delete`；Agent 拒绝。 |
| RST-003 | P0 | Snapshot 属于其他 Host repo；restore create/dispatch 被拒绝。 |
| RST-004 | P0 | stale/changed preview；执行返回 conflict，未启动 Restic。 |
| RST-005 | P1 | 恢复成功；统计、staging path、Operation、Audit 一致。 |
| RST-006 | P1 | 恢复中取消；partial staging 保留并明确标记，不远程递归删除。 |
| RST-007 | P0 | symlink/path traversal 尝试逃逸 staging；失败，外部文件不变。 |
| RST-008 | P1 | Docker Agent；只能写已挂载 restore volume，backup mounts 保持只读。 |

## 10. Retention 与 Maintenance

| ID | P | Given / When / Then |
|---|---:|---|
| MNT-001 | P0 | Retention dry-run；只包含 `managed + plan:<id>`，不含其他 Plan/unmanaged。 |
| MNT-002 | P0 | Preview 后 snapshot set 改变；forget 拒绝并要求重新 preview。 |
| MNT-003 | P0 | 策略将删至少于 minimum 1；操作拒绝。 |
| MNT-004 | P0 | 尝试 empty `--group-by` 或 unsafe remove all；API/adapter 均拒绝。 |
| MNT-005 | P0 | Active backup lease；forget/prune/unlock 不启动。 |
| MNT-006 | P1 | 同 Repo 两个 maintenance；只串行执行。 |
| MNT-007 | P0 | Agent 收到 CHECK/FORGET/PRUNE/UNLOCK job；协议层拒绝。 |
| MNT-008 | P1 | Check JSON 报 broken pack；Repo DEGRADED/critical alert，不自动 repair。 |
| MNT-009 | P1 | Prune 中断；状态明确，下一步先 check，不盲目 success/retry。 |
| MNT-010 | P0 | Unlock preview 有 active lock；不得选择；仅 stale lock 可执行。 |

## 11. Notifications、Logs 与 Audit

| ID | P | Given / When / Then |
|---|---:|---|
| OBS-001 | P0 | Canary secrets 经过 Agent log、Server log、Operation log、metrics、audit、notification；零明文。 |
| OBS-002 | P1 | 同一故障持续；cooldown 内去重，恢复后发送 resolved。 |
| OBS-003 | P1 | 一个 webhook 失败；其他 channel 仍投递，原 Operation 状态不变。 |
| OBS-004 | P0 | Webhook 指向 metadata/loopback/private IP；默认 SSRF policy 拒绝。 |
| OBS-005 | P0 | Audit writer 不可用；download/restore/credential/forget/prune/unlock fail closed。 |
| OBS-006 | P0 | Audit row 被修改；hash chain verification 发现异常。 |
| OBS-007 | P1 | Operation 日志超过 10 MiB；bounded、truncated marker 存在、summary 保留。 |
| OBS-008 | P1 | Dashboard；Online 与 Backup Healthy 使用独立字段/计数。 |
| OBS-009 | P1 | Metrics；不存在 filename、raw UUID explosion 或 secret high-cardinality labels。 |

## 12. API 与并发

| ID | P | Given / When / Then |
|---|---:|---|
| API-001 | P0 | 同 Idempotency-Key + 同 body 并发；只产生一个副作用并返回同 resource。 |
| API-002 | P0 | 同 key + 不同 body；409，无第二副作用。 |
| API-003 | P1 | 旧 If-Match 更新 Plan；412，当前配置不被覆盖。 |
| API-004 | P1 | 10k snapshots cursor pagination；无重复/遗漏（固定 snapshot view）。 |
| API-005 | P0 | Problem response；无 subprocess command、backend path credential 或 secret。 |
| API-006 | P1 | SSE 断线重连；从 event ID 恢复，无状态倒退。 |

## 13. Restart 与灾备

| ID | P | Given / When / Then |
|---|---:|---|
| DR-001 | P0 | Server 在业务变更 commit 后/outbox publish 前崩溃；重启后事件仍处理。 |
| DR-002 | P0 | Worker lease 中崩溃；过期后幂等 reclaim，不重复数据副作用。 |
| DR-003 | P1 | Agent 在 Restic start 前/后崩溃；恢复为明确 LOST/FAILED，不永久 RUNNING。 |
| DR-004 | P0 | 只恢复 DB、无 master key；明确不可解密，系统 fail closed。 |
| DR-005 | P0 MANUAL_DRILL | 新中心 + DB backup + master key + CA/credential；恢复后可查询快照并接收旧 Agent。 |
| DR-006 | P1 MANUAL_DRILL | OneDrive throttling/network outage；指数退避、告警去重、恢复后成功。 |

## 14. 多架构与部署

| ID | P | Given / When / Then |
|---|---:|---|
| DEP-001 | P0 | linux/amd64 Native；enroll/config/backup/restore smoke pass。 |
| DEP-002 | P0 | linux/arm64 Native；同上。 |
| DEP-003 | P0 | linux/amd64 Docker；同上且无 privileged/docker socket。 |
| DEP-004 | P0 | linux/arm64 Docker；同上。 |
| DEP-005 | P0 | 外网扫描中心；Postgres/admin gateway/rclone RC 不可达。 |
| DEP-006 | P0 | 错误 CA/hostname；Agent 和 Restic TLS 连接失败，不 fallback insecure。 |
| DEP-007 | P1 | N→N+1 Server upgrade + N Agent；计划备份不中断，schema compatible。 |
| DEP-008 | P0 | Image/artifacts；有 checksum、SBOM、multi-arch manifest、非 latest pin。 |

## 15. 性能与资源

| ID | P | Given / When / Then |
|---|---:|---|
| PERF-001 | P1 | 100 concurrent Agent streams；heartbeat/reconnect 无明显泄漏。 |
| PERF-002 | P1 | 50 Hosts/100 Plans；Dashboard cached P95 <500ms。 |
| PERF-003 | P1 | 10k snapshot metadata；list P95 <300ms（不含 live Restic）。 |
| PERF-004 | P1 | 10 concurrent backups；Gateway/DB/Server 保持 bounded resources。 |
| PERF-005 | P1 | 大日志/目录；Web 不冻结，virtualization/streaming 有效。 |

## 16. Release 结果记录

每次 release candidate 生成：

```text
version / commit
Restic/rclone versions + checksums
DB schema version
test environment
passed/failed/skipped acceptance IDs
manual drill evidence links
known limitations
approver/date
```

任何 P0 skipped/failed 阻止 V1 发布。P1 例外需要明确 risk acceptance 和 follow-up issue。

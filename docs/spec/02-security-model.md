# 02 — 安全模型

## 1. 保护目标

RestFleet V1 优先保护：

1. 历史备份不被已攻陷 Host 删除或覆盖；
2. 单个 Host 被攻陷时不横向泄露其他 Host 的备份；
3. rclone、OneDrive OAuth、Crypt 密码、中心 CA 与维护凭据不离开中心；
4. Agent 身份不可被简单复制或永久冒用；
5. 恢复、下载、Retention 与维护操作可授权、可审计；
6. Secret 不进入日志、命令行参数、指标、前端状态和错误响应。

## 2. 非保护目标与诚实边界

V1 不声称抵御：

- 中心节点 root 完全失陷；
- 管理员账户与 master key 同时失陷；
- Host root 在备份前篡改源文件；
- 云存储提供商删除或回滚全部数据；
- Agent root 读取该 Host 已持有的 Restic repository password；
- Agent 使用标准 Restic 读取/解密自己 Repository 的历史数据；
- 通过流量大小和时间推断备份活动。

### Append-only 的准确含义

`rclone serve restic --append-only` 禁止删除已有 repository data，但 Restic backup 仍需要读取 repository config、keys、snapshots 与 index 进行去重和一致性工作。因此：

- Agent 对自己的 Repository 不是“纯写入”；
- 临时 Restore Token 不能阻止已取得 Agent root 的攻击者自行读取本 Repo；
- 隔离必须依赖 per-Host Repository、独立密码和 path-scoped gateway identity；
- append-only 的主要价值是防删除/防覆盖，而非防本 Host 读取。

## 3. 信任区域

| 区域 | 信任级别 | 持有 Secret |
|---|---|---|
| Browser | 低；可能被 XSS/恶意扩展影响 | 短期 session cookie，不接触明文底层凭据 |
| Control API | 高 | 解密服务访问能力，不长期持有 materialized secret |
| Maintenance Worker | 最高数据权限 | Repo password、rclone config 的短时明文 |
| Public Gateway | 高数据路径权限 | rclone materialized config、gateway auth verifier |
| PostgreSQL | 敏感但不单独可信 | 密文、token hash、metadata |
| Agent | 单 Host 范围可信 | Agent private key、本 Host Repo password/gateway secret |
| OneDrive | 外部依赖 | rclone crypt 后的 repository objects |

## 4. 威胁与控制

| 威胁 | 控制 |
|---|---|
| 被攻陷 VPS 删除备份 | append-only gateway；无 maintenance credential |
| 跨 Host 读取 | per-Host repo/password/user/path isolation |
| Enrollment Token 泄露 | 256-bit random、10 min TTL、single-use、hash-at-rest、原子消费 |
| Agent 私钥复制 | 本地生成、0600、证书短期、server-side revocation、duplicate identity 告警 |
| MITM | Server TLS + Agent mTLS；固定 CA bundle；禁止 insecure TLS |
| 重放 job/result | message ID、job ID、sequence、deadline、幂等状态机 |
| Plan 注入任意命令 | 不支持 shell；Hooks 仅引用 Agent 本地 allowlist ID |
| 路径穿越 | path canonicalization；argv exec；snapshot path validation |
| Secret 进入日志 | typed secret、central redactor、golden negative tests |
| DB 被盗 | AES-256-GCM envelope encryption；master key 不在 DB |
| SSRF | provider endpoints 与 webhook URL 校验；私网/metadata 地址默认禁止 |
| CSRF/session theft | SameSite cookie、CSRF token、CSP、secure cookie、short idle TTL |
| 维护与备份竞态 | repository locks、DB lease、dry-run preview、operation serialization |
| rclone token refresh 丢失 | materialized config watcher、CAS 持久化、refresh health/alert |

## 5. Agent Enrollment 与 PKI

### 5.1 Enrollment Token

- 由 CSPRNG 生成至少 256 bit 随机值；
- 表示形式带前缀 `rfe_`，便于 secret scanner 识别；
- 默认 TTL 10 分钟，最大 60 分钟；
- 默认可用次数 1，V1 不支持 reusable token；
- 数据库只保存 `HMAC-SHA-256(server_pepper, token)` 或等价 keyed hash；
- consume 必须在同一事务中验证未过期、未使用、Host 匹配并写 `used_at`；
- token 只在创建响应显示一次，之后 UI 仅显示后四位 fingerprint；
- 失败按来源 IP 与 token fingerprint 限速，但日志不记录 token。

### 5.2 Agent 密钥

- Agent 本地生成 Ed25519 key pair；
- private key 永不通过 Enrollment 请求发送；
- Agent 提交 CSR、public key、安装 ID、hostname、arch、OS 与 Agent version；
- Server 证书 Subject/SAN 绑定不可变 `agent_id`，不信任 hostname 作为身份；
- private key 文件 `0600`，目录 `0700`，由 Agent service user 拥有；
- Docker 部署必须使用持久 volume 保存 identity；容器重建不得产生幽灵 Agent。

### 5.3 证书生命周期

- Client certificate 默认有效期 30 天；
- 剩余 7 天时通过现有 mTLS stream 自动轮换；
- 轮换证明使用当前有效证书和同一或新 CSR；
- Server 每次建连检查证书链、有效期、agent_id、数据库状态；
- `REVOKED` Agent 即使证书未过期也被 API 层拒绝；
- CA private key 以 envelope encryption 存储，master key 来自 Docker secret/file；
- CA rotation 必须支持 trust bundle overlap，V1.5 实现自动化；V1 必须有人工 runbook。

## 6. Agent 授权

Agent mTLS 身份只允许：

- 建立属于自己的 stream；
- 读取属于自己的 DesiredState；
- 回报自己的 heartbeat、inventory、operation progress/result；
- 申请自己的证书轮换；
- 获取绑定给自己的 Repo runtime credential revision。

禁止：

- 指定任意 `agent_id`/`host_id`；
- 查询其他 Host/Agent；
- 创建中心 maintenance job；
- 读取 rclone credential；
- 修改 Plan desired state；
- 获取 Web user session。

所有 Agent API 的 subject 必须从验证后的证书导出，忽略 payload 中自报身份。

## 7. Repository 凭据

每个默认 per-Host Repository 具有：

1. 随机 Restic repository password；
2. 独立 gateway username；
3. 随机 gateway password；
4. 不可变 repo UUID/path；
5. 指向中心 CA 的 gateway TLS trust。

Agent 持有 1–5 中完成 backup 所需的值，但不持有 rclone config。凭据在 Agent 上以 root/service-user 可读的 `0600` 文件保存。没有 TPM/keyring 时，V1 不承诺抵御 Host root 窃取。

Gateway 必须验证 username 与路径前缀匹配。只验证“密码正确”但允许任意路径是严重漏洞。

### 7.1 轮换

- gateway password 支持旧/新 overlap，默认 15 分钟；
- Server 下发 credential revision，Agent 持久化并确认后才撤销旧 secret；
- Repo password rotation 通过 Restic key 管理完成，但旧 key 删除属于 maintenance 操作；
- V1 提供 gateway secret rotation；Repo password rotation 可以为 V1.1，但 Schema 必须支持多个 key revision。

## 8. 中心 Secret 管理

数据库中的 secret 使用 envelope encryption：

- 每个 secret 随机 256-bit DEK；
- payload 使用 AES-256-GCM，随机 96-bit nonce；
- DEK 由 master KEK 加密；
- AAD 至少包含 tenant/static namespace、secret type、record ID 与 key version；
- 数据库存储 ciphertext、nonce、wrapped_dek、kek_version；
- master KEK 不存数据库，生产从 `/run/secrets/restfleet_master_key` 读取；
- 使用环境变量只允许开发模式，并输出明确 warning；
- 解密结果使用最短生命周期，避免复制到通用结构体与日志上下文。

`rclone obscure` 不是 secret encryption，不得作为数据库或磁盘保护方案。

### 8.1 rclone materialization

- Credential Manager 在受限 tmpfs 目录生成 mode `0600` config；
- rclone 子进程使用显式 `--config`，不得搜索用户默认路径；
- rclone 更新 OAuth token 后，watcher 读取变化并以 revision CAS 加密回写；
- 成功持久化后更新 `last_refreshed_at`；
- 进程停止后删除 materialized file；
- crash recovery 清理 stale temp directories；
- stdout/stderr 必须经过 token、URL credential 与 provider error redaction。

## 9. Web 安全

- 首个管理员只可通过一次性 bootstrap secret 创建；使用后删除/作废；
- 密码使用 Argon2id，参数存储并支持升级；
- Session 使用随机 opaque token，数据库存 hash；
- Cookie：`HttpOnly; Secure; SameSite=Lax`，明确 Path；
- 状态改变请求使用 CSRF token；
- 登录、下载、恢复、凭据、维护 API 限速；
- CSP 默认 `default-src 'self'`，禁止不必要的 inline script；
- UI 永不回显明文 secret；仅 creation/rotation response 显示一次；
- 下载响应使用安全 Content-Disposition，禁止 snapshot filename 注入 header；
- API 使用明确 CORS allowlist，默认不允许跨域。

## 10. Restore 安全

- 单文件下载在中心执行 `restic dump`，记录 user、snapshot、path、bytes 与结果；
- snapshot ID 必须是完整 64 位小写 hex，不接受模糊 prefix 执行危险动作；
- snapshot path 作为独立 argv；禁止 shell；禁止 NUL；规范化为 snapshot-root-relative；
- Agent restore 默认目标为受控 staging root：`/var/lib/restfleet/restores/<job-id>`；
- 用户提供的子路径必须保持在 staging root 内；
- symlink traversal 必须使用安全打开/目录策略防御；
- V1 禁止原地 restore、`--delete` 和任意绝对目标；
- Docker Agent 只能写入明确挂载的 restore volume；
- Restore Job 需要 UI 二次确认和 reason；
- 取消恢复必须终止 process group，并留下 PARTIAL/CANCELED 状态及 staging 路径。

## 11. Hooks 安全

V1 不允许 Control Plane 下发任意 shell 文本。Agent 本地配置可声明：

```yaml
hooks:
  postgres-dump:
    command: /usr/local/libexec/restfleet/postgres-dump
    timeout: 10m
```

Plan 只能引用 `postgres-dump`。Agent 执行固定 absolute path，不经 shell，不接受 Server 传来的任意 arguments。Hook stdout/stderr 受限额和 redaction；超时终止整个 process group。

## 12. 维护安全

- Retention 必须以 `managed:true` 与 `plan:<uuid>` tag 限定；
- `forget` 必须先 `--dry-run --json` 生成 preview；
- 执行请求引用 preview hash，preview 默认 15 分钟过期；
- preview 与执行间 Snapshot 集合变化时拒绝执行并要求重新 preview；
- V1 禁止使用 `--group-by ''`；
- V1 禁止 `--unsafe-allow-remove-all`；
- 删除策略必须确保每个 Plan 至少保留一个 Snapshot，除非未来增加独立 break-glass 流程；
- `prune` 不与 backup 并发；
- `unlock` 先列出锁并显示年龄，默认只移除 stale locks；
- Repository migration、repair 和 key deletion 不在 V1 UI 中暴露。

## 13. 审计

必须记录：

- 登录成功/失败、登出、session revoke；
- Enrollment Token 创建/使用/撤销；
- Agent enroll、rotate、disable、revoke；
- Repository、credential、Template、Plan 的创建和变更；
- Backup Now、Retry、Cancel；
- Snapshot browse 的普通目录读取可采样，但 Download 必须逐次记录；
- Restore preview/confirm/result；
- check/forget/prune/unlock preview/execute/result；
- Notification credential test；
- 所有授权拒绝与危险配置拒绝。

Audit payload 禁止包含 secret、完整 Authorization header、Cookie、带凭据 URL 和任意文件内容。敏感字段只记录 `changed: true` 与 secret version。

## 14. 安全验收最低线

发布 V1 前必须证明：

- 使用 Agent credential 对其他 Agent 路径返回拒绝；
- 使用 Agent credential 对 DELETE/overwrite 返回拒绝；
- 已撤销证书无法建连；
- 重放 Enrollment Token 无法生成第二个 Agent；
- 中心断线时 Agent 仍使用已确认计划执行；
- 日志与 API 录制中不存在测试 secret；
- `../`、symlink、NUL 与 shell metacharacters 不能突破 download/restore 路径；
- maintenance 与 backup 竞态被锁阻止；
- 浏览器无法读取 HttpOnly session；
- PostgreSQL dump 不包含任何明文 secret。

## 15. 依据

- [rest-server append-only 与 private repos](https://github.com/restic/rest-server#usage)
- [rclone serve restic 的 append-only、private repos、TLS 与认证](https://rclone.org/commands/rclone_serve_restic/)
- [Restic Repository 密码与自动化方式](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html)
- [rclone obscure 的非安全性质](https://rclone.org/commands/rclone_obscure/)
- [gRPC Authentication](https://grpc.io/docs/guides/auth/)

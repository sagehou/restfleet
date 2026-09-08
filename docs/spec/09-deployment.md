# 09 — 部署与发布

## 1. 支持矩阵

### 中心

- Linux host；
- Docker Engine + Docker Compose v2；
- `linux/amd64`、`linux/arm64` images；
- PostgreSQL 16+；
- 能访问所选后端及其官方 OAuth 服务（OneDrive / Google Drive），或通过 HTTPS 访问公网 WebDAV；Agent 只连接中心；
- 对外一个 HTTPS endpoint，可按 path/port 暴露 Web、gRPC、Repository Gateway。

### Agent

- Linux `amd64` / `arm64`；
- Native systemd（推荐）；
- Docker/Compose（可选）；
- 可执行 Restic；
- outbound HTTPS/gRPC 到中心；
- 无需 rclone、云存储凭据或入站端口。

## 2. 中心服务

V1 Compose 逻辑服务：

```text
reverse-proxy        optional/existing
restfleet-server     API/UI/gRPC/workers + central restic/rclone adapter
restfleet-gateway    wrapper + rclone serve restic --append-only
postgres
```

`restfleet-gateway` 可由同一 Go module 的独立 command 构建。它不暴露 rclone RC，只监听 Restic REST protocol。

## 3. 网络

建议网络：

```text
edge
  reverse-proxy
  restfleet-server (HTTP/gRPC)
  restfleet-gateway (REST)

control-internal
  restfleet-server
  postgres

gateway-runtime
  restfleet-server
  restfleet-gateway
```

- PostgreSQL 不发布 host port；
- maintenance/admin listener 不加入 edge；
- Gateway 与 Server 可通过不同 path/hostname 路由；
- gRPC proxy 必须支持 HTTP/2、长连接和合理 keepalive；
- Repository transfer timeout 默认至少 1h，按部署调整；
- 若使用同一域名，建议 `/api`、`/agent`、`/restic` 明确分流。

WebDAV 的运行时连接由受限 tmpfs 中的私有 Unix socket 转发至已校验公网 IP，MUST 不发布该 socket 或新增代理端口。默认系统 CA 验证保持启用，内网/NAS 与自签名 WebDAV 本批不开放；未验证连接不得显示为已完成备份能力。

## 4. 持久化目录

```text
/var/lib/restfleet/
├── postgres/              database volume
├── server/                non-secret state/cache
├── restic-cache/          central repository metadata cache
└── exports/               TTL-bound diagnostic/audit exports

/run/restfleet/            tmpfs/runtime only
├── gateway/rclone.conf    materialized mode 0600
├── gateway/htpasswd       materialized verifier file
├── repo-passwords/        short-lived worker files
└── sockets/
```

`/run/restfleet` 必须位于 tmpfs，重启后由 Server 从加密 DB materialize。Server 与 Gateway 使用固定 UID/GID（建议 10001），权限最小化。

## 5. Docker Secrets

至少：

```text
restfleet_master_key
postgres_password
bootstrap_secret (first start only)
tls private key or ACME-managed mount
```

规则：

- 生产使用 Compose secrets 或只读 mode 0400 secret file；
- master key 32 random bytes，以明确编码格式存放；
- bootstrap 完成后移除 bootstrap secret 并重启；
- 不把 secret 写进 `.env`、compose YAML、image layer 或 logs；
- `.env.example` 只包含非敏感示例和 `_FILE` 参数。

## 6. TLS 与反向代理

支持公开 CA 与私有 CA：

- Web/gRPC 与 Repository Gateway 可以使用同一 Server cert；
- Agent 必须有正确 server_name/SAN；
- 私有 CA bundle 在 Enrollment 中固定下发，后续版本化轮换；
- Agent Restic 使用 CA file 校验 Gateway；
- 生产不得设置 `--insecure-tls`；
- 最低 TLS 1.2，推荐 1.3；
- Basic auth 只允许在 TLS 内使用。

M2 Agent enrollment/gRPC 使用以下分组配置；只要设置其中一项，就 MUST 设置全部：

```text
RESTFLEET_MASTER_KEY_FILE            # 标准 Base64 编码的 32 bytes
RESTFLEET_PUBLIC_URL                 # Agent 可访问的 https:// URL
RESTFLEET_GRPC_ADDRESS               # listener，默认 :8443
RESTFLEET_GRPC_ENDPOINT              # Agent 可访问的 host:port
RESTFLEET_GRPC_SERVER_NAME           # 必须匹配 Server certificate SAN
RESTFLEET_GRPC_TLS_CERT_FILE
RESTFLEET_GRPC_TLS_KEY_FILE
RESTFLEET_SERVER_CA_BUNDLE_FILE      # Agent 用于验证 Server 的 PEM bundle
```

Server 的内部 Agent CA certificate 存在 PostgreSQL，CA private key 使用随机 DEK 加密，DEK 再由 `RESTFLEET_MASTER_KEY_FILE` 中的 key 包裹。master key 文件 MUST 是 mode 0400/只读 secret，数据库与 master key MUST 分开备份。

Traefik/Caddy/Nginx 示例应覆盖：HTTP/2、WebSocket/SSE（若使用）、gRPC、上传/下载 streaming、超时、真实客户端 IP 信任链。

### 6.1 Agent CA 人工轮换 runbook

V1 不自动轮换 Agent CA。计划轮换 MUST 作为维护变更执行：

1. 先验证 master key 与数据库备份可恢复，并冻结新 enrollment；
2. 发布包含旧、新 CA 的临时 trust bundle，保留至少一个完整 Agent certificate overlap window；
3. 使用受审计的离线维护工具生成新 CA envelope 并逐 Agent 换发证书；CA private key 不得导出为持久明文；
4. 确认所有 ACTIVE Agent 已用新链重连后，撤销旧证书并移除旧 CA；
5. 记录操作人、受影响 Agent、开始/结束时间和验证结果。

在专用维护工具交付前，MUST NOT 直接修改 `server_pki`/ `secrets` 表完成轮换；紧急 key loss 应恢复已验证备份或重新 enrollment，而不是绕过证书校验。

## 7. Gateway Runtime

启动顺序：

1. PostgreSQL ready；
2. Server 完成 migration/secret store 初始化；
3. Server materialize rclone config 与 htpasswd 到 tmpfs；
4. Gateway 验证 config 存在、权限正确、remote 可解析；
5. 启动按 Repository 隔离的 `rclone serve restic --append-only --cache-objects=false` 私有 Unix socket；路径隔离由固定 backend root 与 Gateway 安全层共同实现，原始 socket MUST NOT 公开；
6. Gateway TLS/身份/路径/锁归属安全层及其 supervisor readiness 成功；
7. Edge 开始路由 Data Plane。

rclone OAuth token 更新：

- Gateway 可写 materialized config；
- Server watcher 检测安全原子更新，解析后立刻加密回 DB；
- DB 使用 secret revision CAS；
- persist 成功后更新 credential status；
- watcher/gateway crash 触发 DEGRADED 告警；
- shutdown 前尝试 final sync，但正确性不能只依赖 graceful shutdown。

### 7.1 中心凭据测试 worker

启用完整中心配置时，Server MUST 同时启动持久化 credential worker。RESTFLEET_CREDENTIAL_RUNTIME_DIR 默认 /run/restfleet/credentials；MUST 是预建的 0700 tmpfs，属于服务 UID。Compose 已挂载独立目录。RESTFLEET_RCLONE_BINARY 默认 /usr/local/bin/rclone，使用中心镜像固定版本。配置不安全时 MUST 启动失败，不降级为普通磁盘文件。

测试通过 REST POST 入队，worker 经 DB 租约执行只读 rclone lsjson --stat。Server 停止时取消 worker，等待子进程清理后再关闭数据库；未完成任务由下一次启动重领。实际云端 token refresh 的人工验收仍须使用安全环境，MUST NOT 将真实配置作为 CI artifact 或测试日志上传。

### 7.2 中心仓库初始化适配器

中心 MUST 使用固定 Restic 0.19.1 与 rclone 1.75.1，经私有 Unix socket 执行初始化与只读验证；MUST 沿用 Credential Runtime 的 tmpfs、token watcher/CAS 和清理规则。Restic 与 rclone MUST 分别按进程组取消，后端异常退出 MUST 取消正在运行的 Restic。socket/password/config/cache 均位于同一受限临时目录，不新增 TCP 管理端口。

初始化适配器已接入 Repository API/持久化 jobs，初始 Agent 仓库凭据交付/ACK 见 §7.5；Gateway 准入仍待接线，MUST NOT 因离线 smoke test 或初始化 Operation 成功就把 Repository 标成 READY。Server/Agent/Gateway 镜像均包含固定 Restic；rclone 仍只存在于中心 Server/Gateway 镜像。

### 7.3 Gateway 安全层交付边界

已实现单次授权备份安全层及进程内 supervisor/router 与固定二进制离线验收；尚未接入 `restfleet-gateway` command 的公网启动配置、持久化准入/审计和备份会话能力下发；MUST NOT 将 command 骨架或此测试环境部署为可用公网 Gateway。调用方 MUST 维持可信 per-Repository backup lease、独占 backend 生命周期及中心维护排斥；会话替换前 MUST 取消并等待旧请求退出。会话凭据 MUST 通过受保护文件或子进程环境交付 Restic，不能放入 argv/含凭据 URL。

当前安全层最多并发 8 请求、串行上传，上传缓冲最多 32 MiB（临时锁 64 KiB），每请求最多 1h；校验整个对象哈希后才提交，MUST NOT 配置超过上限的 Restic pack。supervisor 按调用方配置限制 1–32 个活动会话，且同 Host/Repository/Gateway identity/Operation/StorageCredential 同时最多一个；部署接线仍 MUST 增加监听连接/未认证请求速率限制及独立 TLS readiness；控制 API/数据库离线时的调度和准入协调仍按 §7 与架构可用性规则验收。会话丢失留下的锁 MUST 由中心经审计维护清理，禁止启动时批量删锁。

### 7.4 Gateway supervisor 生命周期

`WithBackup` MUST 只由可信中心调用方使用，不是 HTTP 创建会话 API。调用前 MUST 验证持久化绑定、记录秘密访问审计、取得并续租 backup lease，排斥同仓库维护和同凭据的测试/初始化/刷新写入者；进程内互斥 MUST NOT 代替数据库 fencing。一个 runtime MUST 只对应一个 supervisor，MUST NOT 新建多个 supervisor 或多个 runtime 目录绕过限制。

supervisor MUST 复用 Credential Runtime 的配置校验、tmpfs、WebDAV 固定出站、运行中 token watcher/CAS 与退出清理。Gateway 专用入口 MUST 接受明确且不超过 24h 的授权截止时间；原有连接测试 1min、普通中心命令 5min 上限保持不变。不得无期限 materialize，也不得自动重新授权或重启失败会话。

启动 MUST 使用固定 argv、固定环境、append-only 与 cache-objects=false，并仅绑定 UUID-scoped 私有 Unix socket；不继承 RCLONE/proxy/LISTEN_* 变量或 socket activation。只有受限 socket 验证通过且只读 HEAD config 成功，才 MAY 安装路由和借出会话能力；此探测只证明 backend 可访问，MUST NOT 表示云凭据健康、完整仓库校验或 Agent ACK。不存在仓库时 MUST NOT 自动 init。

正常结束、超时、取消、后端退出或 refresh/CAS 失败 MUST 撤销能力、移除路由、等待在途请求结束、终止并回收子进程组，再删除 materialized 目录；`WithBackup` 返回前 MUST 完成清理，之后才允许调用方释放持久化 fencing。启动/结束审计与请求拒绝审计 MUST 只包含可信 ID 和固定分类；原始 provider/callback 错误 MUST NOT 外泄。shutdown MUST 先停外部 listener，再 Close supervisor，最后关闭 runtime；Close MUST 拒绝新会话并等待已有清理完成。

supervisor 批次不提供公网部署就绪声明、备份会话能力下发/ACK 或离线持久化准入。独立的仓库初始凭据下发/ACK 见 §7.5，不能替代会话准入。刷新无法安全回写时仍 fail closed，不能据此宣称控制面离线 12h 验收已完成；后续接线 MUST 继续满足 AGT-005，不能删掉该验收来绕过协调问题。

### 7.5 Agent 仓库凭据交付开关

Server 可选配置 `RESTFLEET_GATEWAY_PUBLIC_URL=https://gateway.example.com[:port]`（仅 origin，无尾斜杠、路径、userinfo、query/fragment），默认留空不下发。设置后 MUST 同时具备完整 enrollment 配置；Gateway TLS 使用同一 `RESTFLEET_SERVER_CA_BUNDLE_FILE` 信任集合，MUST 含有效 CA。origin/CA 变化会生成新交付 revision；不得将设置 origin 误认为已启动 Gateway listener。

完成 schema 10 migration、Server/Agent 升级且仓库已初始化后，具备 capability 的 Agent 在连接或心跳时接收本 Host 的仓库凭据并保存 ACK。Web 可查看“尚未下发 / 等待 Agent 确认 / 已确认保存”。此配置不启动 supervisor 会话、不授予 backup lease，也不完成 Gateway rotation/离线协调；Gateway command 仍不是生产可用公网服务。

## 8. Native Agent 安装

目标目录：

```text
/usr/local/bin/restfleet-agent
/usr/local/bin/restic or distro-managed restic
/etc/restfleet/agent.yaml
/var/lib/restfleet-agent/identity/
/var/lib/restfleet-agent/state.db
/var/cache/restfleet-agent/restic/
/var/lib/restfleet/restores/
```

权限：config `0640`（不含可复用 secret 或受 service user 约束）；identity/credential `0600`；state dir `0700`。

systemd 逻辑：

```ini
[Service]
ExecStart=/usr/local/bin/restfleet-agent run --config /etc/restfleet/agent.yaml
Restart=always
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=/var/lib/restfleet-agent /var/cache/restfleet-agent /var/lib/restfleet/restores
```

Agent 常需读取 root-only backup paths，V1 可作为 root 运行，但必须减少 capabilities、禁止网络监听和任意 shell。`ProtectHome` 等 hardening 不能阻止用户明确要备份的只读路径；安装器按计划需求给出提示，不能暗中放宽为读写。

## 9. Docker Agent

示意：

```yaml
services:
  agent:
    image: ghcr.io/sagehou/restfleet-agent:<version>
    restart: unless-stopped
    environment:
      RESTFLEET_CONFIG: /etc/restfleet/agent.yaml
    volumes:
      - agent-state:/var/lib/restfleet-agent
      - agent-cache:/var/cache/restfleet-agent
      - /data:/backup/data:ro
      - /etc:/backup/etc:ro
      - /var/lib/restfleet/restores:/restores:rw
    read_only: true
    tmpfs:
      - /tmp
    security_opt:
      - no-new-privileges:true
```

原则：

- 所有 backup source 显式 `:ro`；
- restore volume 单独 `:rw`；
- identity state 使用 persistent volume；
- 不挂 Docker socket；
- 不使用 privileged；
- path 在 Template 中使用容器内路径，UI 明确显示 deployment mode；
- host networking 非必需。

## 10. Agent Enrollment 命令

安装命令可携带一次性 token，但必须提示 shell history/process exposure。推荐：

```text
RESTFLEET_ENROLLMENT_TOKEN_FILE=/run/secrets/... restfleet-agent enroll ...
```

交互式 stdin 也可。若 Console 提供复制命令，token 仅一次显示且 TTL 短；Agent 完成 enrollment 后清理环境/临时文件。

## 11. 多架构构建

- Go binaries：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64|arm64`；
- Image manifest list 同时包含 amd64/arm64；
- 基础镜像按 digest pin；
- Agent image 包含按架构锁定并校验 SHA-256 的 Restic；
- Server/Gateway image 包含锁定 Restic/rclone；
- 生成 SBOM 与 provenance；
- release artifacts 包含 checksums 和可验证签名；
- CI 在两架构至少做启动/版本/协议 smoke test，arm64 可使用原生 runner 或受控 emulation。

## 12. 版本与升级

版本：SemVer。Server、Agent、protocol、DB schema 独立记录。

升级顺序：

1. 备份 PostgreSQL、master key metadata 与配置；
2. 阅读 release/migration note；
3. pull digest-pinned images；
4. 运行 one-shot migration；
5. 更新 Server/Gateway；
6. 验证 health、credential、gateway、sample snapshot query；
7. 分批更新 Agent；
8. 验证 N/N-1 compatibility 和 scheduled backup。

V1 不提供 Agent 自动升级按钮。管理员使用包管理/Ansible/容器编排升级。

## 13. 回滚

- App rollback 只能回到与当前 schema compatible 的 N-1；
- DB migration 默认不做自动 down；失败用 forward-fix；
- Gateway rclone config 的前一 encrypted revision 可恢复；
- Agent binary rollback 保留 identity/state，并验证 protocol support；
- 不允许通过回滚跳过已记录的 security revocation。

## 14. 备份 RestFleet 自身

必须单独备份：

- PostgreSQL；
- master key（离线/不同位置）；
- Server CA/private key ciphertext 与 trust bundle；
- rclone credential secret revision；
- compose/config/version manifests。

必须演练“新中心节点 + DB backup + master key”恢复。仅有 DB 没有 master key 等同无法恢复 Secret。

## 15. 生产 Readiness

```text
/health/live    process alive, no dependency disclosure
/health/ready   DB/migration/critical worker readiness
/metrics        authenticated/internal only
```

Gateway 独立 readiness 做只读/安全检查，不对公网暴露详细 provider error。Reverse proxy 只有在 ready 后转发。

## 16. 部署验收

- 外网不能连接 PostgreSQL/admin listener/rclone RC；
- Gateway append-only 删除测试失败；
- Agent 端无监听管理端口；
- Native/Docker 的 amd64/arm64 smoke test 通过；
- 私有 CA 验证成功，错误 CA 失败；
- 中心重启后 materialized secrets 被重建且旧 tmpfs 不持久；
- OAuth token refresh 被加密持久化；
- 中心进程重启不丢 durable operations/jobs；
- 日志与 `docker inspect` 不出现 secret。

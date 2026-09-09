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

已实现单次授权备份安全层、进程内 supervisor/router、有界 TLS transport 与固定二进制离线验收；尚未接入 `restfleet-gateway` command 的公网启动配置、持久化准入/审计和备份会话能力下发；MUST NOT 将 command 骨架或此测试环境部署为可用公网 Gateway。调用方 MUST 维持可信 per-Repository backup lease、独占 backend 生命周期及中心维护排斥；会话替换前 MUST 取消并等待旧请求退出。会话凭据 MUST 通过受保护文件或子进程环境交付 Restic，不能放入 argv/含凭据 URL。

当前安全层最多并发 8 请求、串行上传，上传缓冲最多 32 MiB（临时锁 64 KiB），每请求最多 1h；校验整个对象哈希后才提交，MUST NOT 配置超过上限的 Restic pack。supervisor 按调用方配置限制 1–32 个活动会话，且同 Host/Repository/Gateway identity/Operation/StorageCredential 同时最多一个；TLS transport 的连接/认证前限流见 §7.6，部署接线仍 MUST 完成独立 readiness 与持久化准入；控制 API/数据库离线时的调度和准入协调仍按 §7 与架构可用性规则验收。会话丢失留下的锁 MUST 由中心经审计维护清理，禁止启动时批量删锁。

### 7.4 Gateway supervisor 生命周期

`WithBackup` MUST 只由可信中心调用方使用，不是 HTTP 创建会话 API。调用前 MUST 验证持久化绑定、记录秘密访问审计、取得并续租 backup lease，排斥同仓库维护和同凭据的测试/初始化/刷新写入者；进程内互斥 MUST NOT 代替数据库 fencing。一个 runtime MUST 只对应一个 supervisor，MUST NOT 新建多个 supervisor 或多个 runtime 目录绕过限制。

supervisor MUST 复用 Credential Runtime 的配置校验、tmpfs、WebDAV 固定出站、运行中 token watcher/CAS 与退出清理。Gateway 专用入口 MUST 接受明确且不超过 24h 的授权截止时间；原有连接测试 1min、普通中心命令 5min 上限保持不变。不得无期限 materialize，也不得自动重新授权或重启失败会话。

启动 MUST 使用固定 argv、固定环境、append-only 与 cache-objects=false，并仅绑定 UUID-scoped 私有 Unix socket；不继承 RCLONE/proxy/LISTEN_* 变量或 socket activation。只有受限 socket 验证通过且只读 HEAD config 成功，才 MAY 安装路由和借出会话能力；此探测只证明 backend 可访问，MUST NOT 表示云凭据健康、完整仓库校验或 Agent ACK。不存在仓库时 MUST NOT 自动 init。

正常结束、超时、取消、后端退出或 refresh/CAS 失败 MUST 撤销能力、移除路由、等待在途请求结束、终止并回收子进程组，再删除 materialized 目录；`WithBackup` 返回前 MUST 完成清理，之后才允许调用方释放持久化 fencing。启动/结束审计与请求拒绝审计 MUST 只包含可信 ID 和固定分类；原始 provider/callback 错误 MUST NOT 外泄。shutdown MUST 先停外部 listener，再 Close supervisor，最后关闭 runtime；Close MUST 拒绝新会话并等待已有清理完成。

supervisor 批次不提供公网部署就绪声明、备份会话能力下发/ACK 或离线持久化准入。独立的仓库初始凭据下发/ACK 见 §7.5，不能替代会话准入。刷新无法安全回写时仍 fail closed，不能据此宣称控制面离线 12h 验收已完成；后续接线 MUST 继续满足 AGT-005，不能删掉该验收来绕过协调问题。

### 7.5 Agent 仓库凭据交付开关

Server 可选配置 `RESTFLEET_GATEWAY_PUBLIC_URL=https://gateway.example.com[:port]`（仅 origin，无尾斜杠、路径、userinfo、query/fragment），默认留空不下发。设置后 MUST 同时具备完整 enrollment 配置；Gateway TLS 使用同一 `RESTFLEET_SERVER_CA_BUNDLE_FILE` 信任集合，MUST 含有效 CA。origin/CA 变化会生成新交付 revision；不得将设置 origin 误认为已启动 Gateway listener。

完成 schema 10 migration、Server/Agent 升级且仓库已初始化后，具备 capability 的 Agent 在连接或心跳时接收本 Host 的仓库凭据并保存 ACK。Web 可查看“尚未下发 / 等待 Agent 确认 / 已确认保存”。此配置不启动 supervisor 会话、不授予 backup lease，也不完成 Gateway rotation/离线协调；Gateway command 仍不是生产可用公网服务。

### 7.6 Gateway TLS transport

`NewPublicServer` MUST 在绑定公网 socket 前校验证书/私钥匹配和证书当前有效期；可信调用方 MUST 从受保护文件加载证书，Agent MUST 继续校验配置的 CA 和 endpoint 名称。transport 只接受 TLS 1.2+ / HTTP/1.1；MUST NOT 回退明文或通过 Control API/gRPC 代理。HTTP/2 暂不启用，后续启用前 MUST 增加显式 stream 上限和固定 Restic 验收；Agent 控制 gRPC 的 HTTP/2 不受影响。

默认最多接受 128 个连接，未完成 TLS 握手和 idle keep-alive 均占容量；超额连接留在操作系统 backlog，不创建额外请求处理 goroutine。TLS/请求头最多 5s，header limit 16 KiB（遵循 Go net/http 的解析缓冲余量），idle 60s；基础读写 30s，仅经认证且进入安全层的对象传输可延长到 1h。处理后未读 body 的清理最多 5s。连接限制不代替边缘网络防护。

认证前使用全局 1s 固定窗口，最多接纳 256 个请求，不按客户端输入创建限流 key；窗口边界可能形成双倍短时突发。MUST NOT 信任 Forwarded/X-Forwarded-* 作为来源或身份。超额返回固定 429、Retry-After: 1 并关闭连接；每窗口首次超额尝试记录无身份、无原始输入的 `denied/rate_limited` 审计，审计失败该请求返回 503，其他超额请求仍拒绝且不重复触发审计。已进入认证/授权的请求保留逐请求拒绝审计；TLS/HTTP 解析失败不记录原始错误。共享窗口可能影响其他 Host 的吞吐，不承诺遭受洪泛时的公平性。

transport MUST 直接交给 supervisor，保留原始路径，不挂载健康、管理或 metrics 路由。Serve 单次使用，取消或 listener 失败后 MUST 关闭入口、取消并等待在途请求/审计与 supervisor 后端清理，返回后调用方才 MAY Close runtime。固定 Restic/rclone 的 TLS 备份、锁清理和读回验收 MUST 经过此 transport。

此批只完成可复用 transport；command 的受保护配置加载、独立 readiness、持久化 admission/审计接线和离线协调仍未完成，MUST NOT 开放公网部署或将 Repository 标成 READY。

### 7.7 备份占用交付边界

schema 11 的持久化备份占用原语已接入现有中心写入口互斥，见 ADR-0016；它不会启动 Gateway，不解密云端材料、不下发会话能力，也不标记 READY。MUST 在停掉所有旧版中心 writer 后迁移/升级；旧二进制不认识新的占用 fence，不能混跑。

可信调用方 MUST 持久保存准入 ID/owner，并在使用前校验当前绑定和剩余授权时间；每次真实备份仍需独立 session/锁归属。MUST 将会话截止时间限制在准入期限内；关闭并等待全部请求、后端进程组和刷新持久化清理后才允许提交释放。准入到期、Agent 断线/撤销、凭据禁用、DB 不可用均 MUST NOT 被用作“已经清理”的证据。释放确认丢失时保留占用并重试，禁止按到期批量删除/释放记录。

尚未实现 Gateway 的 owner 恢复协调或公开申请入口，因此不能手工拼接这些原语后宣称生产可用。离线许可、审计缓冲与 OAuth refresh 持久化仍须单独满足 AGT-005；在线 Check 的 fail-closed 行为不替代最终离线方案。

### 7.8 在线 Gateway 占用协调接缝

`Supervisor.WithAdmittedBackup` 复用 §7.7 的可信中央占用接口；本批不是公网申请 API、进程 owner 恢复或最终离线许可。调用方 MUST 已持久保存 ID/owner、审计材料读取并提供相同绑定的中央配置；MUST 保持单 supervisor/runtime owner，不能将本地互斥描述为跨进程接管证明。

协调器 MUST 在本地容量/身份互斥内、材料落盘前核验当前占用的 ID、owner、配置 hash、Host、Repository、Gateway、StorageCredential、未释放状态与期限；会话截止时间 MUST 不晚于占用截止时间。运行期间每秒复查一次，单次最多 3s；失败、绑定变化或期限变化 MUST 取消会话并等待清理，不续期。撤销检测存在轮询/请求超时延迟；这不是 AGT-005 的离线方案，DB 不可用仍 fail closed。

只有运行、配置最终同步、tmpfs 清理和结束审计全部成功且授权上下文仍有效时，才自动提交释放；释放最多 3s，MUST 在 supervisor 释放容量及 Close 返回之前完成。重复/冲突调用 MUST NOT 释放另一个运行的占用。任何运行异常、撤销、到期、刷新/审计失败或不确定清理均保留占用，等待后续可信恢复；释放失败不重启备份，不按过期自行解锁。

固定 Restic/rclone 的 TLS 备份读回通过该协调接缝（测试用占用 authority）验证清理后释放；真实 PostgreSQL 原语仍由数据库集成测试覆盖。这不代表完成公网 command、加密材料读取/审计接线、崩溃恢复、会话下发、READY 或三种云端真实服务验收。

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

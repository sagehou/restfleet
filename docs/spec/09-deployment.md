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

supervisor MUST 复用 Credential Runtime 的配置校验、tmpfs、WebDAV 固定出站、运行中 token watcher/CAS 与退出清理。Gateway 单次备份会话入口 MUST 接受明确且不超过 24h 的会话截止时间；原有连接测试 1min、普通中心命令 5min 上限保持不变。不得无期限 materialize，也不得自动重新授权或重启失败会话。

启动 MUST 使用固定 argv、固定环境、append-only 与 cache-objects=false，并仅绑定 UUID-scoped 私有 Unix socket；不继承 RCLONE/proxy/LISTEN_* 变量或 socket activation。只有受限 socket 验证通过且只读 HEAD config 成功，才 MAY 安装路由和借出会话能力；此探测只证明 backend 可访问，MUST NOT 表示云凭据健康、完整仓库校验或 Agent ACK。不存在仓库时 MUST NOT 自动 init。

正常结束、超时、取消、后端退出或 refresh/CAS 失败 MUST 撤销能力、移除路由、等待在途请求结束、终止并回收子进程组，再删除 materialized 目录；`WithBackup` 返回前 MUST 完成清理，之后才允许调用方释放持久化 fencing。启动/结束审计与请求拒绝审计 MUST 只包含可信 ID 和固定分类；原始 provider/callback 错误 MUST NOT 外泄。shutdown MUST 先停外部 listener，再 Close supervisor，最后关闭 runtime；Close MUST 拒绝新会话并等待已有清理完成。

supervisor 批次不提供公网部署就绪声明、备份会话能力下发/ACK 或离线持久化准入。独立的仓库初始凭据下发/ACK 见 §7.5，不能替代会话准入。刷新无法安全回写时仍 fail closed，不能据此宣称配置窗口内的控制面离线验收已完成；后续接线 MUST 继续满足 AGT-005，不能删掉该验收来绕过协调问题。

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

### 7.9 中心占用材料借用与刷新

`ControlPlane.WithBackupMaterial` MUST 在当前占用及访问审计提交后解密云端配置，仅向可信中央 runner 借出 rclone config、remote 和占用 metadata；MUST NOT 将 master key 或 Restic password 交给 Gateway。runner MUST 接入 §7.8 的 supervisor、遵守截止时间并等待全部工作完成；本入口本身不发布路由、不释放占用。刷新回调串行化，中心 MUST 校验 token-only 差异并 envelope 加密 CAS 回写；相同配置不产生新版本，回调在借用结束后不可再使用，原始错误不外传。

此接线仍是在线中央材料服务；公网 command、数据面审计持久化、独立 Gateway 进程的受保护材料交付、owner 恢复与 AGT-005 离线协调未完成。不得将它直接暴露为 Agent API 或据此标记 READY。

### 7.10 可配置离线窗口与故障行为

用户已接受 ADR-0017 的独立 Gateway 方案，并以 [ADR-0032](../adr/0032-configurable-offline-window.md) 修订期限和交付边界。`RESTFLEET_GATEWAY_OFFLINE_WINDOW` 默认 `72h`，允许 `1m`..`720h` 的整秒时长；Compose 透传同名变量。中心普通授权请求的 `Lifetime=0` 或启动 metadata 的 `lifetime_seconds=0` 使用该配置，显式更长请求 MUST 拒绝。修改配置只影响后续中心决定，已签发权限仍按原截止时间失效。

窗口 MUST 从最后一次中心签发计算，不从检测失联、接收、重放或重连重新计算；实际截止为授权与原占用截止的较早者。因此它表示剩余授权窗口，不承诺任意失联时刻之后另有完整 72h。中心 MUST 在线核验当前身份、ACK、配置和占用后才续期；撤销/禁用在失联时最多延迟配置窗口生效。schema 16 将占用与授权请求上限扩到 30d，避免旧 24h 上限截断 72h；单次备份会话仍最多 24h。续期 MUST NOT 改原占用期限，也不释放再抢占。

这里的离线指中心控制 API/DB 不可用、Agent 仍可连接持续运行的 Gateway。Gateway 不可达、停止、重启后材料丢失或授权不足时，Agent MUST 暂停备份并报告可识别的错误，按 Plan retry policy 有界重试。MUST 保留 Agent 身份、私钥、证书和 last-known-good 配置，不要求重新 enrollment；V1 不实现 Gateway 离线冷启动恢复或额外材料恢复机制。

复用既有受保护材料通道、有界加密 Queue 和有序幂等回写；可靠保存失败、容量耗尽、时钟不可信或明确吊销 MUST 停止授权使用。中心恢复后 MUST 重新核验授权并完成可信清理、回写、来源封存与释放，再建立新运行；队列排空或占用到期不能证明旧数据面已退出，也不能复活失败 owner。生产会话入口、daemon/readiness 和完整停止协调仍待交付，现有在线适配器不能直接关闭 DB 检查来模拟成功。

新增自动测试 SHOULD 限于配置默认值/上限、签名期限和拒绝越权的关键边界，复用现有安全负向测试；期限边界 MAY 使用受控时钟，不要求真实等待 72h。rclone/provider 的真实认证、刷新、备份读回与兼容性 MUST 在部署后的 GitHub Actions 安全环境验证，不再为每种 provider 扩展重复模拟套件；未验证的后端不能声称生产验证通过。本地开发区仍只编辑源码与文档。

### 7.11 签名授权决定与本地状态（内部 v1 契约）

ADR-0018 交付 security 签名编解码与 Gateway 本地状态原语；MUST NOT 替换 §7.8 的在线核验后直接开放生产 command。没有新增公共 HTTP/gRPC 接口、DB schema、中心签发服务或持久化回写区。

wire MUST 为规范 JSON payload 后直接拼接 64-byte Ed25519 签名，总长最多 2048 bytes；签名内容为 ASCII 前缀 `restfleet:gateway-authorization:v1`、一个 NUL 字节和 payload。使用独立中心签名密钥，验证公钥 MUST 经受保护配置交付；wire 没有公钥、算法或可自选版本。payload MUST 精确匹配 `security.GatewayStatement` 的字段顺序和 Go encoding/json 紧凑编码，未知/重复字段、额外空白、替代 UUID 表示及非规范数字 MUST 拒绝，不进行宽松归一化。

字段顺序为 binding、revision、issued_at、expires_at、revoked。binding 内字段依序为 admission_id、owner、runtime_id、agent_id、host_id、repository_id、gateway_id、storage_credential_id、delivery_id、gateway_secret_ref、restic_secret_ref、configuration_hash；ID 均为规范 UUIDv7，hash 为 64 位小写 hex 的现有 Gateway origin/CA 指纹。revision 为 1..MaxInt64 的无损十进制整数；时间为 UTC Unix 整秒，issued_at 必须为正且不晚于 9999 年末。普通授权要求 issued_at < expires_at <= issued_at + 720h（编码上限；中心签发 MUST 同时受配置窗口限制，默认 72h） 且不晚于 9999 年末；明确吊销必须 revoked=true、expires_at=0。绑定只含引用，不含秘密；云端材料及版本仍须由后续受保护交付与连续占用检查保证，签名 metadata 本身不是材料校验。

Gateway MUST 从可信配置构造唯一运行绑定并复制验证公钥，不信任 wire 提供的新绑定。只有验签、精确绑定和时间检查通过的单调 revision 才能更新状态；相同 revision 仅允许完整相同内容的幂等重放，重放不重置截止时间。旧 revision、同版本改参、签发时间倒退、未来签发及初次收到已过期授权 MUST 拒绝；普通续期不得复活已知吊销的同一绑定。中心 MUST 只签发已经提交的、经过当前身份/ACK/占用核验的决定，签名工具函数不代替事务授权。

本地状态为 UNKNOWN、VALID、EXPIRED、REVOKED、CLOCK_UNSAFE，连接状态由 transport 单独维护。离线时无新决定不改变已有授权；到期仍停止使用但不是吊销，重连本身也不能续期。Status MUST 使用可信本机时钟，按签发截止时间及接收时建立的单调 deadline 双重检查；发现墙钟回拨或无效时钟后禁止自恢复。已有明确吊销即使遇到时钟异常仍保持 REVOKED。Accept 的幂等成功仅表示已接收，不代替每次使用前的 Status 与数据面其他安全检查。

此本地状态不持久化，不证明进程清理、不释放 fence，也不自动取消/恢复生产会话。调用方 MUST NOT 重建它来清除防回滚/吊销记录；Gateway 重启必须使用新的 runtime_id 并经中心恢复协调，不能离线重放旧授权。中心事务签发/续期接线见 §7.12；可信吊销分发、可靠回写及完整离线验收仍待完成，现有在线流程继续 fail closed。

### 7.12 中心事务签发与连续占用

schema 12 / ADR-0019 将授权决定追加持久化，原占用 ID/owner、释放状态和截止时间保持不变。在线续期只更新签名授权决定，实际有效期 MUST 不超过配置窗口或原占用剩余期限；窗口默认 72h，从签发起计算，不从失联起重新计时（ADR-0032）。超过原占用期限的长期运行仍需后续生命周期协调，MUST NOT 自动释放或重开占用。

`ControlPlane.DecideGatewayAuthorization` 只向可信中心运行协调器开放，无新 HTTP/gRPC 路由；调用方 MUST 控制运行实例身份，并串行化签发投递与最终清理/释放。服务使用最多 3s 的上下文，事务提交后重验请求/绑定/版本/期限，再使用独立 Ed25519 key 签名；DB 失败、取消或过期不返回签名。重放同一最新决定不延寿；旧决定被后续版本取代后不再重新签发。

`Settings.GatewaySigningKey` 默认留空禁用签发；启用时必须为一致的 64-byte Ed25519 私钥并配置有效 Gateway origin/CA。中心复制密钥，仅保留在中心服务中；密钥 MUST 经受保护配置加载，不进入日志、argv、审计、API 或 Gateway runner。命令文件装载见 §7.13；生产运行协调尚未完成，MUST NOT 把测试注入配置当作部署开关。

已知吊销阻止在线材料使用，但不自动结束远程进程或完成审计回写。释放检查覆盖所有历史未过期授权，显式吊销/全部到期只满足释放的一项前提；仍 MUST 证明请求、子进程和持久化工作退出。升级必须停旧 writer、运行迁移并启用 schema 12 的新代码，不能混跑。独立 Gateway 通道、吊销投递/恢复、回写区、生产 command/readiness 与 AGT-005 仍待验收。

### 7.13 加密待回写与本地回放（schema 13 / ADR-0020）

本批实现回写链路，尚未交付授权/云端材料或 Agent 会话能力的生产通道，在线 §7.8/7.9 MUST 保持原检查。Gateway command、可信清理恢复和 AGT-005 仍待完成。

记录 MUST 为 `security.GatewayPendingRecord` 的规范紧凑 JSON，外层为 header、ciphertext（标准 base64）后拼接 64-byte Ed25519 签名，总长最多 512 KiB。签名域为 `restfleet:gateway-pending:v1` 加 NUL；来源公钥 MUST 从中心注册查得，不能接受 wire 提供的公钥。未验签 header 只 MAY 用 admission_id/runtime_id 查询可信来源，不能用于授权或效果；验签后 MUST 比对完整绑定。同一运行实例的多个占用独立注册、排序和封存。ciphertext 使用 `nacl/box.SealAnonymous`（X25519 / XSalsa20-Poly1305），接收私钥仅在中心；解密后 MUST 验证内外 header 完全相同和规范重编码，不归一化未知/重复字段。

header 顺序 MUST 为 binding（同 §7.11）、record_id、sequence、previous_hash、authorization_revision、created_at；ID 为 UUIDv7，sequence 为 1..MaxInt64-1，authorization_revision 为正 bigint，hash 为 64 位小写 hex，首次 previous_hash 为全零。created_at 为正 UTC Unix 整秒且不超过 9999 年末。内层其余字段依序为 kind、event、expected_secret_revision、config。audit 仅携带同一白名单 event，config MUST 为 null、expected_secret_revision=0；refresh 要求 event=null、合法 expected_secret_revision 和最多 256 KiB config，只在中心解密并再次执行 token-only 验证。event 顺序为 binding（host_id、repository_id、gateway_id、session_id）、authenticated、action、reason；未绑定拒绝事件的 ID 均为 nil，不能按请求推断身份。

待回写目录 MUST 是 canonical、服务所有、0700 的持久目录；普通文件 MUST 0600、无 symlink/hardlink。一个目录只容纳一个完整 binding，独占 flock；记录原子写入前 MUST 检查记录数 1–4096 和总文件字节上限（至少 512 KiB +16 KiB、最多 64 MiB，预留 16 KiB 给 identity/确认及临时文件；不计算文件系统目录块）。fsync/rename/目录 fsync 任一步失败 MUST 停止本实例追加，禁止丢弃未确认记录。损坏、缺失前缀或不确定临时文件 MUST 保留并阻塞恢复；不得自动删掉未提交文件来启动数据面。重开只可回放，不可追加或复活授权。

确认 MUST 为 admission_id、runtime_id、sequence、record_id、wire_hash 的规范 JSON +64-byte 中心 Ed25519 签名（域 `restfleet:gateway-pending-receipt:v1` 加 NUL，最多 1024 bytes）。中心 MUST 在同事务提交 effect 和接收历史后才签名；Gateway MUST 核验精确首条记录、可靠保存确认后再删对应文件。确认丢失 MUST 重发相同 wire，不重新加密。同 sequence 改参、跳序、前 hash 不符、唯一 ID 重用及跨 binding MUST 拒绝。

回放 MUST 保留原占用并使用 credential → admission → origin 锁顺序；过期/禁用/已吊销不丢弃旧记录。refresh MUST 引用原运行实例的非吊销历史授权，发生于其签发至到期区间，并满足中心加密 revision CAS；不能以历史回写授予新访问。audit 清理观察 MAY 晚于授权到期，仍不构成进程退出证明。已注册来源 MUST 独占刷新，原同步刷新接口不得旁路。来源封存 MUST 由可信中心在数据面与追加者退出、全部回写后提交精确 tail；未封存来源阻止占用释放，封存自身也不证明清理。

回放通道默认 MUST 使用服务所有 0700 目录内新建的 0600 Unix socket，拒绝接管现有 socket；显式跨 UID 模式见 §7.16。双方核验 SO_PEERCRED 的服务 UID，来源/确认签名仍不可省略。原同 UID 测试不能据此声称 UID 隔离或生产材料交付已经完成。帧依序为 ASCII `RFGR`、big-endian uint32 版本 1、16-byte runtime UUID、big-endian uint32 长度、wire；未知版本和超长/空帧 MUST 拒绝分配/执行。四并发连接、每连接最多 5s、一次请求/确认，中心事务最多 3s；取消 MUST 关闭连接并等待 handler 退出。没有 TCP、HTTP 或 Agent 路由。身份/帧或来源/绑定拒绝 MUST 写固定的无资源 GATEWAY_PENDING_REPLAY_DENIED / REJECTED 审计，不能记录未认证 header、UID、原始错误或配置；DB/审计不可用时不接受原操作。

Server 内部回放 listener 默认禁用。`RESTFLEET_GATEWAY_SIGNING_KEY_FILE` MUST 包含 base64 32-byte Ed25519 seed，`RESTFLEET_GATEWAY_PENDING_KEY_FILE` MUST 包含 base64 32-byte X25519 私钥；文件 MUST canonical、服务所有、0400/0600、regular、非 hardlink，两种私钥都禁止直接环境变量。pending key 要求 signing key，签名要求有效 Gateway/enrollment 配置；设置 `RESTFLEET_GATEWAY_REPLAY_SOCKET` 才启用内部 listener。中心密钥 MUST 持久复用，不得在队列未排空时直接替换/丢弃旧密钥；本批不提供中心密钥 overlap。私钥 MUST 不交给 Gateway，升级前 MUST 停旧 writer、迁移至 schema 13；历史记录存在时 Down 拒绝。来源注册和封存没有公网接口，不能靠设置 listener 宣称 Gateway 可部署或 Repository READY。

### 7.14 本地签名授权会话 owner（ADR-0021）

`gateway.NewAuthorizedBackup` 是尚未接入 command 的内部数据面入口，MUST NOT 接受 Agent 选择的 origin/material，也不替代 §7.8 的在线入口。可信协调器 MUST 先完成当前占用、来源注册与材料匹配，经过受保护认证交付后才构造 owner；内部单次材料通道见 §7.15，生产协调仍未接线。constructor MUST 比对完整 binding、来源/确认公钥、有效授权和原占用上限，只从无追加历史的新 Queue 认领一次来源；恢复、排空或重新构造不得重置 owner 的 token/replay 状态。

同一 admission/runtime 的连续备份 MUST 复用唯一 owner、Authorization 和 token recorder，使用已可靠接受的最新配置和单调 expected revision；每次使用新 operation ID 与随机 session capability，仍受全局 supervisor 的容量和 Host/Repository/Gateway/Credential 冲突限制。会话审计 MUST 写向本绑定 Queue，未路由与全局限流审计仍由 supervisor 的独立全局 recorder 处理，不得猜测来源。

每次路由和每次向 backend 转发（含上传完成后的 HEAD/POST 与锁 DELETE）前 MUST 检查本地签名授权、原占用期限及 Queue 可写性；owner 从构造到关闭 MUST 维持唯一 watchdog，每 100ms 核验，包括尚未备份和两次备份之间的空闲期。授权/占用失效或 Queue 不可写 MUST 先永久拒绝新会话，再取消/join 已开始工作、关闭 token recorder、清零当前明文并冻结 Queue；MUST NOT 等待下次备份请求才清理。生产者可写性保守要求剩余一个最大 512 KiB wire 与一个记录位置，Append 仍执行真实大小校验；关闭、冻结、写入不确定或空间不足 MUST 停止。吊销、到期、时钟不安全或审计/刷新失败 MUST 取消，运行失败后 owner 永久停止启动；新授权、重连或排空不能恢复失败 owner。取消不能撤销已在外部后端提交的请求，不保证反向回滚数据。

运行失败或 Close MUST 等待会话、生命周期 watchdog 与材料清理全部完成；重复/并发 Close MUST 安全且等待同一清理结果。Queue 仅保留给可信回放。成功返回与 Close 均不是中心封存、崩溃清理证明或 fence release，MUST NOT 自动调用 ReleaseBackupAdmission。固定二进制 TLS 连续两次备份及负向测试只证明内部生命周期，不替代 production delivery、scheduler、可信恢复、READY、REP-016 或 AGT-005。

### 7.15 单次加密材料初始化（schema 14 / ADR-0022）

`ControlPlane.GatewayMaterialDelivery` 仅供可信中心协调器使用，不是公共 API。协调器 MUST 提供独立可信 binding/来源 pin，并串行化交付、授权投递及清理；来源私钥与中心验证公钥 MUST 从 Gateway 受保护本地配置取得，不信任 Agent 输入。中心事务最多 3s；交换最多 5s，先验证来源挑战，再提交当前状态核验、单次交付意图与访问审计，最后短时解密并签名加密。失败不回显底层错误或配置。

Gateway 默认监听新建的服务所有 0700 目录内 0600 Unix socket，中心主动连接；双方 MUST 核验预期 SO_PEERCRED UID，中心还 MUST 验证预注册来源签名。原正向测试为同 UID；显式跨 UID 访问见 §7.16，不能据此声称完整生产 UID 隔离。来源创建与独立 pin/source 文件装载见 §7.17；中心受保护 binding/来源元数据与显式单次协调命令见 §7.18，Gateway 生产配置、进程启动器与数据面 command 仍未接线。

帧 MUST 为 ASCII `RFGM`、big-endian uint32 版本 1、16-byte runtime UUID、big-endian uint32 长度、wire，与回放的 `RFGR` 隔离。挑战与回执最多 2048 bytes，材料 wire 最多 512 KiB，config 最多 256 KiB；空帧、错误 runtime、版本或超长帧 MUST 拒绝。

三个 wire MUST 为 Go encoding/json 规范紧凑 JSON 后拼接 64-byte Ed25519 签名。签名域分别为 `restfleet:gateway-material-challenge:v1`、`restfleet:gateway-material:v1`、`restfleet:gateway-material-receipt:v1`，各加 NUL 后接 payload。未知/重复字段、非规范数字、空白、替代 UUID 表示 MUST 拒绝，不归一化。

挑战字段顺序 MUST 为 binding（§7.11）、nonce、recipient；nonce 是 fresh 32-byte 随机值，recipient 是 Gateway 临时 X25519 公钥。二者 MUST 编码为 32 个 0..255 整数的 JSON 数组。材料外层顺序为 challenge、source、ciphertext；source 与 ciphertext 使用标准 base64。ciphertext 使用 `nacl/box.SealAnonymous`，接收私钥仅在 Gateway。中心签名验证后解密，内层顺序 MUST 为 challenge、source、pending_recipient、statement、admission_created_at、admission_expires_at、secret_revision、remote、config_hash、config；pending_recipient 是中心待回写 X25519 公钥，以 32 整数数组编码，statement/config 为标准 base64。MUST 比对内外挑战及可信 source，检查原占用 UTC Unix 整秒期限、初始 secret revision、普通有效授权和 config SHA-256（64 位小写 hex）。材料 MUST NOT 含 DB/master/signing/中心 recipient 私钥、Restic password 或 Agent capability。

`MaterialReceiver` MUST 单次使用。无效交换也消费接收者，取消关闭 socket；Close MUST 消费未开始实例或取消/join 正在执行的交换，并清零来源私钥副本和临时接收私钥。install/rollback/denied 回调 MUST NOT 调用等待自己的 Close。安装只 MAY 通过 `InstallGatewayMaterial` 构造 fresh Queue/唯一 owner，config 借用后清零；回调 MUST 返回 rollback，安装或回执发送失败时取消并 join owner，保留 Queue 供回放。

ADR-0029 将内部启动等待与交换分开：`Receive` MUST 保持包含 Accept 的等待/交换合计 5s 上限（失败审计另给最多 3s）；可信启动者 MAY 显式使用 `ReceiveWaiting`，等待参数 MUST 大于零且最多 5min，Accept 后交换仍 MUST 最多 5s。父 context 或 Close MUST 取消等待和交换；无效参数/等待超时 MUST 消费接收器、关闭 listener 并清零，不能自动重试或接管旧 owner。等待不延长授权或占用。发送/接收失败的已观察拒绝 MUST 在独立最多 3s 的 context 内审计，不继承交换/父 context 取消，退出 MUST 等待该回调和 rollback；不能回显原始错误。REP-054–055 MUST 在 Actions 验证，本入口本身不提供 daemon 或 READY。

回执字段顺序 MUST 为 challenge、wire_hash，来源签名验证精确材料 wire SHA-256。它只确认初始化，不是 Agent ACK、READY、清理或释放。中心提交单次意图后 MUST NOT 重新交付，包括精确重试、新挑战、丢失回执和无已回写记录；未知结果保留 fence，等待可信恢复。升级 MUST 停旧 writer 后迁移 schema 14，已有交付历史时 Down 拒绝；不新增公共 API、自动注册来源或授权签发。

### 7.16 独立 UID 与显式共享组通道（ADR-0023）

Server/Gateway SHOULD 使用不同的固定非 root UID，管理员 MUST 将两者加入专用非 root GID。监听方 MUST 独占拥有对应通道目录，mode 精确为 0710、GID 与配置一致；新建 socket MUST 为监听方 UID、配置 GID、mode 0660。不接受额外特殊权限、组/其他用户写目录、symlink、非规范路径或旧 socket 接管。组只授予已知路径的遍历与 socket 连接，不能列目录、修改目录或替换 socket；祖先路径 MUST 由可信管理员维护，不接受不可信写入者。

`ListenReplay`、`ServeReplay`、`Replay`、`Drain`、`MaterialReceiver.Receive`、`SendMaterial` 的可选 sharedGroup MUST 从可信配置显式传入，不从路径推断。省略/零 GID 保持 0700/0600 原模式；多个 GID 或保留值 4294967295 MUST 拒绝。组成员身份不代替 SO_PEERCRED 精确 peer UID、来源/中心签名或加密。共享组误加第三用户允许其干扰连接，MUST 仍拒绝其操作；被干扰的单次初始化保留 fence，不自动重试。

中心设置 `RESTFLEET_GATEWAY_REPLAY_PEER_UID` 与 `RESTFLEET_GATEWAY_REPLAY_GROUP` 时 MUST 同时配置两项、回放 socket 和既有有效中心密钥。值 MUST 为规范十进制 uint32，均非零且非保留值，peer UID MUST 不同于本中心 UID。不设置两项保持同 UID 私有模式。该配置仅接入中心回放 listener，不生成生产 Gateway 身份、启动进程、注册来源或授予 READY。

共享组 MUST NOT 用于中心 DB/master/signing/接收私钥、Gateway 来源私钥、明文配置和待回写 Queue；它们仍 MUST 在各自服务所有的 0700 私有目录内，以 0400/0600 文件或已有 tmpfs 规则保护。独立挂载中心 secrets，Gateway 不持有中心秘密。没有新的 wire、公共 API、DB schema 或部署服务依赖。

GitHub Actions `gateway-isolation` job 编译 race-enabled 测试二进制并运行 REP-042。root 仅是测试进程启动/权限协调器，三个实际协议进程均为不同非 root UID；Gateway 在自身进程产生来源私钥，中心只获公钥，中心私钥经匿名 stdin 管道单独交给中心。测试核验加密材料/签名回放与确认、借用明文清零、跨服务私钥读取拒绝、socket 替换拒绝和同组第三 UID 拒绝；准入/DB 事务仍由既有集成测试覆盖。开发工作区 MUST NOT 执行该 job 的编译或进程测试。`4c9285c` 的 Actions 验收已通过，后续 MUST 以当前 head 检查为准；完整生产信任配置/运行协调、续期/吊销、恢复、数据面 command/readiness、rotation/READY、真实云端和 AGT-005 仍未完成。

### 7.17 来源创建与独立公钥 pin 文件装载（ADR-0024）

Gateway 身份操作 MUST 以其服务 UID 执行，仅处理本地受保护文件，不访问 DB 或接收云端材料。管理员 MUST 预先准备服务所有 0700 私有持久目录，祖先由可信管理员维护；此目录 MUST 与共享 IPC 目录和 Queue 分开。开发工作区 MUST NOT 执行以下命令或生成真实身份，它们只用于 Actions 验收及正式部署：

```text
restfleet-gateway source-init --source-key-file /var/lib/restfleet-gateway/identity/source.seed
restfleet-gateway source-public --source-key-file /var/lib/restfleet-gateway/identity/source.seed
```

`source-init` MUST 仅创建新 Ed25519 seed，写入 0600、标准 base64 32-byte seed 加换行；排他 pending 文件、文件/目录 fsync、无覆盖 Link 发布及最终目录 fsync 全部成功后，才输出标准 base64 32-byte 公钥。旧目标、symlink、损坏/不确定文件及重复初始化 MUST 拒绝。失败 MUST 保留私有证据，不能自动删 pending/覆盖身份；并发已完成后本次仍未写入私钥的空 reservation 可回收。`source-public` 只导出已有身份公钥，不自动创建、修复、恢复授权或确认 fence 清理。

中心签名公钥 MUST 从可信管理员取得，独立配置到 Gateway 私有目录内的 0400/0600 文件，内容为标准 base64 32-byte Ed25519 **公钥**，不能复制中心 seed/私钥。`LoadGatewayTrust` 与 `NewMaterialReceiverFromFiles` MUST 核验文件与目录的 canonical 路径、精确服务 UID/权限、regular/单链接及大小，拒绝特殊权限、symlink/hardlink、pending 痕迹和同一 seed 文件充当 pin。初始化 wire 仍不能选择验证信任；receiver 保持原单次生命周期，加载私钥副本在复制后清零。生成成功/公钥导出/加载均不代表来源已注册、可信 binding 已确认或数据面可运行。

REP-043–044 MUST 在 Actions 运行。跨 UID 夹具由可信测试协调器独立安装公钥 pin，Gateway 子进程使用上述生产文件创建/装载 API；私钥不通过共享目录、argv、环境变量或中心进程。该验收不替代生产配置装载、来源注册、启动协调、续期/吊销、Agent 会话能力、恢复、数据面 command/readiness、rotation/READY、REP-016 或 AGT-005。

### 7.18 中心单次初始化协调（ADR-0025）

可信协调者 MUST 为已有已 ACK 仓库/占用及本次独立 Gateway 进程准备完整 binding，并独立取得该进程使用的来源公钥；metadata MUST NOT 来自 Agent、初始化请求或回放 wire。Gateway 本地受保护配置 MUST 独立绑定相同身份。本批只装载中心元数据，尚未实现 Gateway daemon 配置和启动器。

中心 metadata MUST 在中心服务所有 canonical 0700 私有目录内，0400/0600 regular file、单链接、无特殊权限/symlink/hardlink/pending，最多 4096 bytes。JSON 字段顺序 MUST 为：

| 字段 | 类型与约束 |
|---|---|
| version | 整数 1 |
| binding | §7.11 的完整 binding，含 fresh runtime UUIDv7 |
| source_public | 标准 base64 的 32-byte 来源 Ed25519 公钥，不能传入 seed/私钥 |
| decision_id | 本次初次授权幂等键，规范 UUIDv7 |
| lifetime_seconds | 整数 0–2592000；0 使用中心配置（默认 72h），显式值不得超过配置，实际授权不超过原 admission 到期时刻 |
| socket_path | canonical absolute Unix socket 路径，最多 107 bytes；实际 owner/权限由通道验证 |
| gateway_uid | uint32，预期 Gateway peer UID；生产非零、不同于中心 UID |
| shared_group | uint32，生产显式专用非零 GID；非生产同 UID 私有模式为 0 |

保留值 4294967295 MUST 拒绝。字段 MUST 全部显式给出；MAY 排版空白，其他编码 MUST 与 Go encoding/json 规范重新编码相同，未知/重复/遗漏/大小写替代/null/非规范 UUID 或数字 MUST 拒绝。私有模式 MUST 同 UID；共享模式 MUST 为不同非 root UID。元数据和私钥目录 MUST 与共享 IPC/Queue 分开，祖先路径由可信管理员维护。

正式部署或 Actions MAY 以中心服务 UID 显式执行，开发工作区 MUST NOT 执行：

```text
restfleet-server gateway-start --config-file /var/lib/restfleet/gateway-startup/initialization.json
```

命令 MUST 复用完整中心运行配置、当前 schema、master/signing/pending 受保护密钥文件，不生成 CA、启动 HTTP/gRPC/子进程或增加 root 权限。生产 MUST 由非 root 中心 UID 执行且使用不同非 root Gateway UID 与显式共享组。先检查数据库/schema/审计，再认证 socket/SO_PEERCRED/来源签名挑战；成功后初次签发（expected revision = 0）、核验完整 committed binding、注册来源、提交单次交付意图和访问审计后加密交付，最后验证精确回执。默认单次交换最多 5s、总 context 最多 10s；显式等待/follow 按下述启动交接补充，失败仅输出固定分类。

三个数据库阶段保留各自原子性，后续失败不回滚先前成功的授权或来源。意图提交后 MUST NOT 再交付，包括安装失败、回执丢失、重读相同 metadata 或新挑战。新 runtime MUST NOT 接管旧 admission；初始化交换 MUST NOT 自动重试、续期、撤销、封存或释放占用。默认命令不等待 socket、不启动续期 worker；显式等待/follow 仅按下述启动交接执行。协调者 MUST 将其与同 admission 其他授权投递/清理串行化。普通 Server 启动 MUST NOT 自动执行该命令，服务管理器 MUST NOT 把旧文件当作重启配方；进程新鲜性仍须可信启动者证明，文件读取不能证明它。

命令成功只确认初始化回执，不代表 Agent 会话能力、可用数据面、清理或 READY。REP-045–046 MUST 在 Actions 执行；Gateway 生产配置/daemon、多仓库数据面、续期/吊销、全局离线审计、Agent 能力、可信恢复/readiness、rotation/READY、真实云端与 AGT-005 仍待完成。

#### 中心准备命令（ADR-0025 补充，2026-10-16）

正式部署或 Actions MAY 在显式初始化前执行：

```text
restfleet-server gateway-prepare --config-file /var/lib/restfleet/gateway-startup/request.json --plan-file /var/lib/restfleet/gateway-startup/plan.json
restfleet-server gateway-start --config-file /var/lib/restfleet/gateway-startup/plan.json
```

request MUST 采用上述中心私有目录/文件及精确 JSON 策略，最多 4096 bytes。字段顺序 MUST 为 `version`（1）、`agent_id`、`runtime_id`、`source_public`、`admission_lifetime_seconds`、`lifetime_seconds`、`material_socket`、`authority_socket`、`gateway_uid`、`shared_group`；所有字段显式存在。Agent/runtime 为规范 UUIDv7；独立来源公钥为标准 base64 非全零 32 bytes，两个 socket MUST 不同并沿用既有绝对规范路径/107-byte 与 UID/组规则。admission 时长为 60..2592000 秒；授权时长为 0..2592000 秒，显式值不得超过中心配置，0 沿用配置。原 admission 截止仍限制所有实际授权。输入不接受 Host/Repository/secret 引用，由当前已确认交付和仓库权威记录推导。

准备 MUST 先校验中心配置、DB/schema/审计及已确认交付，自动生成占用/owner/初次决定 ID，先 fsync 并 no-replace 发布 plan，再调用已有预留事务。plan MUST 为中心所有 0600 文件、0700 私有目录，不包含秘密；字段顺序为 `version`（2）、`preparation`（完整原 request）、`startup`（完整版本 1 初始化 metadata），最多 8192 bytes。已有 plan MUST 精确匹配输入和当前绑定；失败/重试 MUST 保留原文件、ID 和期限，不覆盖文件、不忽略 `.pending` 痕迹。原预留事务的已释放/到期/冲突拒绝与审计保持；提交或输出丢失不能通过改用新 plan 自动接管原占用。

一个 plan 只对应一个仓库。`RESTFLEET_GATEWAY_AUTHORITY_FILE` MAY 直接指向成功初始化后的该 plan，worker 从 `authority_socket` 取得目标；原版本 1 多目标 manifest 保持兼容。准备命令不注册全局审计来源、不产生授权或交付材料、不启动 Gateway；部署者 MUST 独立准备 fresh runtime、来源、公钥 pin、本地配置和 Queue，并串行执行准备→全局来源注册→启动 Gateway 等待材料→单次初始化→启动续期 worker。启动失败继续保留占用，不能根据文件存在推断成功或清理。总命令 context 仍最多 10s，普通 Server 启动不得自动执行准备/初始化。受控故障恢复、期限前换运行、READY 和 Agent 实际执行仍需接通。

#### 显式启动交接

`gateway-start` MAY 同时指定 `--audit-config-file /absolute/path/registration.json --wait-seconds 120 --follow`，减少单仓库运行的分步交接；默认行为保持不变。audit 配置 MUST 按 §7.22 装载，runtime MUST 与初始化 binding 相同、来源公钥 MUST 不同；不匹配 MUST 在 DB 打开前拒绝。交付前 MUST 复用现有全局来源幂等注册，失败不进行材料交换。

wait MUST 为整数 0..300。正值只 MAY 等待尚不存在的材料 socket，已有非 socket、其他文件错误或取消 MUST 失败；等待结束后仍执行原路径/UID/签名认证和最多 5s 的单次交换。MUST NOT 重试交换或重发材料；总设置 context 最多 wait+10s，等待不延长任何已提交期限。

follow MUST 使用同一版本 2 plan 中的授权 socket，只有精确初始化回执成功后才启动原 worker。初始化与续期串行交接，之后复用 §7.19 的信号取消/join、精确重放、吊销和期限。follow 与非空 `RESTFLEET_GATEWAY_AUTHORITY_FILE` 同时配置 MUST 拒绝；部署者 MUST 保证其他中心进程也没有同 admission 的 worker。中心 API/RFGR 仍由普通 Server 提供，follow 不启动第二组 listener。命令失败或退出 MUST NOT 自动重跑初始化；普通 Server 的原 authority-file worker 可用于后续恢复授权投递，但不能恢复失败 Gateway owner。此入口不证明 READY，完整部署说明见 [Gateway 启动交接](../gateway-run.md)。

### 7.19 授权续期与明确吊销交付（ADR-0026）

`ServeAuthorization` MUST 拥有一个新建受保护 Unix listener，绑定单一完整 admission/runtime，逐连接串行接受 metadata-only 决定。私有默认与显式跨 UID 共享策略 MUST 与 §7.16 相同；服务 MUST 使用精确 SO_PEERCRED UID，独立来源与中心 pin；来源私钥退出时清零。帧 MUST 为 `RFGA`、big-endian uint32 版本 1、16-byte runtime UUID、big-endian uint32 长度和 body。每连接只交换挑战→statement→回执，最多 5s；挑战/回执/statement 各最多 2048 bytes（包括签名），空/超长、未知 magic/version/runtime MUST 拒绝。

挑战 MUST 按字段 binding、nonce 编码规范紧凑 JSON，nonce 为 32-byte CSPRNG 数组且非全零，随后拼接 64-byte 来源 Ed25519 签名；签名域为 `restfleet:gateway-authority-challenge:v1` 加 NUL。回执 MUST 按 challenge、wire_hash 编码，后拼接来源签名；域为 `restfleet:gateway-authority-receipt:v1` 加 NUL。完整 binding 同 §7.11；wire_hash 为精确中心 signed statement 的 64 位小写 SHA-256 hex。未知/重复/遗漏/大小写替代或非规范 JSON MUST 拒绝。statement MUST 使用既有中心签名域；不得把初始化或回写证明作为授权交付证明。

可信中心 MUST 在 Gateway 来源挑战/UID 验证后查询未封存注册来源，核验完整 binding 与独立 pin，再调用既有决定事务（expected revision ≥ 1）。普通续期必须核验当前身份、ACK、凭据、配置和原占用期限；明确吊销不得因身份/凭据禁用而无法交付。协调者 MUST 将初始化、所有决定/投递和可信清理按 admission 串行，MUST 指向已初始化的同一 owner；通道不能证明安装或进程新鲜性。

Gateway MUST 核验中心签名与完整 binding，再调用该 owner 的 `AcceptAuthorization`；普通续期不能越过原 admission 到期，失效/失败 owner 不得复活。授权状态与连接状态分离；中断不续期、不吊销、不删除已接受决定。吊销由 watchdog/每操作 guard 取消并 join 工作，清零明文并冻结 Queue；回执不等待/证明这项清理，也不确认 READY、Agent ACK、封存或释放。

回执丢失 MUST 保留中心已提交决定和原 fence。MAY 显式重放同一最新 committed 决定/幂等键，MUST 保留签发与到期时刻；不得自动签发新授权、重发初始化材料或恢复失败 owner。单次交付无自动重试。取消 MUST 关闭活跃连接并等待当前 callback 退出；callback MUST 响应 context。拒绝审计 MUST 使用固定无资源 `GATEWAY_AUTHORIZATION_DELIVERY_DENIED` / `REJECTED`，不得记录 wire、未认证身份、路径或原始错误；已观察到的拒绝 MUST 另用最多 3s 的独立 context 完成审计，调用方取消 MUST NOT 丢弃该观察，listener 退出 MUST 等待审计 callback。审计失败 MUST 停止接收通道。开发工作区 MUST NOT 执行服务或验证；REP-047–048 仅在 Actions 运行。

初始批次不新增命令或 schema；2026-10-16 接入 Server 内置持续投递 worker。`RESTFLEET_GATEWAY_AUTHORITY_FILE` 可选；不设置则保持原显式投递行为。设置时 MUST 同时配置 enrollment、签名/待回写密钥及 replay listener。生产 MUST 使用不同非 root 服务 UID 和专用组。

该文件 MUST 使用中心服务所有的 canonical regular 0400/0600 受保护文件，最多 64KiB；以下为字段顺序，实际 ID、公钥与路径 MUST 来自可信已初始化运行 metadata，不得使用 Agent 请求：

```json
{
  "version": 1,
  "targets": [{
    "binding": "与初次初始化相同的完整 GatewayAuthorizationBinding 对象",
    "source_public": "已注册来源 Ed25519 公钥的 base64",
    "socket_path": "/run/restfleet-gateway/ipc/authority.sock",
    "gateway_uid": 1002,
    "shared_group": 1003
  }]
}
```

`binding` 的实际值 MUST 为对象，以上字符串仅示意；嵌套字段顺序沿用初次初始化。targets MUST 为 1..32 个不同 admission，peer UID/组 MUST 与中心 replay 配置一致；重复/未知/遗漏字段、非规范编码 MUST 拒绝。配置只在 Server 启动读取，修改后须重启 Server；MUST NOT 据此启动或恢复 Gateway。同 admission MUST 仅有一个协调者，初始化、其他决定投递及清理 MUST 与 worker 互斥。

worker MUST 每目标串行、每 10s 检查原数据库决定与明确禁用/吊销。普通续期 MUST 在原 grant 实际有效期过半、仍未过期、原 admission 尚有更长期限且上一决定已获匹配回执时，经原 CAS 事务重新核验授权。明确撤回 MAY 覆盖未确认 grant；网络/数据库故障或到期 MUST NOT 转成吊销。所有决定 MUST 仍在来源 fresh challenge/UID/注册 pin 验证后提交。

启动/丢失回执 MUST 优先精确重送最新 committed 决定；`ReplayGatewayAuthorization` MAY 重送 revision 1，但 MUST NOT 首次签发、重新注册或交付材料。重送 MUST 保持原签发/到期，不受新配置窗口改写；过期/失败 owner MUST NOT 续发复活。内存确认版本只是投递优化，PostgreSQL 决定为持久依据。单次 RFGA 尝试仍无内部重试。服务退出 MUST 取消并 join 全部投递；未确认清理 MUST 保留 fence。新增行为未新增测试，Actions 验收仍待完成；可信清理/恢复、Repository READY、真实云端及 AGT-005 继续待完成。

### 7.20 全局离线拒绝审计（schema 15 / ADR-0027）

公网入口及本地通道的全局审计 MUST 使用独立 `GatewayAuditBinding`（origin_id、runtime_id，均 UUIDv7）和中心预注册来源，不从请求猜测 Host/Repository 或凭据。可信协调器通过 `RegisterGatewayGlobalAudit` 注册独立公钥，返回来源 metadata 和既有中心 X25519 接收**公钥**；不得把注册当作授权、材料交付或进程新鲜性证明。

复用 §7.13 的受保护 Queue、硬上限、原子写/fsync 和 RFGR 交换；全局来源使用不同私有目录，MUST NOT 与仓库来源共用目录。header 字段保持 binding、record_id、sequence、previous_hash、authorization_revision、created_at 顺序，末尾增加 audit_origin（字段 origin_id、runtime_id）。binding MUST 是 `GatewayAuthorizationBinding` 的完整零值编码，authorization_revision MUST 为 0；kind MUST 为 `global_audit`，config=null、expected_secret_revision=0。event MUST 无 binding/认证身份，仅允许 `denied/route_unavailable`、`denied/rate_limited`、`event_rejected/invalid_event` 或 `channel_denied` 的 material_rejected/authority_rejected/session_rejected 固定分类。非法内部事件 MUST 仅落盘固定 event_rejected，不保存原值，再停止生产者。

全局 wire 使用 `restfleet:gateway-global-audit:v1` 加 NUL 的来源签名，公钥加密机制/512 KiB 上限保持；回执使用 `restfleet:gateway-global-audit-receipt:v1` 加 NUL 的中心签名，字段 admission_id（nil UUID）、runtime_id、sequence、record_id、wire_hash 后增加 audit_origin_id（非 nil UUIDv7）。两个域 MUST NOT 相互授权、确认或选取错误来源；旧仓库域 MUST 省略零值新增字段，原规范编码不变。中心只用未验证 header 做正确注册表 lookup，之后 MUST 验来源签名、解密并精确核验内外 header、记录 kind、无资源固定事件及规范重新编码，不回退到其他授权域。

`GlobalAuditRecorder` MUST 唯一认领 fresh 全局队列，Record 成功表示可靠落盘。生产协调器 MUST 向 Supervisor 同时提供 Record 与 Ready；已初始化 owner 的 constructor、每操作 guard 和每 100ms 生命周期 watchdog MUST 核验该 Ready。容量不足、磁盘/确认不确定、Queue 关闭/冻结、时钟回拨或生产者失效 MUST 永久停止该运行，取消/join、清零仓库材料；排空不能复活。同一次运行关闭全局 recorder MUST 在入口和所有 append callback 退出之后冻结队列，保留待回放证据。

中心 replay listener 使用 `ReplayGatewayRecord` 区分两种严格域；不增加 TCP/HTTP/Agent 路由。全局接收 MUST 在来源锁下顺序、原子追加既有审计链及接收历史，提交后才确认；精确重复和封存后旧回执可幂等恢复，冲突/跳序/未注册/错误来源或 runtime/未来时间/早于注册/封存后新记录拒绝。中心不可达时本地持久化仍可接受观察，认证材料/授权仍由既有独立路径控制。

回放完成不证明公网入口已停止。可信协调器 MUST 在所有入口、会话及全局生产者 join 后，以精确 tail 调用 `SealGatewayGlobalAudit`；此封存不封存仓库来源、不释放任何占用、不授予 READY。schema 15 的升级顺序和历史保留见数据库规范；REP-049–051 全部在 Actions 验证。生产配置/daemon、自动续期/吊销、Agent 能力、可信恢复/readiness、rotation/READY、真实云和 AGT-005 继续待完成。

### 7.21 Gateway 显式恢复回放（ADR-0028）

崩溃后管理员 MAY 在 Gateway 服务身份下显式执行 `restfleet-gateway replay --config-file /absolute/path/replay.json`，每次处理一个既有仓库或全局 Queue。MUST 先停止旧 producer；活跃 flock、损坏或不确定记录阻塞恢复。命令只使用公钥，MUST NOT 读取来源 seed/中心私钥、申请或恢复授权、交付材料、启动子进程/公网 listener、修复损坏/不确定文件、封存来源或释放占用；可靠回执已经精确确认的遗留文件继续按既有恢复规则回收。整个尝试 context 最多 1min，每帧 5s，取消关闭连接并退出；失败不自动重试。重试 MUST 保留原 wire 和可靠回执，不重新加密或按失败丢弃未确认记录。

配置文件 MUST 位于 canonical、Gateway 服务所有的 0700 私有目录中，文件 MUST 0400/0600、regular、单链接、无 symlink/特殊权限，最多 4096 bytes；同名 `.pending` 阻塞读取。字段及顺序 MUST 为 `version`（1）、`binding`（仅仓库域，完整 §7.11 绑定）或 `audit_origin`（仅全局域，§7.20 绑定）、`source_public`、`recipient_public`、`central_pin_file`、`queue_directory`、`max_bytes`、`max_records`、`socket_path`、`server_uid`、`shared_group`。另一个域 MUST 省略，两个同时存在或两个均缺失拒绝。两公钥使用标准 base64、解码正好 32 bytes，recipient 不得全零；pin 文件为独立配置的中心 Ed25519 公钥，沿用 §7.17 的私有文件/目录策略，MUST NOT 从 wire/peer/Queue 自选 pin。允许排版空白，MUST 按规范重新编码拒绝未知/重复/缺失/null/大小写替代/替代数字等输入。

路径 MUST 绝对、规范且无控制字符，socket 最多 107 bytes；`max_bytes/max_records` MUST 与原 Queue identity 完全一致，范围沿用 §7.13。零共享组 MUST 使用本服务相同 UID 的 0700/0600 私有通道；非零组 MUST 使用不同的两个非 root UID、专用 GID 及 §7.16 的 0710/0660 通道，保留 UID/GID 值拒绝。生产 MUST 使用不同的非 root 服务 UID 与共享组。管理员 MUST 从既有可信运行元数据取得原 binding、来源/接收公钥及上限，不能从未认证请求或猜测身份构造配置。

Recover/RecoverGlobalAudit MUST 比对完整 identity，保持 producer 冻结；中央仍通过 §7.13/7.20 的注册来源、验签/解密、顺序、原子 effect/接收历史后才签精确回执。命令仅在精确确认全部记录并成功关闭 Queue 后，输出 `version`（1）、对应 `binding` 或 `audit_origin`、`sequence`、`wire_hash` 的 JSON。输出不含秘密或原始请求，错误固定为 `gateway recovery replay unavailable or inconsistent`。输出失败不会撤销已保存回执，再次显式运行 MAY 得到相同 tail。

tail MUST NOT 被当作旧进程/请求/追加者清理证明、来源封存、占用释放或 READY；上述决定继续由可信中心另行核验。REP-052–053 在 Actions 覆盖命令与跨 UID 恢复；生产 daemon、完整可信清理/恢复、自动续期/吊销、Agent 能力、readiness、rotation/READY、真实云和 AGT-005 继续待完成。

### 7.22 中心显式全局审计注册（ADR-0030）

管理员 MAY 在正式部署或 Actions 中以中心服务 UID 显式执行，开发工作区 MUST NOT 执行：

```text
restfleet-server gateway-audit-register --config-file /var/lib/restfleet/gateway-audit/registration.json
```

命令 MUST 复用既有完整中心运行配置及 master/signing/pending 文件，生产 MUST 使用非 root UID。普通 Server 启动 MUST NOT 自动注册；命令 MUST NOT 启动 listener、worker、CA、Gateway 或子进程。其总 context 最多 10s；DB/schema/审计链核验和现有注册事务共享最多 3s，不自动重试。既有中央密钥 MUST 按 §7.13 持久复用，不能在待回写记录未排空时直接换掉。

metadata MUST 在中心服务所有 canonical 0700 私有目录内，0400/0600 regular file、单链接、无特殊权限/symlink/hardlink/pending，最多 1024 bytes。字段及顺序 MUST 为 `version`（整数 1）、`audit_origin`（§7.20 的 origin_id/runtime_id，规范 UUIDv7）、`source_public`（标准 base64 的非全零 32-byte Ed25519 公钥）。三个字段 MUST 全部显式给出，MAY 排版空白；其他编码 MUST 与 Go encoding/json 规范重新编码相同，未知/重复/遗漏/大小写替代/null/非规范 UUID 或数字 MUST 拒绝。元数据 MUST 不含来源私钥、仓库/占用、socket 或云端配置。

管理员 MUST 独立从可信 Gateway 身份导出来源公钥及确认 origin/runtime，不能从 Agent 请求、初始化 peer 或回放 wire 选取来源。注册仅允许全局审计接收，不证明进程新鲜性。命令成功后只 MAY 输出 JSON，字段顺序为 `version`（1）、`audit_origin`、`source_public`、`recipient_public`（中心 X25519 接收公钥，标准 base64 32 bytes）、`created_at`（原注册 UTC RFC3339Nano 时刻）。独立中心 Ed25519 签名 pin 仍按 §7.17 配置；结果没有授权、材料、清理或 READY 语义。

精确重复 MUST 保持原注册及时刻，不重复 effect；不同来源公钥/runtime、同 runtime 新 origin 或封存后重新注册 MUST 拒绝。输出失败、取消或提交结果不确定 MUST 保留已提交来源，不删除、回滚或自动重试；管理员 MAY 显式重试精确原 metadata。失败只返回固定分类 `gateway audit registration unavailable or inconsistent`，普通命令入口的 stderr 继续仅报告固定 SERVER_EXIT；MUST NOT 回显路径、原始 DB/输出错误、密钥或配置。不能通过注册获取 backup grant、云端材料、来源封存或 fence release。

REP-056–057 MUST 在 Actions 执行，包含实际命令入口、保护文件/生产运行配置及真实 PostgreSQL；专门的 root 负向步骤只用于验证生产命令拒绝该身份。完整 Gateway daemon、自动续期/吊销、Agent 会话能力、可信恢复/readiness、rotation/READY、真实云和 AGT-005 继续待完成。

### 7.23 Gateway 单次启动生命周期（ADR-0031）

`gateway.LoadServiceConfig` / `StartService` 是可信进程内的 Linux 启动组件。MUST 使用一个 runtime/supervisor、一个独立全局来源和 1–32 个仓库 owner；完整 binding 和各来源注册 MUST 由可信中心另行确认。配置文件读取不能证明进程新鲜性，服务管理器 MUST NOT 用旧 metadata/Queue/socket 自动重启或接管。生产 MUST 使用不同非 root Server/Gateway UID 与显式专用共享组。

metadata MUST 位于 Gateway 所有 canonical 0700 私有目录内，以单链接 0400/0600 regular file 保存，最多 64 KiB；特殊权限、symlink/hardlink/pending、未知/重复/遗漏/null/大小写替代及非规范编码 MUST 拒绝。MAY 有排版空白；其他编码 MUST 与 Go encoding/json 规范重编码相同。字段及顺序 MUST 为：

`version`、`environment`、`central_pin_file`、`audit_origin`、`audit_source_file`、`recipient_public`、`audit_queue_directory`、`max_bytes`、`max_records`、`runtime_directory`、`rclone_binary`、`certificate_file`、`tls_key_file`、`client_ca_file`、`listen_address`、`max_sessions`、`server_uid`、`shared_group`、`replay_socket`、`startup_wait_seconds`、`repositories`。

ADR-0033 增加 `client_ca_file`：生产 daemon MUST 提供 Gateway 所有、受保护的 Agent CA PEM 文件，最多 64 KiB。旧内部 adapter/conformance 夹具 MAY 省略该新增字段，生产 command MUST 拒绝缺失，MUST NOT 回退 Basic。该 CA 只验证客户端，Gateway 的 server certificate/trust 仍沿用原 TLS 要求。

version MUST 为 1；environment MUST 为 production/development/test；audit_origin 为 §7.20 的完整来源，recipient_public 为标准 base64 的非全零 32-byte 中心 X25519 公钥，独立 Ed25519 pin 继续从 central_pin_file 加载。max_bytes/max_records 沿用 §7.13，各 Queue 上限相同；max_sessions 为 1–32，startup_wait_seconds 为 1–300，每次 Accept 后交换仍最多 5s。私有/共享 server_uid/shared_group 规则沿用 §7.16，保留值拒绝。listen_address MUST 为 literal IP 与规范十进制端口，不能使用 DNS；生产端口 MUST 非零。

repositories MUST 为 1–32 项，字段依序为 `binding`、`source_file`、`queue_directory`、`material_socket`、`authority_socket`。binding 沿用 §7.11 且 runtime_id MUST 与全局来源相同；各仓库的 admission_id、host_id、repository_id、gateway_id MUST 分别唯一。source_file MUST 使用独立 seed，各仓库与全局来源公钥不得复用。配置中不能携带云端明文、中心私钥、DB/master key、Restic password 或 Agent session capability。

路径 MUST canonical absolute、无控制字符且不重复，Unix socket 最多 107 bytes。Queue 与 runtime MUST 相互独立且不嵌套，与私钥/metadata/IPC 目录分离；私钥/metadata MUST 与共享 IPC 分离。Queue 目录 MUST 服务所有、0700、无特殊权限且位于持久介质，启动显式拒绝 tmpfs/ramfs；管理员仍 MUST 保证持久挂载及可信祖先。runtime 继续只允许 0700 tmpfs。TLS PEM 与私钥 MUST 经既有 protected reader 加载，单文件最多 32 KiB，先验证匹配/当前有效期；rclone binary/runtime 仍经原 runtime 校验。

启动 MUST 先加载全部独立来源/pin、runtime、fresh 全局 Queue/producer 和 TLS 材料，再建立所有材料 listener 并行执行 ReceiveWaiting。材料 MUST 匹配配置 recipient，install 只 MAY 认领 fresh 仓库 Queue/原 owner。任一初始化失败 MUST 取消/join 全部交换与 rollback，保留 Queue 和 fence。只有全部交换完成且 owner/global 可用后，才 MAY 建立 RFGA listener 并绑定公网 TLS；失败不开放残缺数据面。

运行 MUST 维持原 owner 的授权 listener 和失效监视，以及一个有界回写 worker。每轮每 Queue 最多发送一条原 wire，轮次后等待 1s；每交换最多 5s，轮次耗时包含交换。连接/回执丢失继续保留原密文，下一轮重试；收到非法/不匹配的确认或 Next/Acknowledge 失败 MUST 永久停止运行。重连不续期/吊销，不创建新材料或重加密。任一 owner/global 失效 MUST 停止全部本地授权，不因排空或高版本 grant 复活。

关闭 MUST 取消/关闭入口，等待材料/授权回调、会话、TLS 请求、后端进程组和回写 worker；最终审计 join 后清零来源/云端明文，冻结/关闭两类 Queue，最后关闭 runtime。重复/并发 Close MUST join 同一退出，MUST NOT 删除证据、封存来源、释放占用或自动重启。WithBackup 仅供可信进程内调用方使用，不能把云端材料/config/callback 暴露给 Agent 请求。

REP-058–060 MUST 仅在 Actions 执行。跨 UID job 从 protected production config 经 Service 完成材料、RFGA、TLS、两类回写及吊销清理，第三 UID 仍无 effect；固定引擎验收增加 Service 路径并保留原两种路径。Agent 长期 gateway password 与每次 session password 的生产交付协议未连接，本批不提供可部署 daemon command。自动授权分发、Agent 会话能力、可信恢复/readiness、rotation/READY、真实三后端与 AGT-005 继续待完成，离线时长与 Gateway 不可达暂停的合同已按 ADR-0032 明确。

### 7.24 复用 Agent 身份的 daemon 与会话入口（ADR-0033）

本节替代 §7 及历史批次中 htpasswd、生产会话密码分发、daemon 未接线的描述，保留原中央授权、隔离、材料/回写与清理边界。生产入口为：

```text
restfleet-gateway run --config-file /var/lib/restfleet-gateway/metadata/service.json
```

command MUST 读取 §7.23 的受保护 metadata，强制加载 Agent CA 并启用 mTLS；无证书、错误 CA 或身份不匹配 MUST 拒绝。Server TLS 与 Agent TLS 验证 MUST 保持启用。SIGINT/SIGTERM MUST 取消并 join 原 Service；服务管理器 MUST NOT 用旧配置自动重启，退出后的来源封存/占用释放仍须可信中心协调。

Gateway MAY 直接暴露 TLS 或使用四层 TLS passthrough；边缘 MUST NOT 终止 Agent mTLS 后以 header 转发身份。备份仍直接进入 Gateway，控制 API/gRPC 不转发数据。

`GET /gateway/v1/readiness` MUST 使用已配置 owner 的 Agent mTLS 身份，仅在当前 Service、全部 owner 和可靠全局审计有效时返回 200。readiness MUST NOT 代表 Repository READY、中心确认最新授权、来源封存或可释放 fence；无身份不允许经代理头探测。

会话入口及字段见 `api/openapi/gateway-v1.yaml`。Gateway MUST 只选择中心预初始化且绑定证书身份的 owner，不允许传入 config、remote、密码或 callback。请求 operation_id 仅用于运行内去重；Gateway MUST 生成新的 UUIDv7 session_id，在独立路径发布已探测成功的只读/创建数据入口。重复请求不启动新后端、不延寿；每次运行最多 4096 条记录，上限后拒绝新会话。DELETE 只结束并清理该会话，不能删除仓库、执行 unlock、写入备份成功或释放中心占用。

Agent 客户端 MUST 复用本机 TLSIdentity 和已保存 Repository credential，验证 Gateway CA/域名、拒绝跳转，按 session_id 核验返回路径；不访问 Control API、不重配对或清除身份。Restic 客户端 MUST 经受保护的 CA/client certificate 文件使用这个 HTTPS endpoint，私钥/仓库密码不能出现在 argv。单次期限取任务 deadline、开始后 24h、原 admission 到期的较早值；guard/watchdog 继续控制默认 72h 离线授权。

本批按用户指示暂不写新测试。新入口的 Actions 验证、持续授权分发、可信停止恢复和 Repository READY 仍待完成；历史夹具通过不等于生产 mTLS 链路跑通。真实三后端继续按 REP-016 在部署 Actions 验收。

### 7.25 生产 Gateway 停止与最终回写（ADR-0034）

生产 command MUST 调用 `RunService`，在 SIGINT/SIGTERM 或运行失败后先关闭全部入口、取消/join 请求、会话、子进程组、RFGA、刷新/审计 callback 和原回放循环。MUST 在 rclone 已退出后，以最多 3s 的独立 context 同步最后 token-only 配置；授权/校验失败或写入不确定 MUST 阻止完成证明。MUST 核验 runtime 仅剩自身 `.lock`、无组清理失败和未确认刷新，并成功关闭 runtime。原 owner 云端明文 MUST 已清零，全部 producer MUST 冻结。

随后 `Queue.Finish` MUST 使用原来源签名，加密追加唯一固定无身份 `runtime_cleanup/joined` 事件，沿用 `audit`（仓库）或 `global_audit`（全局）kind 和原 header/序列/哈希链；不新增字段、签名域、schema 或 Agent 能力。仓库记录 MUST 引用最后普通授权 revision。普通 Append/记录 callback MUST 拒绝终结事件，恢复 Queue 只有公钥 MUST 无法追加。已失败、关闭、未认领或未冻结队列 MUST 拒绝；容量不足或不确定写入 MUST 保留占用。

生产 daemon MUST 在证明落盘后用独立最多 1min context，经既有 RFGR 先排空全局、再排空仓库队列，按原回执可靠保存后回收记录。服务管理器 MUST 允许该有界停止过程（systemd `TimeoutStopSec=120s`）；MUST 继续 `Restart=no`，不能把旧 metadata 当重启配方。嵌入式 `StartService` 保持由可信调用方显式收尾，不自动产生终结记录。

中心 MUST 核验注册来源、签名/解密、完整 binding、连续 sequence/previous hash、原 grant revision 与时间后，才接受终结事件。仓库事务 MUST 同时写入最后接收历史/审计、撤回该 runtime 剩余授权、封存来源、释放原占用并写 outbox；并发续期/维护 MUST 继续由原 credential/admission/source 锁串行。精确重复 MUST 幂等，封存后的新记录 MUST 拒绝。全局记录只 MAY 封存自己的来源，不能授予任何仓库访问或释放权。runtime 完成 MUST NOT 吊销 Agent 配对或删除其已接受配置。

本节扩展 §7.13/7.20 的固定审计事件 allowlist，并替代 §7.21 关于 replay 永不触发封存/释放的绝对表述：replay 只有重送既有已签名终结记录时，中心才执行上述事务；tail、普通记录、回执或排空本身仍不构成证明。中心不可达/确认丢失时 MUST 保存原 wire；恢复后 MAY 用原 replay 命令精确重送，不得重签、补造或恢复失败 owner。升级 MUST 先中心后 Gateway，旧中心 MUST 拒绝未知事件并保持未确认状态。

硬崩溃、SIGKILL、队列损坏/已满、最终刷新或清理不确定且没有终结记录时 MUST 保留占用，后续需可信中心受控恢复。中心正常轮换与受控恢复入口见 §7.27；本地新 runtime 的配置/进程仍由可信部署独立准备和启动，中心 MUST NOT 生成 Gateway 私钥或代替服务管理器重启。真实产品备份仍待接通；READY 读模型见 §7.26。本次未新增测试用例，新终结/轮换/恢复流程的专门验收保持待验证；既有 Actions 通过不能替代它。

### 7.26 Gateway 真实后端观察与仓库可用性（ADR-0035）

生产 `RunService` MUST 在全部单次初始化与公网绑定后启用一个串行观察 worker；启动立即开始一轮，此后 SHOULD 每 5min 开始下一轮，轮次时间包含探测耗时。每仓库最多 30s，沿原私有 append-only 后端执行 `HEAD config`，必须 200 且 Content-Length 为 1..65536，HEAD 最多 15s。TLS 证书无效 MUST 停止整次运行。空闲探测与正常会话共享原 owner/runtime/材料 watcher、容量及清理，不能初始化/写入对象；已有会话复用同一 backend，并加入 session 生命周期的取消/join。空闲探测期间新会话 MAY 返回 409，不得消耗 operation 去重记录；仅探测容量竞争 MUST 跳过，不能永久失败原 owner。

成功 MUST 通过原注册仓库来源 Queue 追加 scoped `backend_ready/verified`，仍为原 `audit` kind/签名域。真实会话原有 HEAD 成功后同样记录观察。失败不能生成健康证据，且不改变原 owner 失败、授权/时钟/Queue watchdog 和最后刷新/清理要求。生产停止 MUST join 观察 worker 后才可产生终结清理记录；嵌入式 `StartService` 保持原显式调用契约。

探测忙碌 MUST 返回 `Retry-After: 1`。Agent 只对该明确响应最多在 30s 内以相同 operation ID、Repository 和原 deadline 重试；其他冲突或响应不确定沿用原失败/清理路径。取消及任务 deadline 继续限制整个会话，不新增任务或延长期限。

中心 MUST 按原 RFGR 来源/签名/解密/序列/时间接收，scoped event 的 Host/Repository/Gateway 必须与原 header 一致；全局来源不得产生该观察。仓库 list/detail 只 MAY 从这些已提交记录构建 READY 读模型：当前初始化/ACK、身份/凭据/配置指纹、完整交付/秘密引用匹配，原占用及该 runtime 的最新普通授权未到期/撤回，来源未封存/占用未释放；用于观察的原 grant 在探测时有效，且观察不在未来、距今小于 10min。曾有观察但条件不满足 MUST 显示 DEGRADED。LOCKED/DISABLED/ERROR 保持优先，无观察不得因回执或 ACK 显示 READY。

该读模型 MUST 不更新配置 revision/updated_at、不产生授权/维护/清理/备份结果。控制面不可用时本地备份仍按原窗口执行；恢复后的过期历史回放不能重新 READY，实际新探测才能恢复可用显示。必须按预期观察/备份/刷新量配置原有 Queue 上限，满/坏/不确定队列继续停止使用并保留 fence，不为健康记录丢弃其他证据。升级先中心后 Gateway；旧 reader 拒绝未知事件。按用户要求未新增/改写测试，新生产观察和状态投影仍待专门验收。

### 7.27 Gateway 正常轮换与受控恢复（ADR-0036）

正式部署或 Actions MAY 以中心服务 UID 显式执行；开发工作区 MUST NOT 执行：

```text
restfleet-server gateway-rotate --config-file /var/lib/restfleet/gateway-rotation/rotation.json
restfleet-server gateway-recover --config-file /var/lib/restfleet/gateway-recovery/recovery.json
```

两个文件 MUST 位于中心服务所有 canonical 0700 私有目录，0400/0600 regular、单链接、无特殊权限/symlink/hardlink/pending；字段全部显式，MAY 排版空白，其他编码 MUST 与 Go `encoding/json` 规范重编码相同。生产 MUST 以非 root 中心 UID 执行。

`rotation.json` 字段顺序为 `version`（1）、`current`（既有 `GatewayAuthorityTarget`：binding、source_public、socket_path、gateway_uid、shared_group）、`next_preparation`（§7.18 的中心准备输入）、`plan_file`、`audit_config_file`、`wait_seconds`（1..300）。current/next 的 runtime 必须不同、Agent/UID/组必须相同；两个路径必须独立、绝对、规范且不是配置文件本身。`audit_config_file` MUST 是 §7.22 的受保护全局来源 metadata，其 runtime 必须等于下一 runtime，来源公钥不得与仓库来源相同。

轮换 MUST 先读取当前最新决定并核验完整 binding；只有原占用未释放、仍有足够剩余时间（至少 `wait_seconds + 60s`）时才 MAY 开始。中心 MUST 投递明确撤回并保持同一 owner；等待下一状态时每 1s 重读权威记录，超时或信号取消 MUST 返回固定错误并保留 fence。只有 `released_at` 已提交且存在该 runtime 的 `GATEWAY_RUNTIME_CLEANUP_COMPLETE` 时，才 MAY 调用原 `gateway-prepare` 预留下一占用、注册下一全局来源、等待新材料 socket、执行单次初始化并接续原 authority worker。已 committed 撤回只 MAY 精确重放；不得重新初始化、重发材料、复用旧 binding 或把恢复释放当作清理证明。部署者 MUST 在等待窗口内以 Gateway 服务 UID 启动已预置的新进程；中心命令不启动子进程。

`recovery.json` 字段顺序为 `version`（1）、`binding`（完整 §7.11 binding）、`source_public`（可选；从未注册来源时 MUST 省略，否则为 32-byte 非全零 Ed25519 公钥）。恢复 MUST 由管理员在停止旧进程并隔离 Host 后显式执行，且同时满足 §8.9：占用已到期、无终结记录、无有效普通授权、来源匹配。事务 MUST 原子撤回剩余授权、关闭来源、写 `GATEWAY_BACKUP_RECOVERY` / `RECOVERY_CONFIRMED` 审计并释放占用；MUST NOT 生成 `runtime_cleanup/joined`、删除 Queue、重发材料或恢复旧 owner。已释放的精确重复 MAY 幂等成功；未到期、已有终结记录或来源不匹配 MUST 拒绝且保留占用。

两个命令均复用现有 DB/schema、签名/待回写密钥和审计链，不新增 schema、wire、公开 HTTP/gRPC API 或服务依赖。RESTFLEET_GATEWAY_AUTHORITY_FILE 非空时 `gateway-rotate` MUST 拒绝，避免同一 admission 出现第二个投递者；`gateway-recover` MUST 在停用活动 authority worker 后执行，同一进程配置非空时 MUST 拒绝。REP-058–060 的既有边界保持，轮换/恢复的 Actions 专门验收在 M4 退出前补齐；命令成功不代表 Repository READY 或真实备份完成。

#### 本地新 runtime 预置

Gateway 服务 UID MAY 在正式部署或 Actions 执行以下命令；开发工作区 MUST NOT 执行：

```text
restfleet-gateway provision --config-file /var/lib/restfleet-gateway/metadata/provision.json --state-file /var/lib/restfleet-gateway/metadata/provision-state.json
restfleet-gateway finalize --config-file /var/lib/restfleet-gateway/metadata/provision.json --state-file /var/lib/restfleet-gateway/metadata/provision-state.json --plan-file /var/lib/restfleet-gateway/metadata/plan.json --registration-file /var/lib/restfleet-gateway/metadata/registration.json --output-file /var/lib/restfleet-gateway/metadata/service.json
```

`provision.json` 是 Gateway 所有的受保护配置，字段顺序为 `version`（1）、`environment`、`agent_id`、`admission_lifetime_seconds`、`lifetime_seconds`、`material_socket`、`authority_socket`、`gateway_uid`、`shared_group`、`central_pin_file`、`audit_source_file`、`repository_source_file`、`audit_queue_directory`、`queue_directory`、`runtime_directory`、`rclone_binary`、`certificate_file`、`tls_key_file`、`client_ca_file`、`listen_address`、`max_bytes`、`max_records`、`max_sessions`、`server_uid`、`replay_socket`、`startup_wait_seconds`。所有字段显式；路径、UID/组、端口、socket 长度和上限沿用 §7.23/§7.24。`gateway_uid` MUST 等于当前 Gateway 服务 UID；共享组模式要求 `server_uid` 为不同非 root UID，私有模式要求二者相同。Queue MUST 已存在或由命令创建为当前服务所有 0700 持久目录；runtime MUST 为当前服务所有 0700 tmpfs；TLS/CA/pin/Rclone 仍按既有受保护策略校验。

`provision` MUST 生成新的 runtime UUIDv7、独立全局审计 origin UUIDv7 及两把本机来源 seed，创建缺失的私有 Queue/runtime 目录，并以无覆盖语义写 durable `provision-state.json`（仅含公开 ID/公钥和原配置）。stdout 只输出 `version`、`preparation`、`audit_registration`：前者按 §7.18 字段供中心 `gateway-prepare`，后者按 §7.22 字段供 `gateway-audit-register`。私钥 MUST 留在 Gateway 文件；命令不联系中心、不注册、不初始化、不启动进程。已有 state MUST 精确匹配原 config 和 source 公钥幂等返回；state 缺失但 source 已存在、config/来源不匹配或目录不安全 MUST 拒绝，不得接管旧运行。

中心准备与注册成功后，部署者 MUST 将 plan 和注册结果复制为 Gateway 服务 UID 可读的受保护文件（内容只有公开 metadata；不得复制中心私钥/DB 材料）。`finalize` MUST 核验 plan/registration 的 runtime、Agent、来源公钥、socket、UID/组与 state 完全一致，构建 §7.23 `service.json` 并调用同一 `Validate`；已有不同输出 MUST 拒绝，精确重复 MAY 幂等。成功后部署者以 `restfleet-gateway run --config-file service.json` 启动本进程。预置/定稿成功不证明来源已注册、占用合法、清理或 READY。

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

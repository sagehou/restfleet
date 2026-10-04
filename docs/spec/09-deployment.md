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

### 7.9 中心占用材料借用与刷新

`ControlPlane.WithBackupMaterial` MUST 在当前占用及访问审计提交后解密云端配置，仅向可信中央 runner 借出 rclone config、remote 和占用 metadata；MUST NOT 将 master key 或 Restic password 交给 Gateway。runner MUST 接入 §7.8 的 supervisor、遵守截止时间并等待全部工作完成；本入口本身不发布路由、不释放占用。刷新回调串行化，中心 MUST 校验 token-only 差异并 envelope 加密 CAS 回写；相同配置不产生新版本，回调在借用结束后不可再使用，原始错误不外传。

此接线仍是在线中央材料服务；公网 command、数据面审计持久化、独立 Gateway 进程的受保护材料交付、owner 恢复与 AGT-005 离线协调未完成。不得将它直接暴露为 Agent API 或据此标记 READY。

### 7.10 有界离线授权与待回写目标

用户已接受 ADR-0017 的独立 Gateway 方案；本节是待实现约束，不是公网部署步骤。§7.8 的在线轮询与 §7.9 的同步 DB 回写在替代协调器完成验证前保持原行为，MUST NOT 直接关闭这些检查以模拟离线成功。

后续在 M4 内按以下依赖顺序交付：

1. **授权与连续 fence**：中心签发、在线续期、Gateway 验证和防回滚 MUST 绑定同一可信运行实例及完整资源/配置版本。新授权 MUST 先经中心事务验证当前状态、精确 ACK 和未释放占用；禁止预发可在撤销后无限延寿的未来授权。schema 11 的到期时间目前不可更新，实施续期 MUST 同时提供显式迁移、旧 writer 升级约束与并发负向测试，不能通过释放再抢占制造维护空窗。
2. **受保护通道与持久化**：独立 Gateway MUST 通过有身份验证、版本和大小限制的本地通道获取必要材料，不获取中心 DB/master key。待回写区 MUST 在受保护持久卷上有字节/记录数硬上限，可靠落盘后才确认接受；MUST NOT 丢弃未确认记录来腾空间。加密封装与来源认证 MUST 独立验证，秘密不得进入审计 payload、日志或 artifact。
3. **回写与恢复**：每条记录 MUST 有可信 owner/运行实例/占用绑定、唯一标识和有序版本；中心 MUST 拒绝冲突重放、跳序、篡改、跨绑定和非 token-only 配置变化。DB 提交后才 MAY 确认对应记录并回收本地空间，确认丢失 MUST 幂等重放；刷新仍执行加密 revision CAS。记录全部接收不等于进程清理，释放占用仍需确认全部请求、子进程和持久化工作结束。失败保留 fence，不自动重启或接管。
4. **生产接线与验收**：再接入 command、独立 readiness、会话交付及恢复状态。readiness MUST 区分控制面不可达、剩余授权不足、待回写容量和恢复阻塞；不得把监听 socket 存在或 Agent ACK 当作 Repository READY。

离线测试 MUST 覆盖签发/续期前后断网、接收延迟、旧授权重放、时钟回拨、授权到期、撤销/禁用、连续多次本地计划备份、OAuth refresh、磁盘满/写入失败、损坏记录、中心提交后确认丢失，以及 Gateway 崩溃重启与维护竞争。MUST 验证已授予权限不会超过签发后 12h，且 fence 不因超时或崩溃自动释放。

AGT-005 的 12h 离线备份要求保持未完成：MUST 明确记录实验的签发、续期、失联和备份时间，不得把“授权最多 12h”当成“任意时刻失联后还剩 12h”。若完整可用性要求与撤销上限无法同时满足，MUST 报告取舍并请求决定，不得静默改动验收或通过重置离线计时掩盖差异。真实三后端刷新/认证/恢复仍需安全环境验收；上述设计与模拟测试均不替代人工证据。

### 7.11 签名授权决定与本地状态（内部 v1 契约）

ADR-0018 交付 security 签名编解码与 Gateway 本地状态原语；MUST NOT 替换 §7.8 的在线核验后直接开放生产 command。没有新增公共 HTTP/gRPC 接口、DB schema、中心签发服务或持久化回写区。

wire MUST 为规范 JSON payload 后直接拼接 64-byte Ed25519 签名，总长最多 2048 bytes；签名内容为 ASCII 前缀 `restfleet:gateway-authorization:v1`、一个 NUL 字节和 payload。使用独立中心签名密钥，验证公钥 MUST 经受保护配置交付；wire 没有公钥、算法或可自选版本。payload MUST 精确匹配 `security.GatewayStatement` 的字段顺序和 Go encoding/json 紧凑编码，未知/重复字段、额外空白、替代 UUID 表示及非规范数字 MUST 拒绝，不进行宽松归一化。

字段顺序为 binding、revision、issued_at、expires_at、revoked。binding 内字段依序为 admission_id、owner、runtime_id、agent_id、host_id、repository_id、gateway_id、storage_credential_id、delivery_id、gateway_secret_ref、restic_secret_ref、configuration_hash；ID 均为规范 UUIDv7，hash 为 64 位小写 hex 的现有 Gateway origin/CA 指纹。revision 为 1..MaxInt64 的无损十进制整数；时间为 UTC Unix 整秒，issued_at 必须为正且不晚于 9999 年末。普通授权要求 issued_at < expires_at <= issued_at + 12h 且不晚于 9999 年末；明确吊销必须 revoked=true、expires_at=0。绑定只含引用，不含秘密；云端材料及版本仍须由后续受保护交付与连续占用检查保证，签名 metadata 本身不是材料校验。

Gateway MUST 从可信配置构造唯一运行绑定并复制验证公钥，不信任 wire 提供的新绑定。只有验签、精确绑定和时间检查通过的单调 revision 才能更新状态；相同 revision 仅允许完整相同内容的幂等重放，重放不重置截止时间。旧 revision、同版本改参、签发时间倒退、未来签发及初次收到已过期授权 MUST 拒绝；普通续期不得复活已知吊销的同一绑定。中心 MUST 只签发已经提交的、经过当前身份/ACK/占用核验的决定，签名工具函数不代替事务授权。

本地状态为 UNKNOWN、VALID、EXPIRED、REVOKED、CLOCK_UNSAFE，连接状态由 transport 单独维护。离线时无新决定不改变已有授权；到期仍停止使用但不是吊销，重连本身也不能续期。Status MUST 使用可信本机时钟，按签发截止时间及接收时建立的单调 deadline 双重检查；发现墙钟回拨或无效时钟后禁止自恢复。已有明确吊销即使遇到时钟异常仍保持 REVOKED。Accept 的幂等成功仅表示已接收，不代替每次使用前的 Status 与数据面其他安全检查。

此本地状态不持久化，不证明进程清理、不释放 fence，也不自动取消/恢复生产会话。调用方 MUST NOT 重建它来清除防回滚/吊销记录；Gateway 重启必须使用新的 runtime_id 并经中心恢复协调，不能离线重放旧授权。中心事务签发/续期接线见 §7.12；可信吊销分发、可靠回写及完整离线验收仍待完成，现有在线流程继续 fail closed。

### 7.12 中心事务签发与连续占用

schema 12 / ADR-0019 将授权决定追加持久化，原占用 ID/owner、释放状态和截止时间保持不变。在线续期只更新签名授权决定，实际有效期 MUST 不超过签发后 12h 或原占用剩余期限；不能据此宣称任意失联时刻之后都有完整 12h。超过原占用期限的长期运行仍需后续生命周期协调，MUST NOT 自动释放或重开占用。

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

每次路由和每次向 backend 转发（含上传完成后的 HEAD/POST 与锁 DELETE）前 MUST 检查本地签名授权、原占用期限及 Queue 可写性；watchdog 每 100ms 核验，取消空闲/已开始工作并等待全部清理。生产者可写性保守要求剩余一个最大 512 KiB wire 与一个记录位置，Append 仍执行真实大小校验；关闭、冻结、写入不确定或空间不足 MUST 停止。吊销、到期、时钟不安全或审计/刷新失败 MUST 取消，运行失败后 owner 永久停止启动；新授权、重连或排空不能恢复失败 owner。取消不能撤销已在外部后端提交的请求，不保证反向回滚数据。

运行失败或 Close MUST 在取消/join 会话与 watchdog 后关闭 token recorder、清除当前明文并冻结来源；Queue 仅保留给可信回放。成功返回与 Close 均不是中心封存、崩溃清理证明或 fence release，MUST NOT 自动调用 ReleaseBackupAdmission。固定二进制 TLS 连续两次备份及负向测试只证明内部生命周期，不替代 production delivery、scheduler、可信恢复、READY、REP-016 或 AGT-005。

### 7.15 单次加密材料初始化（schema 14 / ADR-0022）

`ControlPlane.GatewayMaterialDelivery` 仅供可信中心协调器使用，不是公共 API。协调器 MUST 提供独立可信 binding/来源 pin，并串行化交付、授权投递及清理；来源私钥与中心验证公钥 MUST 从 Gateway 受保护本地配置取得，不信任 Agent 输入。中心事务最多 3s；交换最多 5s，先验证来源挑战，再提交当前状态核验、单次交付意图与访问审计，最后短时解密并签名加密。失败不回显底层错误或配置。

Gateway 默认监听新建的服务所有 0700 目录内 0600 Unix socket，中心主动连接；双方 MUST 核验预期 SO_PEERCRED UID，中心还 MUST 验证预注册来源签名。原正向测试为同 UID；显式跨 UID 访问见 §7.16，不能据此声称完整生产 UID 隔离。来源创建与独立 pin/source 文件装载见 §7.17；完整可信 binding 配置、来源注册、进程启动器与数据面 command 仍未接线。

帧 MUST 为 ASCII `RFGM`、big-endian uint32 版本 1、16-byte runtime UUID、big-endian uint32 长度、wire，与回放的 `RFGR` 隔离。挑战与回执最多 2048 bytes，材料 wire 最多 512 KiB，config 最多 256 KiB；空帧、错误 runtime、版本或超长帧 MUST 拒绝。

三个 wire MUST 为 Go encoding/json 规范紧凑 JSON 后拼接 64-byte Ed25519 签名。签名域分别为 `restfleet:gateway-material-challenge:v1`、`restfleet:gateway-material:v1`、`restfleet:gateway-material-receipt:v1`，各加 NUL 后接 payload。未知/重复字段、非规范数字、空白、替代 UUID 表示 MUST 拒绝，不归一化。

挑战字段顺序 MUST 为 binding（§7.11）、nonce、recipient；nonce 是 fresh 32-byte 随机值，recipient 是 Gateway 临时 X25519 公钥。二者 MUST 编码为 32 个 0..255 整数的 JSON 数组。材料外层顺序为 challenge、source、ciphertext；source 与 ciphertext 使用标准 base64。ciphertext 使用 `nacl/box.SealAnonymous`，接收私钥仅在 Gateway。中心签名验证后解密，内层顺序 MUST 为 challenge、source、pending_recipient、statement、admission_created_at、admission_expires_at、secret_revision、remote、config_hash、config；pending_recipient 是中心待回写 X25519 公钥，以 32 整数数组编码，statement/config 为标准 base64。MUST 比对内外挑战及可信 source，检查原占用 UTC Unix 整秒期限、初始 secret revision、普通有效授权和 config SHA-256（64 位小写 hex）。材料 MUST NOT 含 DB/master/signing/中心 recipient 私钥、Restic password 或 Agent capability。

`MaterialReceiver` MUST 单次使用。无效交换也消费接收者，取消关闭 socket；Close MUST 消费未开始实例或取消/join 正在执行的交换，并清零来源私钥副本和临时接收私钥。install/rollback/denied 回调 MUST NOT 调用等待自己的 Close。安装只 MAY 通过 `InstallGatewayMaterial` 构造 fresh Queue/唯一 owner，config 借用后清零；回调 MUST 返回 rollback，安装或回执发送失败时取消并 join owner，保留 Queue 供回放。

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

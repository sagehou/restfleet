# ADR-0033：Gateway 复用 Agent 配对身份

状态：已接受；首次接线实现中，Actions 验收待完成。

用户要求首次用短期单次 token 配对，此后使用本机生成的长期密钥对通信，自动重连、续证，管理员可吊销。M2 已实现这个流程，Gateway MUST 复用，不新增通信密码或 bearer 生命周期。

生产 Gateway MUST 使用受保护 Agent CA 验证现有 mTLS 证书，从规范 URI SAN 导出身份并匹配中心授权 owner 的 Agent/Host/Repository。私钥 MUST 留在 Agent；证书沿用 30 天有效期、剩余 7 天自动续证。普通故障 MUST NOT 清除配对或 last-known-good 配置。MUST NOT 以 payload、Basic 密码或代理头选择身份。

本决定替代 ADR-0014 的独立秘密能力及安全规范 §7 的 Gateway 密码轮换。Agent MAY 直接对已授权的本 Host 仓库开启会话，无须逐次访问 Control API。请求 operation_id 仅用于去重，不证明中心 Operation 或结果；Gateway MUST 自行生成非秘密 UUIDv7 session_id，数据路径为 `/restic/{gateway_id}/{repository_id}/{session_id}/`。旧路径 MUST NOT 映射到新会话，锁归属与历史对象保护仍遵守 ADR-0014。

相同 operation_id/参数 MUST 返回原会话，不重复运行或延长期限；参数冲突 MUST 拒绝。会话只管理路由及运行内去重，不作为权威任务队列，不跨进程恢复。每次运行 MUST 保留最多 4096 条记录，达到上限拒绝新会话；新运行仍须可信中心清理/占用协调。期限 MUST 取任务 deadline、开始后 24h、原占用到期的较早值。既有 guard/watchdog、可靠审计/回写、tmpfs 与私有 rclone socket MUST 继续生效。

`restfleet-gateway run` MUST 强制要求 `client_ca_file`，不得回退 Basic；无 CA 的旧进程内构造器保留给已有 adapter/conformance 夹具，不构成生产部署入口。独立会话契约见 `api/openapi/gateway-v1.yaml`，readiness 不授予 Repository READY、清理证明或占用释放。

新 Hello capability `repository_identity_v1` 与原 `repository_credentials_v1` 一起声明。新 Agent 的 CredentialRevision MUST 不携带 gateway_password；其他内容完全相同时 MAY 单向清除同 revision 的旧密码，重新加入密码或变更绑定/Restic 密码 MUST 拒绝。旧 capability 交付及 schema 16 的 Gateway secret 引用保留兼容，生产认证不使用它们。本次不增加 verifier、候选密码激活或强制 schema 清理前置条件。

吊销复用中心身份状态及 RFGA 明确撤回决定；现有中心授权 worker 已接入在线分发，按 10s 轮询当前 Agent/Host/Repository/StorageCredential 的明确撤回状态，Gateway 原 watchdog/逐次请求检查据此停止使用和清理。Agent 续证保留本机私钥，每次新 Gateway 会话从本机 identity 文件装载当前证书，不要求重新配对。该生产链路的专门验收仍待完成。离线遵守 ADR-0032 的配置窗口（默认最后签发起 72h，受原占用限制），不能声称中心失联时立即获知吊销。证书不绕过授权、撤回不可复活或维护排斥；会话结束、来源排空或授权到期不释放 fence。

REP-011 保留 ID，替代为身份自动续证/吊销；REP-020 的独立秘密改为身份与独立会话范围，其他负向要求保留。按用户指示暂不写新测试，源码完成及未验证行为 MUST 分开记录，生产分发/可信恢复/READY 未完成前不能声称已跑通。

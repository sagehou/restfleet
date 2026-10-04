# ADR-0030 — 中心显式注册 Gateway 全局审计来源

状态：已接受（M4 全局审计启动接线；不表示 daemon 或 READY 已完成）。

## 问题

ADR-0027 已提供独立全局来源及审计事务，但生产启动者仍只能通过进程内调用注册来源。Gateway 启动需要取得可信注册结果和中心接收公钥，不能让请求或回放帧自行注册公钥。

## 决定

增加中心显式命令 `restfleet-server gateway-audit-register --config-file FILE`，复用初始化命令的运行配置、受保护密钥装载、数据库连接及总 10s context。命令 MUST 以非 root 中心服务 UID 在生产运行，普通 Server 启动 MUST NOT 自动执行。它不启动 API、worker、CA、Gateway、rclone 或其他子进程。

管理员 MUST 从可信 Gateway 身份导出取得来源**公钥**，独立选择本次 origin/runtime UUIDv7，并在中心私有目录准备规范版本元数据。装载沿用既有 protected reader，1024 bytes 上限及精确 JSON 重新编码检查；格式见 09 §7.22。metadata 不含 socket、UID、仓库、占用或秘密，不能作为证明进程新鲜性的配方。

数据库/schema/审计链核验及现有幂等注册调用共享最多 3s context。成功只输出公开的 binding、来源公钥、中心 X25519 接收公钥及原注册 UTC 时刻。独立中心签名 pin 继续由管理员配置，不从该结果或未认证 peer 自选信任。

精确重复保留原来源与时刻，不重复注册审计；错误公钥/runtime、同 runtime 新来源及封存后注册 MUST 拒绝。输出失败、取消或不确定提交 MUST NOT 回滚/删除已提交来源；管理员 MAY 显式重试精确原 metadata。命令 MUST NOT 自动重试、授予备份权限、读取云端材料、封存来源、释放占用或宣布 READY。

不新增公共 HTTP/gRPC 接口、wire、DB schema 或服务依赖。中央私钥在未排空 Queue 期间 MUST 保持既有持久复用规则。

## 验收

REP-056–057 仅在 Actions 验证保护文件/规范编码与固定错误、实际 CLI→PostgreSQL→公开 JSON、输出失败后的精确重复、注册冲突/封存/schema/审计/DB 失败，以及生产 root 拒绝。CLI 使用从已迁移表复制约束的独立 schema，三后端完整数据库验收经生产配置入口继续验证 audit-only 无授权/材料副作用。Gateway daemon、Agent 能力、可信恢复/readiness、rotation/READY、真实云和 AGT-005 继续待完成。

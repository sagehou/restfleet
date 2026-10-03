# M4 Gateway 待审查分支

目标分支为 `feat/m4-gateway-pending-replay`，基线 `da0d92f`。当前内容尚未合并，不计入 main 完成进度。

## 拟用 PR 标题

feat(gateway): add encrypted replay, signed local owners and isolated bootstrap channels

## 拟用 PR 说明

Gateway 在中心已签发的有界授权内运行备份时，需要可靠保存失联期间的审计与 token 刷新，避免确认丢失导致重复写入或重新初始化。本变更增加来源签名的加密待回写区、中央原子回放及精确确认，接入唯一生命周期 owner，再用挑战绑定的加密 Unix 通道单次初始化。显式共享组支持不同非 root 服务 UID；连接仍核验指定 UID 与独立签名，私钥与队列保持各服务私有。

- schema 13 注册不可替换来源及只追加接收历史，回写 effect/审计/token-only 加密 revision CAS 原子提交；记录可靠保存后才接受，精确确认可靠保存后才回收。
- 本地签名授权 owner 复用 supervisor，连续备份维持最新 token/revision；每次 backend 转发检查授权/容量，失效取消并 join，失败 owner 不复活。返回或 Close 均不释放中心 fence。
- schema 14 每占用只提交一次材料意图及访问审计，之后才解密和签名加密；提交后失败、回执未知、精确重试及新挑战不重新交付。初始化回执不证明 Agent ACK、READY 或清理。
- 默认 0700/0600 通道保持。显式共享组要求精确监听方 UID/GID、0710 目录和 0660 socket，同组用户不能改目录或替换 socket；SO_PEERCRED 仍绑定精确 peer UID。Server replay 新参数必须配对，不提供公网管理接口或自动 Gateway 启动。
- ADR-0020–0023、REP-031–042，以及安全/数据库/部署规范与 M4 进度同步。没有新 REST/protobuf/Web 契约。

### 验证

前三个本地提交在用户要求迁移验证环境之前已通过 Go/race、真实临时 PostgreSQL、固定 Restic 0.19.1/rclone 1.75.1、vet/staticcheck 与跨架构构建；这是历史证据，不证明后续变更通过。临时数据库、工具链、缓存和构建产物已清理。

跨 UID 新变更未在开发环境编译或运行。Actions `gateway-isolation` 新增 race-enabled 实际双服务/第三用户验收；root 只用于测试协调，实际协议服务为不同非 root UID。Gateway 在自身进程生成来源私钥，中心只取得公钥；中心私钥通过匿名管道独立交付。测试包含加密初始化、持久队列/签名回写及确认、明文清零、私钥访问与 socket 替换拒绝、同组第三 UID 拒绝。既有 quality job 继续覆盖真实 DB/固定引擎/完整 race，cross-build 直接编译 Gateway/queue/security 包，覆盖未接入 command 的代码。最终以本 PR 当前提交的 Actions 为准；在成功之前保持草稿。

### 升级与未完成边界

升级 MUST 停旧 writer 后迁移 schema 14，历史存在时相应 Down 拒绝。中心密钥持续复用，本批无密钥 overlap；确认、来源封存和授权到期均不证明进程清理。可信协调器仍须分别确认退出、全部回写及历史授权后才释放 fence。

M4 仍进行中。生产 source/pin 装载、启动协调、授权续期/吊销、全局离线审计、Agent 会话能力、可信恢复、command/readiness、rotation/READY、真实三后端和 AGT-005 未完成。签发后最多 12h 不代表任意失联后仍完整可用 12h；本套件不替代该证据。关联 #15。

## 发布前源码审查记录

本批仅修改源码、测试、工作流和文档。测试密钥在 Actions 运行时随机生成，fixture 配置经匿名管道传递；不提交凭据、证书、数据库、引擎/工具链、日志或编译产物。配置示例使用已有 synthetic 占位值。发布仍需解决此前自动审批提出的公开发布授权；此记录不代替 Actions secret scanner 或运行验收。

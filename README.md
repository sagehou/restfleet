# RestFleet

RestFleet 是一个面向多台 Linux VPS 的自托管 Restic 备份控制平面。它通过轻量 Agent 统一下发备份计划、采集运行状态、浏览快照、触发恢复并集中执行仓库维护，同时把 rclone 与云存储凭据严格留在中心节点。

> 当前仓库按 specification-first 方式开发。M0 与 M1 Control Plane Skeleton and Auth 已完成；`main` 分支上的规范仍是 V1 实现的约束来源。

开发工具链与升级策略见 `docs/development/toolchain.md`，贡献前 MUST 阅读 `AGENTS.md`。

## 核心边界

- Agent 仅主动向中心建立出站 mTLS 连接，不开放入站管理端口。
- 备份数据直接流向 Repository Gateway，不经过 Control API。
- rclone、OneDrive OAuth、Crypt 密码与仓库维护权限只存在于中心节点。
- Agent 可读取并追加其被授权仓库，但不能删除或改写已有仓库对象。
- 默认每台 Host 使用独立 Repository、独立 Restic 密码和独立网关身份。
- 中心离线时，Agent 使用最近一次已确认配置继续本地调度备份。
- `forget`、`prune`、`check`、`unlock` 只能由中心 Maintenance Worker 执行。
- Linux `amd64`、`arm64`，Native systemd 与 Docker 两种 Agent 部署方式均为 V1 要求。

## 开发与运行

常用质量门禁：

```sh
make generate
make lint
make test
make build cross-build
```

M1 Server 需要已经迁移到 schema v2 的 PostgreSQL。生产模式 MUST 通过只读 secret 文件提供数据库连接，且禁止关闭 Secure Cookie。开发 Compose 也拆分了 migrator/runtime 数据库身份，并且不向宿主机发布 PostgreSQL 或 metrics 端口。

首次启动：

```sh
cd deploy/compose
cp .env.example .env
./prepare-dev-secrets.sh
docker compose -f compose.yaml -f compose.bootstrap.yaml up --build
```

Bootstrap token 仅在首次初始化时从本机 `secrets/bootstrap-token` 读取，不应复制到日志或命令参数。在 Web 中创建首个管理员后，MUST 停止开发栈、删除 `secrets/bootstrap-token`，并只用 `docker compose up --build` 重启；数据库中的 bootstrap 状态会保持永久关闭。公开 HTTP 默认监听 `:8080`，Compose 默认只绑定到宿主机 loopback。

## 文档导航

| 文档 | 内容 |
|---|---|
| [产品规范](docs/spec/00-product.md) | 目标、角色、场景、非目标与产品规则 |
| [系统架构](docs/spec/01-architecture.md) | 控制面、数据面、维护面与组件边界 |
| [安全模型](docs/spec/02-security-model.md) | 威胁模型、PKI、凭据、权限与安全不变量 |
| [领域模型](docs/spec/03-domain-model.md) | Host、Agent、Repository、Template、Plan 等对象 |
| [Agent 协议](docs/spec/04-agent-protocol.md) | Enrollment、mTLS、心跳、配置同步与任务协议 |
| [备份与恢复](docs/spec/05-backup-restore.md) | Backup、Snapshot、Download、Restore、Retention |
| [HTTP API](docs/spec/06-api.md) | 管理 API 契约与错误模型 |
| [数据库](docs/spec/07-database.md) | PostgreSQL Schema、约束、索引与事务规则 |
| [Web Console](docs/spec/08-web-console.md) | 页面、交互、危险操作与状态表达 |
| [部署](docs/spec/09-deployment.md) | 中心、Native Agent、Docker Agent 与升级 |
| [可观测性](docs/spec/10-observability.md) | Metrics、Logs、Health、Alert、Audit |
| [V1 范围](docs/spec/11-v1-scope.md) | V1、V1.5、V2 的功能边界 |
| [验收测试](docs/spec/12-acceptance-tests.md) | 可执行的功能、安全与故障验收条件 |
| [实施计划](IMPLEMENTATION_PLAN.md) | M0–M12 的实现顺序和完成定义 |
| [Codex 规则](AGENTS.md) | 实现时不可违反的仓库级规则 |

## 技术基线

- Server / Gateway wrapper / Agent：Go
- Web：React + TypeScript + Vite
- 管理 API：REST/JSON，OpenAPI 3.1 为契约
- Agent 通道：gRPC 双向流，HTTP/2 + mTLS
- Database：PostgreSQL
- Agent 本地状态：bbolt，原子持久化
- Backup engine：外部 Restic 二进制
- Storage bridge：`rclone serve restic`
- 中心部署：Docker Compose
- Agent 构建：`linux/amd64`、`linux/arm64`，`CGO_ENABLED=0`

## 规范优先级

发生冲突时按以下顺序处理：

1. `AGENTS.md` 中的安全不变量；
2. `docs/spec/02-security-model.md`；
3. 其他 `docs/spec/*.md`；
4. `IMPLEMENTATION_PLAN.md`；
5. 代码中的注释和临时实现。

任何改变安全边界、外部 API 或持久化模型的实现，必须先修改规范并在 PR 中说明迁移策略。

## 参考实现能力

RestFleet 不复刻 Backrest 的执行模型，只参考其 Repo / Plan、Dashboard、Snapshot Browser、Operations 与 Maintenance 的产品组织方式。底层能力以 Restic 与 rclone 的官方接口为准：

- [Restic 文档](https://restic.readthedocs.io/en/stable/)
- [Restic JSON 输出](https://restic.readthedocs.io/en/stable/075_scripting.html#json-output)
- [rclone serve restic](https://rclone.org/commands/rclone_serve_restic/)
- [rclone OneDrive](https://rclone.org/onedrive/)

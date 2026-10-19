# M4 轮换与恢复交接（2026-10-19）

本批从 `bf9f978`（PR #67 交接）继续，源码分支 `feat/m4-rotation-recovery`。目标是把 M4 第二项剩余的能力接通：正常换运行、无终结证明的受控恢复，以及 Gateway 侧新 runtime 的本地预置/定稿。M4 仍未标记完成；新增行为的专门运行验收和真实三后端验收保持待完成。

## 已接入的源码

| 能力 | 入口 |
|---|---|
| 正常轮换：撤回旧 runtime → 等待签名终结记录与原子释放 → 预留下一占用 → 注册下一全局来源 → 单次初始化 → 接续授权 worker | `internal/server/gateway_rotation_linux.go`、`cmd/restfleet-server/gateway_rotation_linux.go` |
| 受控恢复：仅到期、无终结记录、来源匹配时撤回/封存/释放，审计 `RECOVERY_CONFIRMED` | `internal/persistence/postgres/gateway_recovery.go`、`internal/server/gateway_rotation_linux.go` |
| 本地预置：生成新 runtime/来源 seed、Queue/runtime 目录、durable state 与公开 preparation/注册 metadata | `internal/gateway/provision_linux.go`、`cmd/restfleet-gateway/provision_linux.go` |
| 定稿：按中心 plan/注册结果生成受保护 service.json，拒绝不同输出覆盖 | 同上 |
| 决策与规范 | `docs/adr/0036-gateway-run-rotation-and-recovery.md`、`docs/spec/09-deployment.md` §7.27、`docs/spec/02-security-model.md` §8.9、`docs/gateway-run.md` |

## 保持的安全边界

- 正常轮换只使用显式撤回，不把到期、排空、回执或进程不可达当作清理证明；没有 `GATEWAY_RUNTIME_CLEANUP_COMPLETE` 时不预留下一占用。
- 下一 runtime 必须使用新 runtime、独立来源、独立占用；旧 binding/材料/owner 不复用、不复活。
- 受控恢复只在原占用到期、无终结记录、无有效普通授权且来源匹配时执行；不伪造 `runtime_cleanup/joined`，不删除 Queue，不重发材料，不恢复旧授权。
- `provision` 在 Gateway UID 下生成来源 seed 和公开 metadata；中心不生成/读取 Gateway 私钥。Queue 必须为 0700 持久目录，runtime 必须为 0700 tmpfs。
- `finalize` 只在 plan/注册结果的 runtime、Agent、来源、socket、UID/组与 state 完全一致时写 service.json；已有不同输出拒绝。
- 未新增 schema、wire、公开 HTTP/gRPC API 或服务依赖。

## 当前验证

- 开发工作区只做源码/文档编辑与只读检查；未执行编译、测试、认证或服务启动。
- 分支推送触发的 Actions 以最新 run 为准。`1e2eeeb` 的 run `39229473961` 中 `quality` 已通过 `make test`，其余镜像与隔离 job 通过；`e8923d2` 的 run `39229882739` 仍在运行，结果以该 run 最终结论为准。
- 新增测试仅覆盖受保护 metadata/策略边界；轮换/恢复状态机、跨 UID 生产链路、自动续证/吊销、READY 过期降级和真实三后端仍需专门验收。

## 接手顺序

1. 先合并 PR #67，再把 `feat/m4-rotation-recovery` 作为独立交付审查/合并；不要用直接推送 `main` 绕过 PR。
2. 在部署环境或 GitHub Actions 执行轮换验收：正常撤回 → 终结记录 → 原子释放 → 下一 runtime 初始化；等待超时/丢失回执保留 fence；到期前拒绝；恢复仅到期后生效且不产生清理记录。
3. 补齐 M4 仍未验证的生产 mTLS 会话、自动续证/吊销、READY 过期降级、并发取消/join 与真实 OneDrive/Google Drive/WebDAV 验收。
4. M4 退出后再进入 M5（模板/计划/本地调度），M5/M6 的验收归属保持实施计划中的原始要求。

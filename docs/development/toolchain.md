# 工程工具链

## M0 基线

| 项目 | 锁定值 | 更新策略 |
|---|---|---|
| Go | 1.27.1 | 跟随受支持稳定补丁版本；minor 升级单独 PR |
| Node.js | 24.20.0 LTS | 跟随同一 LTS major；major 升级单独 PR |
| TypeScript | 5.9.3 | 当前 OpenAPI 生成工具的最新兼容 major；兼容后再升 7 |
| React | 19.2.x | npm lockfile + Dependabot |
| Vite | 8.2.x | npm lockfile + Dependabot |
| PostgreSQL | 18.6 | 固定 patch image tag；升级前跑 migration/兼容测试 |
| Restic | 0.19.1 | M4/M6 引入 binary 时固定版本与每架构 SHA-256 |
| rclone | 1.75.0 | M4 引入 binary 时固定版本与每架构 SHA-256 |

Go module 为 github.com/sagehou/restfleet，Web 使用 npm。Go、npm、GitHub Actions 与 Docker 依赖由 Dependabot 每周检查；升级不得绕过规范和验收测试。

M0 不把 Restic/rclone 放进空壳镜像：二进制从实际需要它们的 M4/M6 开始加入，并在同一变更中固定官方 checksum。这样避免当前镜像提前携带尚未调用的高权限工具。

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
| rclone | 1.75.1 | 中心镜像与 CI 固定官方 multi-arch image digest；Agent 镜像不包含 |

Go module 为 github.com/sagehou/restfleet，Web 使用 npm。Go、npm、GitHub Actions 与 Docker 依赖由 Dependabot 每周检查；升级不得绕过规范和验收测试。

M4 中心 Server/Gateway 镜像包含官方 rclone 1.75.1，固定 multi-arch digest `sha256:45401ad7410db1d67ffdb58e19059ad20b0d8e0285a60e38bbec55cc1019c7a5`，同时覆盖 linux/amd64 与 linux/arm64。CI 从同一镜像提取 binary 做离线契约测试，不需要 OneDrive secret。Agent 镜像不包含 rclone。Restic 在 Repository provisioning/执行接入时加入，并固定版本与 checksum。

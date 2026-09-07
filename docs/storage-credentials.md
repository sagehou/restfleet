# 中心存储凭据（M4 多后端）

当前提供配置导入、metadata、异步只读连接测试、替换/禁用，以及独立仓库记录。支持 OneDrive、Google Drive 和公网 HTTPS WebDAV，均包装在 rclone Crypt 后。仓库记录仍是 PROVISIONING，初始化任务、Public Gateway 与完整备份执行尚未接通，不能将“导入成功”或“读取测试成功”当成可备份。

## 准备与导入

1. 按部署文档配置中心 master key 和 TLS，沿用已有 master key，MUST NOT 为新后端换一把 key。
2. 在可信设备运行 rclone config，完成所选后端认证，创建一个直接包装它的 Crypt remote。配置是秘密，MUST NOT 提交 Git、上传工单或粘贴到聊天中。
3. 管理员进入 Console → 存储凭据 → 导入凭据，提交名称、Crypt remote 名称及仅含这两个 remote 的配置；后端自动识别，无需提交 provider。
4. 保存后状态为“未测试”，秘密输入清空且不会回显。点击“测试连接”可观察持久化 Operation；成功仅表示 Crypt 根目录可读取，不证明写权限。

## 支持矩阵

| 配置段 | 允许字段与限制 |
|---|---|
| OneDrive | type=onedrive、token、drive_id、drive_type、client_id、client_secret、region=global |
| Google Drive | type=drive、token、client_id、client_secret、scope、root_folder_id、team_drive |
| WebDAV | type=webdav、HTTPS url、vendor、user、pass 或 bearer_token |
| Crypt | type=crypt、remote、password、password2、filename_encryption=standard、directory_name_encryption=true |

每次只导入一个后端和一个 Crypt。Google Drive 对应 rclone 的 type=drive，不是 type=gdrive。Crypt root 可为空或 canonical 相对路径；remote 名称 2–64 位 ASCII 字母数字/下划线/连字符，以字母开头。配置最多 256 KiB UTF-8；额外 remote、重复 key/section、未知字段（即使为空）、全局选项、命令、env_auth、任意本地文件引用和自定义 OAuth endpoint MUST 被拒绝。

### OneDrive

token MUST 包含 access_token、token_type=Bearer、refresh_token、RFC3339 expiry；支持 personal/business/documentLibrary 与默认 global 端点。旧记录、密文和 revision 原样兼容。

### Google Drive

先按 [rclone 官方指引](https://rclone.org/drive/#making-your-own-client-id)创建自有 OAuth client 并授权；本批不依赖 rclone 公用 client。导入 client_id、client_secret、完整 token，以及实际 root_folder_id 或 team_drive，MUST NOT 使用 root 别名。建议明确选择固定目录，避免新授权账户将默认根目录切换到另一位置。

scope 支持 drive 或 drive.file（缺省为 drive）；drive.file 仅能访问应用获权的文件，指定已有目录时须确认实际权限。服务账号 JSON 文件/内嵌 JSON、域委托、只读 scope 和自定义 OAuth 端点不开放。rclone 配置中的 OAuth client_secret 是原生字符串，不是 Crypt obscure 密码；数据库仍对完整配置做信封加密。

### WebDAV

URL MUST 为公网 HTTPS 地址；不接受 URL 用户名/密码、query、fragment、异常路径、私网/loopback/metadata/保留 IP。DNS 校验在 runtime 执行，混合公网与私网 DNS 也会拒绝；导入 UNTESTED 不意味着地址可访问。证书必须由中心现有系统信任链验证，内网/NAS、自签名 WebDAV 本批不开放，不要使用 insecure TLS。

支持 vendor=other（默认）、nextcloud、owncloud、fastmail、rclone。Basic 认证使用 user 与 rclone 已 obscure 的 pass；静态 Bearer 使用 bearer_token，不能同时填两种认证。NTLM、SharePoint、headers、bearer_token_command、auth_redirect 和用户指定的 unix_socket 均不支持。

runtime 自动生成服务 UID 私有 Unix socket，将连接固定到已验证的公网 IP:port；TLS 仍由 rclone 校验，重定向不会选择新的网络目的地。不需要添加端口映射或代理设置，socket 配置不进入数据库。静态密码/Bearer 不会自动刷新，过期时通过“替换凭据”更新。

## 替换与禁用

替换 MUST 保留 backend、remote、目录/drive ID、WebDAV URL/vendor/user/认证方式和 Crypt password/password2（包括 obscure 原字符串）。OAuth 可更新 token/client，WebDAV 可更新同一身份的 pass/Bearer；不得借此切换账号或存储位置。通用 WebDAV 无法独立证明两个 Bearer 属于同一账号，管理员必须在可信服务端确认；换账号应新建凭据并走明确迁移，不能当作密码轮换。

旧 revision 返回 412，目标变化返回 409；刷新详情后重新输入秘密，不自动重试替换。成功后保留加密历史并变为 UNTESTED。Crypt 和 WebDAV pass 的 obscure 只是一种配置编码，不提供持久化保密性；完整配置的保护来自 envelope encryption。

禁用保留密文历史，不删除云端数据；禁用后不能通过替换隐式启用。Agent 永远不持有这些中心凭据。

## 升级与验收

当前 schema 为 8。迁移 00008 只扩展 provider CHECK，不重写现有 OneDrive 数据、AAD、凭据版本或 Repository 绑定。用既有 migrator 升级后再启动新 Server；存在新后端记录时 Down 会失败，不会删除或改标它们。

CI 验证配置拒绝、metadata 隔离、权限/事务/CAS、Google refresh、WebDAV DNS/TLS/重定向/清理、Web UI 和双架构构建。真实 OneDrive/Google OAuth 刷新及三种后端的写入、备份、恢复、认证失效和重启恢复仍 MUST 在持有测试凭据的安全环境验收；离线测试不替代真实服务兼容性。

下一种 rclone 后端按字段允许列表、网络策略和正负测试接入，不直接接受任意后端配置。决策见 [ADR-0012](adr/0012-rclone-multiple-backends.md)。

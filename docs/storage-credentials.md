# 中心存储凭据（M4 第一批）

此版本提供配置导入、只读 metadata、替换和禁用。尚未提供远端连接测试、仓库初始化或备份。所有新导入/替换的凭据显示“未测试”。

## 准备与导入

1. 按部署文档配置中心 master key、TLS 与 Agent enrollment。沿用已有 master key，MUST NOT 为新凭据临时生成另一把 key。
2. 在可信设备运行 rclone config，完成 OneDrive 授权，并创建包装它的 Crypt remote。配置本身是秘密，MUST NOT 提交 Git、上传工单或粘贴到聊天中。
3. 以管理员登录 Console → 存储凭据 → 导入凭据。输入显示名称、Crypt remote 名称，以及仅包含这两个 remote 的配置。
4. 保存后确认状态“未测试”。输入框会清空，服务端不返回任何秘密；自行离线保管原始配置。

受限格式支持的键：

| 配置段 | 允许字段 |
|---|---|
| OneDrive | type=onedrive、token、drive_id、drive_type、client_id、client_secret、region=global |
| Crypt | type=crypt、remote、password、password2、filename_encryption=standard、directory_name_encryption=true |

OneDrive token MUST 包含 access_token、token_type=Bearer、refresh_token 与 RFC3339 expiry。支持 personal/business/documentLibrary drive type；custom endpoint、tenant-specific token_url 和其他 region 暂不接受。Crypt remote 只能指向配置内的 OneDrive remote，路径为规范相对路径；允许空根路径。密码使用 rclone 配置中已有的 obscure 值。

原始配置最大 256 KiB（按 UTF-8 bytes 计）。remote 名称 2–64 位、字母开头，只能含字母数字、下划线和连字符。不接受额外 remote、重复 INI key/section 或未知配置项。

## 替换和禁用

“替换凭据”用于更新 OAuth token/client 凭据。MUST 保留原始 remote、drive、路径、Crypt password/password2（包括其 obscure 字符串）与加密选项。服务端会验证这些字段，避免导致历史仓库不可读取。

多人同时编辑时，旧 revision 返回 412。点击“刷新详情”后重新输入秘密；前端不会自动重试替换。替换成功会保留加密历史版本并重新变为“未测试”。

“禁用凭据”保留加密历史，不触碰云端数据。禁用后不能直接替换或自动恢复启用。

## 数据库升级与验证

此批 schema 从 4 升为 5，增加 storage_credentials 和 storage_credential_revisions，复用 M2 secrets envelope。使用已有 migrator 升级，然后启动对应版本 Server；不要以未升级的数据库运行新版本。

验证项包括：ADMIN/VIEWER/CSRF 边界、配置注入/路径拒绝、错误 master key、AAD 绑定、并发替换、审计失败回滚、列表分页和浏览器清空秘密输入。编译、测试、race 与镜像构建由 GitHub CI 执行。

真实 OneDrive 连接与 token refresh 的验收将在后续 M4 runtime 交付后，在持有真实凭据的受控环境执行。

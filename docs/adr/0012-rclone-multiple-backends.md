# ADR-0012：显式支持多个 rclone 后端

用户将 V1 范围从仅 OneDrive 扩大至 rclone 多后端。本批 MUST 支持 OneDrive、Google Drive（rclone type=drive）和 HTTPS WebDAV；保留一个后端加一个 Crypt 的受限配置结构，不透传任意 rclone 选项。既有 RCLONE_ONEDRIVE 记录和信封 MUST 原样兼容，新增 RCLONE_GDRIVE、RCLONE_WEBDAV；provider 从已验证配置派生，不能由客户端另行声明。

Google Drive MUST 使用自有 OAuth client、可读写 scope（drive 或 drive.file），MUST 指定实际 root_folder_id 或 team_drive（不能使用 root 别名），防止更换 OAuth 账户后默认根目录指向另一位置；不接受服务账号文件、域委托、env_auth、自定义 OAuth 端点或远程命令。OAuth client_secret 按 rclone 原生字符串保存；ADR-0007 将 OAuth client_secret 视为 obscure 值的限制在此纠正，Crypt password 和 WebDAV pass 仍 MUST 为 obscure 配置编码，所有值的持久化保护仍是信封加密。WebDAV 支持 Basic 或静态 Bearer（两者互斥），vendor 仅 other、nextcloud、owncloud、fastmail、rclone；NTLM/SharePoint、bearer_token_command、headers、用户 unix_socket 和 auth_redirect 不开放。

WebDAV 是新的自定义网络目的地，MUST 仅接受 HTTPS，拒绝 URL 用户信息、query/fragment、异常路径与非公网 IP。每次 runtime 启动 MUST 解析并验证全部 DNS 地址，再将出口固定到已验证的 IP:port；通过服务 UID 私有 tmpfs 的 0600 Unix socket 透明转发 TLS，复用 rclone 的 unix_socket transport。MUST 不新增 TCP listener、不解密 TLS、不禁用证书校验；重定向不能选择另一个网络目的地，DNS 重绑定不能绕过校验。生成的 socket 参数仅进入临时配置，MUST 不进入密文版本/API，也不能被进程修改后接受。暂不开放内网/NAS；如需支持须新增部署级明确放行规则，而非 API 开关。

替换 MUST 保持 backend、目录/drive、WebDAV URL/vendor/user、认证方式和 Crypt key；仅更新 OAuth token/client 或相同 WebDAV 身份的 pass/bearer。watcher MUST 只接受 OneDrive/Google Drive 的 token 更新；WebDAV 没有 OAuth 自动刷新，不得因密码被子进程改写而增加版本。

配置导入、记录创建、读取测试和真正的备份可用性 MUST 分开表达；三类后端的云端写入、备份/恢复、OAuth refresh 及真实服务差异仍需安全集成环境验收。下一种 rclone 后端通过新增显式校验与测试接入，不引入独立驱动框架。

依据：[rclone WebDAV](https://rclone.org/webdav/)、[Google Drive](https://rclone.org/drive/)、[固定版本 WebDAV transport](https://github.com/rclone/rclone/blob/v1.75.1/backend/webdav/webdav.go)、[固定版本 Unix socket 客户端](https://github.com/rclone/rclone/blob/v1.75.1/fs/fshttp/http.go)。

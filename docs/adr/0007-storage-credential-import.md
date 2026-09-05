# ADR-0007：受限配置导入与不可变密文版本

- Status: Accepted
- Date: 2026-09-05
- Milestone: M4，第一批凭据管理

## 决策

中心凭据 API MUST 仅接受一个 OneDrive remote 与一个直接引用它的 Crypt remote。输入使用受限 INI 子集，并在解析后按允许字段重新生成配置。不得直接将原始配置传给 rclone。

第一批 MUST 支持默认 global Microsoft 端点；自定义 token/auth endpoint、其他 region、其他 backend、remote 链、环境变量展开、路径穿越、重复 INI 配置段/键与未知选项 MUST 被拒绝。配置上限为 256 KiB UTF-8。remote 名称限定为 2–64 位 ASCII 字母、数字、下划线、连字符且以字母开头。

Crypt MUST 使用 standard 文件名加密和目录名加密；省略这两个选项时规范化为安全默认值。password/password2 和自定义 client_secret MUST 是 rclone 已 obscure 的配置值；obscure 只作为配置编码，数据库保护 MUST 使用现有 envelope encryption。

每次导入/替换 MUST 新增不可变 secrets 记录，并在同一事务写入 storage_credentials、storage_credential_revisions 与 AuditEvent。AAD MUST 绑定 namespace、secret type、credential ID、secret revision、secret ID 和 master key version。普通 metadata 查询 MUST NOT join 或解密 secrets。

替换 MUST 使用 If-Match，并保持 remote 名称、drive、路径、region 和 Crypt 设置相同；仅允许变更 token、client_id 和 client_secret。Crypt obscure 字符串也 MUST 保持原值，重新 obscure 后的不同字符串会被保守拒绝。更换存储目标或 Crypt 密码属于迁移，不能伪装为 token 更新。

读取旧明文以校验替换前 MUST 先写 secret access 审计；写入审计不可用时 MUST 事务回滚。服务启动沿用 M2 的 master key 和 CA 校验；错误 master key MUST 阻止启用 enrollment/凭据管理。Agent 协议不增加任何中心凭据字段。

## API 与界面

提供 create、cursor list、detail、replace-secret、disable。修改仅允许 ADMIN，metadata 允许 ADMIN/VIEWER。公共 mutation 授权函数统一检查 ADMIN，以符合现有 API 角色矩阵。替换和禁用使用 revision CAS，冲突返回 412。

返回值 MUST 仅含名称、provider、remote_name、状态、版本和时间。导入与替换状态为 UNTESTED，不表示远端可用。配置文本 MUST NOT 放进 React state、localStorage 或响应缓存；表单提交、取消和 pagehide 时清空，失败后需重新输入。

禁用只改变中心 metadata，不删除配置历史或云端数据；禁用后不允许通过 replace-secret 隐式启用。

## 交付边界与后续

第一批不启动 rclone，不把秘密 materialize 到磁盘，也不开放 Gateway 或仓库创建。异步 Test Operation、tmpfs materialization、refresh CAS、Gateway 路径隔离、Repository provisioning 与 gateway rotation 在后续 M4 PR 交付。未完成 REP-001–012 / DEP-005/006 全部适用验收前 MUST NOT 将 M4 标记 COMPLETE。

后续 runtime MUST 复用这一配置校验入口，清理 rclone 子进程环境并明确指定配置文件；不得让环境变量覆盖已验证的网络端点。

## 依据

- [OneDrive 配置选项](https://rclone.org/onedrive/)
- [Crypt 配置选项](https://rclone.org/crypt/)
- [安全模型](../spec/02-security-model.md)

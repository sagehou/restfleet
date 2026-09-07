# ADR-0011：仓库归属与初始化分开提交

Repository 创建 MUST 仅在 PostgreSQL 中原子保存 PROVISIONING 记录、两份独立随机凭据的加密信封、初始凭据版本与审计；云端初始化保留为后续独立的持久化 Operation/jobs。这样请求失败不会留下孤立密文，也不会将“记录保存成功”误认为“云端可用或 Agent 已接受”，代价是需要明确的初始化步骤。

每个未归档 Repository MUST 独占 Host，DISABLED/ERROR 仍保留归属；不能通过禁用或初始化失败自动释放唯一约束而产生第二个仓库。并发创建按 Host 行锁串行化，失败请求返回 409 HOST_REPOSITORY_EXISTS；网络结果不确定时读取列表确认，不自动创建另一套身份。

Gateway 身份 MUST 由中心生成 UUIDv7，路径 MUST 固定绑定身份与 Repository；Gateway/Restic 密码分别生成 256-bit 随机值，复用现有信封加密并通过 AAD 绑定 Host、Repository、种类、revision、secret ID 与 master key ID。API 仅返回 metadata 和 revision，不暴露身份、路径、secret 引用或秘密内容；凭据版本存在不构成 Agent ACK。

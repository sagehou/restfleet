# ADR-0013：中心初始化任务与可用状态分离

初始化 MUST 使用独立 `REPOSITORY_INITIALIZE` Operation，复用现有 jobs、幂等记录、状态事件、outbox、审计和凭据 refresh CAS。POST initialize MUST 只接受 ADMIN、CSRF 与 Idempotency-Key，不接受请求体或执行参数。成功只表示中心验证了 Restic v2、仓库身份与空 snapshots；Repository MUST 保持 PROVISIONING，MUST NOT 伪造 Gateway 可用性或 Agent ACK。

同一存储凭据的测试与初始化 MUST 串行，以保留当前 refresh CAS 的单写入者语义；这是 V1 的有意吞吐限制，不增加独立队列服务。初始化 MUST 取得 repository_leases 中的排他维护租约，并拒绝任何有效备份/维护租约。租约绑定 job owner、Operation 与 Repository，续租和提交 MUST 同时验证 job 与 repository fence，审计等待后再次验证；失去租约 MUST 取消并回收子进程。

V1 MUST 只运行一个中心 storage worker，复用 runtime 独占 tmpfs 锁与进程组回收；不得以多个 runtime 目录部署并行 Server。DB 租约不是云对象写入 fencing token，单实例进程所有权同样是安全前提。重启重领最多三次，MUST 复用已保存密码和路径，MUST NOT 自动 unlock、删除或重新生成凭据。失败保持 PROVISIONING 并通过 Operation 展示固定错误，管理员重试创建新 Operation；成功后不得重新 initialize，后续检查使用专门维护流程。

schema 9 MUST 仅增加初始化 metadata、Operation 关联与租约，并扩展固定枚举；旧密文和 AAD 不变。成功事务记录原生 Restic ID、format_version、initialized_at 与审计。初始化不更新凭据连接测试健康，也不向 Agent 下发任何新秘密。已有初始化记录时 Down MUST 拒绝丢弃历史，生产使用前向修复。

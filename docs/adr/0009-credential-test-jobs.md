# ADR 0009 — 持久化凭据连接测试与 refresh CAS

- 状态：Accepted
- 里程碑：M4

## 决策

Server MUST 在同一个进程启动中心 credential worker，使用 PostgreSQL jobs，而非内存队列或新增服务。POST test MUST 原子提交 Operation、Job、幂等记录、初始状态事件、outbox 和用户审计，再返回 202。未配置 runtime/master key 时 MUST 返回 503，MUST NOT 接受无法执行的任务。

test MUST 接受无 body 请求，要求 ADMIN、CSRF 和 1–128 字节可见 ASCII Idempotency-Key；不接受 query、重复 key header 或隐式运行参数。幂等 scope 绑定 actor、POST 与包含 credential UUID 的规范路径；只保存 key/body hash，至少保留 24 小时。同 key 返回同一 Operation 的当前 metadata，不新增任务。同一凭据最多有一个未终态测试，不同 key 的并行请求返回 409 CREDENTIAL_TEST_BUSY。

Operation 状态转换 MUST 统一验证 docs/spec/03-domain-model.md 的允许边，锁行后原子写事件和 outbox。中心本地 worker 领取时在一个事务内依次记录 DISPATCHED、ACKNOWLEDGED、RUNNING，表示本地接收和执行，不表示 Agent RPC。终态不可重开。

Job 使用 FOR UPDATE SKIP LOCKED，租约 30 秒、每 5 秒续租；worker shutdown 取消子进程并保留未完成租约供恢复。过期 RUNNING 的只读测试最多重领三次，保持原 Operation 身份并递增 attempt；耗尽后变为 TIMED_OUT/WORKER_LOST。租约、可领取时间与状态时间由 DB clock 决定。metadata 只在测试观察的 secret revision 仍匹配且凭据未禁用时更新。

## 秘密与事务边界

领取事务 MUST 先记录 STORAGE_SECRET_ACCESS，再允许读取和解密 envelope。worker MUST 复用 ADR-0007/0008 的配置白名单、AAD 与 tmpfs runtime。

refresh MUST 验证仅 OAuth token 变化，并在 job owner/lease 与 secret revision CAS 双重约束下新增不可变 envelope/history，原子更新 credential secret revision、Operation 所观察 revision、last_refreshed_at 和审计。后续恢复 MUST 从最新密文版本重新 materialize。失败 MUST 不提交任何新 secret 或 metadata。

事务锁顺序 MUST 是 job → operation → credential → audit。refresh 和完成 MUST 在审计写入后再次检查 lease，防止锁等待期间过期的 worker 提交。管理员替换/禁用后，旧任务 MUST NOT 覆盖密文或恢复 HEALTHY；其结果为 CREDENTIAL_CHANGED/CREDENTIAL_DISABLED。禁用立即阻止新测试；已有只读测试可结束，但结果失效，不能再次启用凭据。

API/DB/audit MUST 只保留固定错误码，不接受 provider 或数据库原始错误。连接测试的成功只证明 Crypt 根目录可读取；不证明写权限、仓库已初始化、Gateway 隔离或 append-only。

## Schema 与部署

迁移 00006 创建 Operation/events/jobs/idempotency 表，只开放当前 CREDENTIAL_TEST 类型；其他 Operation 的资源字段、类型和派发方式随相应里程碑扩展，不能提前启用未实现命令。StorageCredential 新增最近测试 Operation ID、时间/结果和最近 refresh 时间。

Server 的 /run/restfleet/credentials MUST 是已存在、服务 UID 拥有的 0700 tmpfs。Compose 挂载独立 16 MiB tmpfs，noexec/nosuid/nodev；native Server 必须由部署工具预建相同目录。路径或 binary 不安全时启动失败。shutdown MUST 等 worker 退出后再关闭 runtime 和 DB。

Web 从 last_test_operation_id 恢复轮询，不缓存 secret；网络错误后的手动重试复用同一幂等 key。只展示固定状态/错误码与时间。失效的旧测试不能代替新配置测试。

## 验证

- API → PostgreSQL → runtime fake executable → watcher → encrypted CAS → metadata 集成链路。
- 并发幂等、重复领取、续租/过期/接管、旧 owner 写入拒绝、重领耗尽。
- token 加密持久化、运行中替换/禁用、目标篡改拒绝、审计失败回滚。
- shutdown/restart 恢复、状态机/事件/outbox、匿名/VIEWER/CSRF 和 secret canary。
- Web 提交、轮询恢复、错误状态和模糊网络失败后的同 key 重试。

真实 OneDrive refresh 仍须安全环境人工验证；本批不完成 Gateway/Repository provisioning/rotation，M4 MUST 保持进行中。

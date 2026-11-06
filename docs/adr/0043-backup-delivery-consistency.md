# 备份配置发布与健康状态保持一致

Plan 变更、DesiredState、outbox 和审计 MUST 在同一个 PostgreSQL 事务提交，按 Agent、Plan 的顺序加锁；状态变更不增加配置 revision，但提交时 MUST 重验读取时的状态，防止陈旧请求覆盖后续暂停。重连 MUST 重报当前已接受配置的 revision/hash，中心据原快照幂等对账，不能把传输发送成功视为中心已经保存。

schema 29 和 REST Plan 契约新增 `backup_health=WARNING`，表示最近相关备份生成快照但带告警；它维持成功时间，同时计入 Dashboard 的不健康计划，避免把 exit 3 伪装成完整成功。暂停恢复时从最近健康相关结果重建，取消及并发跳过不清除既有告警。向旧 schema 降级时若仍有 WARNING 行 MUST 拒绝，不能静默改为 HEALTHY；客户端 MUST 接受新增枚举或对未知健康值显示未知。

已接受手动任务 MUST 使用 Agent 服务生命周期，并在本地落盘后、发送接受消息前注册执行者；控制流重连/续证不等于用户取消。正常调度允许一个轮询周期加 1s 的唤醒误差；`misfire_grace_seconds=0` 禁止停机或超出该误差后的补跑，不跳过正常到点。静态程序内嵌 IANA 时区，API、Agent 激活和执行共用规范路径校验。

本决定沿用现有数据库、任务和 mTLS 协议，不新增队列服务或改变 Gateway fence、授权期限、Host 隔离与可信清理。Web 使用资源 ID 轮询恢复当前完整状态，不承诺事件历史重放；#96 的 API-006 按该行为验收，SSE 保留为可选后续实现。

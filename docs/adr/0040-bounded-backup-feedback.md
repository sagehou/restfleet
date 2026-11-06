# 有界备份事件与 HTTP 操作幂等

Agent 的日志只投影为固定 section/level/code、UTC 时间和非负计数；不保留原始 stdout/stderr、路径、命令或任意错误字符串。服务端再次严格解码和白名单校验，采用同一词汇，避免依赖正则遗漏秘密。`backup_logs_v1` 增量能力传输每块至多 32KiB / 200 项的连续事件；中心以已提交的最高连续序列 ACK，Agent 才删除缓冲。新增日志只追加，重复序列保留首次提交内容。

每次 Operation 固定首次写入时接受的日志预算（默认 10MiB，最低 64KiB），ACK 不重置累计预算；保留 16KiB 给有限词汇的首尾、Hook、错误和摘要，超限丢弃后续进度并保存唯一 truncation marker。这样同时限制离线缓冲和中心累计存储，无需引入原始日志归档或额外服务。终态先确认结果再补日志；日志不改变 Operation 终态或证明 Gateway 清理。

schema 28 同时保存 attempt、完整 summary、单调健康时间以及日志 head/entries。手动 backup、retry、cancel 复用事务式幂等模式，另建引用 backup_operations 的请求记录以保留原 operations 外键约束。以 actor/method/具体 route 为 scope，hash key/body，保存 24h 响应快照；同请求精确重放，同 key 不同 body 拒绝。业务、副作用请求记录与审计同事务。取消原因只持久化固定 USER_CANCELED，任意输入仅用于请求 hash，不进入日志、响应或 Agent。

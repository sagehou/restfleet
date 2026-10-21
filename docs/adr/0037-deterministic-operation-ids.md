# ADR-0037：定时任务使用确定性 UUIDv5 操作 ID

状态：已接受；实现与 Actions 验证中。

## 背景

Agent 本地调度必须在同一 `(plan_id, scheduled_at)` 上生成稳定操作键，重启、重试和重复触发不能创建第二个 Operation。当前实现使用 UUIDv5（`uuid.NewSHA1`），但中心与 Gateway 只接受 UUIDv7，导致定时任务在 Restic 启动前失败、终态上报触发断流重连。

## 决定

- Agent 定时任务的操作 ID 使用 UUIDv5，输入为 `plan_id + "|" + scheduled_at(UTC, RFC3339)`；该 ID 是确定性本地键，不是秘密。
- 中心生成的手动操作、Host、Agent、Repository、Plan、Snapshot 等其他公共 ID 继续使用 UUIDv7。
- Server、Agent 与 Gateway 的 `operation_id` 字段接受规范 UUIDv5 或 UUIDv7；其他绑定 ID（Host/Repository/Gateway/Agent/Plan/Runtime）仍 MUST 为 UUIDv7。
- Gateway 只把 `operation_id` 用作请求去重和会话路由键，不据此授予身份、权限或清理证明。UUIDv5 可预测不构成安全问题，因为这些值不参与认证。
- 协议、OpenAPI 与数据库说明 MUST 明确这是 `operation_id` 的唯一例外，并保留现有 UUIDv7 行为。

## 被否方案

- **全部改用 UUIDv7**：会丢失同一 scheduled time 的确定性去重；需要在 Agent 本地额外持久化映射表，增加崩溃恢复复杂度。
- **只放宽 Server、不改 Gateway**：定时任务仍无法开启数据面会话，不能修复端到端链路。
- **引入新的操作键类型**：会改变协议字段和数据库主键，超出当前最小修复范围。

## 兼容性与验收

现有 UUIDv7 手动操作不受影响；旧 Agent 继续使用其原有路径。新增正向测试覆盖 UUIDv5 的进度/结果上报与 Gateway 会话；负向测试继续拒绝 UUIDv4、nil 和跨绑定 ID。GitHub Actions 必须覆盖 Server、Agent 与 Gateway 三处校验。

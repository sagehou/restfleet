# 备份执行配置通过增量能力进入 Agent

Template 的 opaque retry/resource 对象改为明确且有边界的结构，保留原 JSON 键；PlanSchedule 用可选 `execution` 进入 canonical hash，并通过 `backup_execution_v1` 的有界严格 JSON 扩展传输。这样无需在多层协议复制配置模型，未配置扩展的旧快照保持 hash 兼容；旧 Agent 面对新配置时明确报兼容错误并保留旧配置，不能接受后忽略。

Hook 只能引用本机受保护 allowlist 中的 UUID，中心不能下发 executable、参数或 shell。执行采用 argv、独立 timeout 和进程组取消，只记录固定结果，避免把任意 Hook 输出作为秘密通道。

自动重试采用持久的新 Operation 与 parent/root/attempt，在指数退避期间保留原 I/O 槽位及有界 deadline，减少新的调度状态机。重启将未结束 attempt 标成 LOST，结果按 parent ACK 顺序补报。暂态分类不能从任意 stderr 中猜测；密码、证书、配置和路径错误不能自动重试。本决定不改变 Gateway 授权、fence、云凭据单写者或中心维护边界。

# 备份重试只采用可信的暂态故障分类

Restic 的任意 stderr 无法可靠区分网络、后端限流与权限故障，因此不解析其错误文本来决定重跑。Gateway 在既有会话 API 增加可选 `Restfleet-Backup-Retry` 响应头，只返回固定的 `GATEWAY_UNAVAILABLE` 或 `GATEWAY_RATE_LIMITED`：POST 的 503/429 仅在本请求未进入会话准入时可携带；DELETE 的 204 仅在请求、进程、材料与审计收尾完成后，可返回本会话观察到的只读后端暂态错误。未知/权限故障、写入或删除不确定及已提交 snapshot 抑制该提示，后端错误文本、路径、header 和云端材料均不转发。读请求的 500/502/503/504 归为不可用，429 归为限流；rclone 将 provider 错误折叠时不声称识别了具体 provider 原因。

Agent 仅在已验证 TLS、当前绑定路径和上述状态组合下接受提示。首次 POST 明确 dial 失败且尚未建立连接可按暂态网络错误重试；其他通信不确定仍 MUST 以原 Operation ID 清理，未获 204 MUST 报告清理不确定，不重跑。已执行 Restic 时，只有 exit 1、没有 summary/snapshot/源读取错误且清理确认后才可采用提示；成功、exit 3、密码/锁/未知退出码、取消和超时保留原分类。每次自动重试仍使用新 Operation、原 parent/root、Plan 次数/退避、原 deadline 和下一调度窗口；不新增持久 schema，不释放占用、不延长授权、不恢复锁删除权。旧 Gateway 的数据路径失败无提示时保留原保守失败，旧 Agent 可忽略此增量头。

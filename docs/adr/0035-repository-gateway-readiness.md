# 仓库可用性复用 Gateway 后端观察

## 问题与决定

中心初始化和 Agent durable ACK 已经存在，但它们不能证明 Gateway 实际读到了仓库。生产 Gateway MUST 在首次后台探测及每次真实会话的现有 `HEAD config` 成功后，通过原仓库来源的签名加密 Queue 记录固定 `backend_ready/verified` 事件；不增加身份、密码、wire 字段、schema 或服务依赖。观察仅证明相应时刻的后端读取和有效本地授权，不证明备份完成、外部网络永远可达或清理完成。

后台 SHOULD 每 5min 开始一轮，串行探测每仓库，单仓库最多 30s，HEAD 继续最多 15s。空闲时复用原 owner/runtime、append-only 后端、材料 watcher 和所有清理边界；只调用 HEAD，不写入对象或初始化仓库。探测短暂占用该仓库时，新的 Agent 会话 MAY 返回已有 409 忙碌语义，不创建/消耗 operation 去重记录。全局容量不足 MUST 跳过本轮，不能仅因探测竞争永久失败原 owner。已有会话 MUST 复用其后端，加入原 session 的取消/join 生命周期，不能创建第二个进程或借旧 session 恢复访问。真实后端、材料/刷新或审计失败仍遵守现有失败停止规则。

生产 MUST 在全部初始化、TLS 材料核验和公网绑定后启用后台观察；证书过期或尚未生效 MUST 停止运行，不能继续产生健康证据。后台探测 MUST 与其他 worker 一起取消/join；只有探测/回调也退出后才 MAY 生成原终结清理记录。嵌入式 `StartService` 保持原审计契约，不隐式启动该生产观察者。

探测忙碌的 409 MUST 携带 `Retry-After: 1`；其他冲突不携带。Agent 只对这个明确未创建会话的响应在 30s 内原 ID/原 deadline 重试，任务取消或到期停止；MUST NOT 将参数冲突、其他响应或不确定请求当作忙碌重试。无需建立新 operation 或发送无会话的 DELETE，原不确定接收清理保持。

中心 MUST 沿原 RFGR 核验注册来源、签名/解密、完整 binding、顺序和时间，并将 scoped event 的 Host/Repository/Gateway 与 header 逐项比对。`GATEWAY_BACKEND_READY` 只 MAY 来自这些已提交的仓库记录；全局来源、Agent ACK、材料或 RFGA 回执不能替代。用于可用性判断的观察 MUST 发生在它引用的普通 grant 有效期内；过期/撤销后的历史回放继续保存审计，但不能借此恢复授权。

## 状态读模型

仓库 list/detail 的 `status` MUST 是中心配置状态与现有已认证观察的读模型；不新增持久化状态表，不做无审计的任意状态写入。LOCKED、DISABLED、ERROR MUST 保持配置优先级。对其他状态：

- 从未收到有效后端观察时保持 PROVISIONING；仅有初始化/ACK 不可 READY，不能相信没有证据的旧持久 READY。
- READY MUST 同时满足初始化、当前 Agent durable ACK、Agent/Host/Repository/StorageCredential 可用、当前 delivery/秘密引用及中心配置指纹完全匹配、原占用未释放/未到期、来源未封存，以及该 runtime 最新决定未撤回且未到期。
- 最新后端观察 MUST 不晚于当前时间，距今小于 10min。既有观察过期、授权撤回/到期、封存/释放、身份/凭据禁用或绑定改变时 MUST 显示 DEGRADED；新运行须提供自己的新观察。
- 恢复通信只允许按序重放原观察。历史观察已过 10min 时不能重新显示 READY；只有尚有效的当前运行实际完成新探测后才可恢复可用显示。显示变化不改变配置 revision/updated_at，不执行任何授权、续期、封存、释放或备份结果转换。

查询 MUST 批量读取一页仓库的原 replay/audit/authority metadata，不解密秘密、执行云端探测或改变权威历史。Web MUST 解释 READY 的近期探测边界，保留初始化、Agent ACK 和备份成功的区别。固定离线窗口仍按 ADR-0032 控制；中心 UI 不可用不阻止有效授权内的备份，10min 显示期限不缩短或延长授权。

## 兼容与验证

OpenAPI 保留原字段和枚举，明确 status 的读模型及配置 revision 语义。事件沿原 audit kind/签名域；MUST 先升级中心再升级 Gateway，旧 reader 拒绝未知事件并保留未确认记录。原历史回放、授权、锁归属、不可覆盖/删除、身份续证/吊销和清理证明要求保持。

按用户指示暂不新增/改写测试用例。既有 Actions 的编译、lint/vet/race、Web 和负向套件仍 MUST 通过；新的生产周期探测、读模型、过期/撤销降级和并发取消/join 尚待专门运行验收，绿色旧套件不能替代。

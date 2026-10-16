# Gateway 启动交接

本流程用于部署环境或 GitHub Actions。开发工作区 MUST NOT 执行这些命令。它接通当前 M4 的启动入口，不代表完整自动备份已经验收；M5/M6 的调度和 Agent Restic 执行仍需完成。

Agent 只在首次配对使用临时 token，此后复用本机密钥和 mTLS。下面的 ID 和文件只管理中心与 Gateway 的运行交接，不增加 Agent 密码或要求重新配对。

## 准备已有资源

中心 MUST 已完成数据库迁移、仓库初始化和当前 Agent 凭据确认，并配置 Gateway origin、Agent CA、中心签名/回写密钥及 RFGR listener。中心和 Gateway 使用不同非 root UID 和专用 IPC 组。

部署者 MUST 为本次新运行独立配置全局及仓库来源、公钥 pin、runtime UUIDv7、TLS、Agent CA、持久 Queue 与 tmpfs runtime。私钥分别留在所属服务。原运行没有确认清理时，MUST 保留占用，不得通过改 ID 或清空目录接管。

文件格式和权限以部署规范 [§7.18](spec/09-deployment.md#718-中心单次初始化协调adr-0025)、[§7.22](spec/09-deployment.md#722-中心显式全局审计注册adr-0030)、[§7.23](spec/09-deployment.md#723-gateway-单次启动生命周期adr-0031) 为准。

## 执行交接

以中心服务 UID 准备原请求对应的占用：

```text
restfleet-server gateway-prepare --config-file /var/lib/restfleet/gateway-startup/request.json --plan-file /var/lib/restfleet/gateway-startup/plan.json
```

plan 先持久保存固定 ID，再提交占用。提交或响应不确定时，只能沿用原文件重试；绑定变化、已释放或到期占用会被拒绝。部署者将 plan 中的 `startup.binding` 安装到 Gateway 本地配置，保留两边独立私有目录。

以中心服务 UID 注册全局来源，取得公开的接收公钥：

```text
restfleet-server gateway-audit-register --config-file /var/lib/restfleet/gateway-audit/registration.json
```

将公开结果的 `audit_origin`、`recipient_public` 安装到 Gateway 本地配置；来源 seed 与独立中心签名 pin 仍按各自可信配置装载。公开结果不能作为替换签名 pin 的依据。

中心普通 Server 继续提供 API、数据库协调与 RFGR 回写；本次选择 follow 时，MUST 不为该 admission 启用另一个授权 worker，且该命令环境中的 `RESTFLEET_GATEWAY_AUTHORITY_FILE` MUST 留空。

以中心服务 UID 启动交接，显式等待最多 120 秒：

```text
restfleet-server gateway-start --config-file /var/lib/restfleet/gateway-startup/plan.json --audit-config-file /var/lib/restfleet/gateway-audit/registration.json --wait-seconds 120 --follow
```

在等待窗口内，以 Gateway 服务 UID 启动已经预置的新运行：

```text
restfleet-gateway run --config-file /var/lib/restfleet-gateway/metadata/service.json
```

中心先核验相同 runtime 的独立全局来源，等待材料 socket 出现，再执行一次经过来源/UID 认证的初始化。Gateway 只有完成全部初始化才开放公网；中心收到精确回执后直接进入原授权 worker，保持运行直到收到停止信号。worker 不重新初始化，不重发材料。失联期间 Gateway 仍按原有限授权工作；follow 停止本身不吊销 Agent，也不证明 Gateway 已清理。

不使用 follow 时，可以保留原一次性 `gateway-start`，初始化完成后再让普通 Server 以原 plan 启用 authority-file worker。两种方式 MUST 只选一个。多仓库 Gateway 仍逐一初始化，全部完成后使用既有多目标 manifest，不能对同一 admission 启动重复投递者。

生产 Gateway 初始化成功后立即开始串行只读后端探测，后续约每 5min 一轮；正常备份也复用原 HEAD 产生观察。中心收到原签名加密回写后，才结合当前初始化、Agent ACK、身份/配置/授权判断 READY；观察超过 10min 或当前条件失效则降级。空闲探测短暂占用仓库时，新备份请求可重试 409，不会消耗该 operation。该状态不证明备份完成，也不改变离线授权期限。必须先升级中心再升级 Gateway，旧中心不接受新观察事件；新流程仍待专门运行验收。

## 停止与失败

先给 Gateway 正常停止信号，允许其 join、最后刷新、清理和最终回写完成；只有中心验证终结记录后，原占用才释放。随后停止 follow。systemd 继续使用 `Restart=no` 和 `TimeoutStopSec=120s`。

启动、材料安装或回执失败时 MUST 保留 plan、Queue、来源和原占用，不能循环重跑初始化。中心授权投递进程中断后，可用普通 Server 的原 authority-file worker 精确恢复已提交决定的投递；禁止通过重新交付材料恢复失败 owner。Gateway 硬崩溃且没有清理证明时仍保持阻塞，受控恢复与期限前换运行尚待接通。Agent 配对和已接受配置始终保留。

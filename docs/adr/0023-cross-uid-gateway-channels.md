# Gateway 内部通道使用独立服务 UID 与专用共享组

## 背景

ADR-0020/0022 的 0700 目录、0600 Unix socket 只能支持同 UID，或让中心以 root 连接。独立 Gateway MUST 保持中心数据库、master key 和签名私钥隔离；生产服务不应依赖 root 运行。本批在已有 Linux 通道中增加显式共享组模式，保留默认私有模式和原签名/加密协议。

## 决定

- Server 与 Gateway SHOULD 使用两个固定、不同的非 root UID。管理员 MUST 建立专用非 root GID，仅授权这两个服务加入；该组不是身份认证，MUST NOT 用于私钥、待回写区、明文配置或数据库访问。
- 每个通道目录 MUST 由监听方拥有，mode 精确为 0710，GID 精确为配置的共享组。socket MUST 新建，监听方拥有，mode 精确为 0660、GID 精确匹配。组成员只能遍历已知路径和连接 socket，不能列目录、创建/删除文件或替换监听方 socket。MUST 拒绝 symlink、非规范路径、错误 owner/group、特殊权限及组/其他用户可写的通道目录。
- 共享组 MUST 显式配置在监听方和连接方，不得根据目录权限自动推断。省略或零 GID MUST 保持原 0700/0600 模式；MUST 拒绝多个 GID 和保留值。所有连接继续精确核验 SO_PEERCRED 的对端 UID，来源签名、中心签名及加密仍不可省略。未知同组用户可以消耗连接机会，但不能触发材料访问/回写效果；初始化被干扰后保留 fence，不自动重试或接管。
- 中心回放入口 MUST 仅在同时配置 replay socket、有效中心密钥、不同的非 root peer UID 与专用非 root GID 时开启共享模式。私有默认保持原行为。不增加 TCP/公共管理接口、schema 或新的 wire 版本。
- 部署 MUST 使用独立服务的 0700 私有目录/0600 文件保存各自密钥和队列，单独挂载中心 secrets；Gateway MUST NOT 挂载 DB/master/signing/中心解密私钥。管理员仍负责可信 binding、公钥注册与独立 pin 装载，不能从 Agent 请求取得信任。共享目录的祖先 MUST 由可信管理员维护，避免替换路径；该通道不授予生产启动器或 owner 恢复权限。

## 验证与边界

REP-041–042 的测试仅在 GitHub Actions 执行。新增 `gateway-isolation` job 编译 race-enabled 测试二进制，再以 root 测试协调器创建三个不同的非 root 子进程；它只负责 fixture 权限和进程启动。Gateway 在自身进程生成来源私钥，中心仅收到来源公钥；中心签名/接收私钥只通过匿名 stdin 管道交给中心子进程。测试包含加密初始化、可靠签名回放/确认、私钥不可跨服务读取、socket 不可替换及同组第三 UID 被拒绝。真实数据库的准入/单次事务由既有集成测试覆盖，不由此夹具证明。

`4c9285c` 的 [Actions](https://github.com/sagehou/restfleet/actions/runs/37170709515) 九项检查全通过，含上述跨 UID 验收；后续改动 MUST 以当前 head 的检查为准。生产 source/pin 装载、进程启动与协调、授权续期/吊销、全局离线审计、Agent 会话能力、可信清理/恢复、command/readiness、rotation/READY、真实云端及 AGT-005 仍未完成。

# Gateway 来源身份的本地创建与受保护信任装载

## 背景

ADR-0022/0023 已具备来源签名挑战、中心签名加密材料和不同非 root UID 的通道，但生产身份仍依赖测试注入。来源私钥 MUST 在 Gateway 本地生成；中心公钥 pin MUST 通过独立可信配置装载，不能取自初始化材料或连接方。

## 决定

- `restfleet-gateway source-init --source-key-file <absolute-path>` MUST 在已存在的 canonical、Gateway 服务所有、无特殊权限的 0700 私有持久目录生成新的 Ed25519 seed。命令 MUST 只输出标准 base64 的 32-byte 来源公钥；seed 以标准 base64 加换行保存在 0600 文件，不进入 argv、环境变量、错误或 stdout。
- 创建 MUST 拒绝已有目标，包括 symlink、损坏文件和旧身份，不做 load-or-create 或自动替换。新 `<name>.pending` MUST 排他创建；seed 文件 fsync、目录 fsync 后才通过原生 Link 发布，目标存在时 Link MUST 拒绝覆盖。移除 pending link 并完成最终目录 fsync 后才返回公钥。发布中有两个链接时，受保护读取 MUST 拒绝 Nlink=2。
- 失败/中断留下的 pending 或已发布但结果不确定的文件 MUST 保留，初始化重试 MUST NOT 重新生成或覆盖。唯一可回收例外为并发创建已完成时，本次仍为空且从未写入私钥的排他 reservation；它不是崩溃恢复证据。可信管理员 MUST 明确审查不确定结果，不能自动删除文件来接管旧运行绑定或释放占用。
- `source-public` MUST 只读取已有受保护 seed 并导出对应公钥，不创建或修复文件。导出公钥不证明先前不确定的 fsync/进程清理，也不授予绑定、占用或数据面权限。
- `LoadGatewayTrust` / `NewMaterialReceiverFromFiles` MUST 从两个独立的受保护文件装载来源 seed 与中心签名公钥 pin；两者均为 canonical、服务所有 0700 私有目录内的 0400/0600 regular file，无 symlink/hardlink/特殊权限，base64 解码后精确 32 bytes。pin 的内容 MUST 由可信管理员独立取得并配置；MUST NOT 传入中心 seed 或把来源 seed 文件当作 pin。来源加载不生成替代身份，不忽略 pending 痕迹；receiver 复制完成后，临时来源私钥副本 MUST 清零。
- 复用受保护密钥 reader，补充特殊权限拒绝，并用可清零 byte buffer 解码，避免把私钥复制为不可变字符串。祖先路径仍 MUST 由可信管理员维护。私有密钥目录 MUST 与 IPC 共享目录和待回写 Queue 分开。

## 验证与边界

REP-043–044 的单元、race、命令及跨 UID 验收 MUST 只在 GitHub Actions 执行。覆盖并发只有一个成功身份、三处 fsync 故障保留证据、旧身份/不安全权限/symlink/hardlink/pending 拒绝、公钥输出与固定错误。跨 UID 测试改为 Gateway 自身创建和装载真实受保护 seed，可信测试协调器单独安装中心公钥 pin；中心仍只取得来源公钥，第三 UID 和中心均不能读取 Gateway 私有文件。

本批不增加 schema、wire 或公共 HTTP/gRPC API。完整可信 binding 配置、中心来源注册接线、独立启动协调、续期/吊销、全局离线审计、Agent 会话能力、可信恢复、数据面 command/readiness、rotation/READY、真实云端与 AGT-005 仍未完成。来源身份存在或命令成功 MUST NOT 被描述为 Repository READY。

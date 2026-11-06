# 参与开发

开始修改前 MUST 阅读 AGENTS.md、安全模型、受影响子系统规范和[开发计划](IMPLEMENTATION_PLAN.md)中唯一 ACTIVE 的阶段。当前三个交付阶段就是 Milestone；旧 M0–M12 仅保留范围与验收追溯，不再逐个排队执行。

阶段内 MUST 连续完成 Server、Gateway、Agent、Web、契约和验证所需改动。一个阶段 MAY 包含多个可审阅 PR，内部组件、单个修复和中间检查点不另建阶段或 issue，不要求用户逐步批准后才继续。

编译、测试、认证及其他运行验证 MUST 仅在 GitHub Actions 中执行；开发工作区 MUST 只用于源码/文档编辑和只读检查，MUST NOT 启动数据库、应用服务、测试进程或本地编译工具链。以下质量门禁由 `.github/workflows/ci.yml` 执行，不是本地提交前命令：

~~~sh
make generate
make lint
make test
make build cross-build
~~~

涉及 API、数据库、Agent 协议或安全边界的变更 MUST 同步更新契约、迁移、规范/必要 ADR 及对应正负测试；这些工作属于同一交付，不单独开基础原语阶段。历史批次的测试暂缓记录不作为当前豁免。

每阶段只有一个主 issue。普通 PR 用 `Refs #阶段主issue` 关联，并说明具体用户行为、影响范围、原 Acceptance IDs、Actions 证据和未验证项；只有满足阶段退出条件的收尾 PR 才使用 `Closes #阶段主issue`。源码合入、旧 CI 绿色和专项验收完成 MUST 分开描述。跟踪合并使用 not_planned 并注明承接 issue，不表示功能完成。

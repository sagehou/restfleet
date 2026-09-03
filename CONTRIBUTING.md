# 参与开发

开始修改前 MUST 阅读 AGENTS.md、安全模型、受影响子系统规范和实施计划中标记为 ACTIVE 的里程碑。

本仓库一次只实施一个 Milestone。提交前运行：

~~~sh
make generate
make lint
make test
make build cross-build
~~~

涉及 API、数据库、Agent 协议或安全边界的变更 MUST 同步更新契约、迁移、规范及对应验收测试。

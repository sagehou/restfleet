# ADR-0005: Agent 证书使用固定 URI SAN 身份

- Status: Accepted
- Date: 2026-09-04

## Context

Agent 的授权身份必须来自通过验证的 mTLS client certificate，不能来自 hostname、CSR 自报 SAN 或 gRPC payload。证书中的 Agent ID 还需要稳定、无歧义且可由不同实现解析。

## Decision

V1 由 Server 为每个 Agent certificate 构造以下身份：

- Subject Common Name MUST 是规范化的小写 Agent UUID；
- 证书 MUST 恰好包含一个 URI SAN：`urn:restfleet:agent:<agent_uuid>`；
- Server MUST 忽略 CSR 请求的 Subject 与 SAN，只采用已验证的 Ed25519 public key；
- 每次连接 MUST 同时验证证书链、有效期、URI SAN、Subject 一致性、serial 与数据库中的 Agent/certificate 状态；
- Agent payload MUST NOT 提供可用于授权的 `agent_id` 或 `host_id`。

## Consequences

- 证书身份提取不依赖可变 hostname；
- 伪造 CSR SAN 不会扩大权限；
- 已撤销 Agent 即使持有尚未到期的有效签名证书，也会在数据库授权门被拒绝；
- 其他语言实现必须严格采用同一 URI 格式；
- 将来改变 URI 格式属于协议兼容性变更，必须通过新协议版本和迁移策略实施。

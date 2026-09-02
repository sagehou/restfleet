# AGENTS.md

This file defines repository-wide implementation constraints for human and AI contributors. The normative product and technical specifications live in `docs/spec/`.

## Read first

Before changing code, read:

1. `docs/spec/02-security-model.md`
2. the specification for the affected subsystem
3. the active milestone in `IMPLEMENTATION_PLAN.md`

If an implementation conflicts with a specification, update the specification in the same change or stop and request a decision. Do not silently invent a different security model.

## Non-negotiable security invariants

- An Agent MUST NOT receive rclone configuration, cloud-storage credentials, OneDrive OAuth tokens, rclone crypt passwords, the server master key, or maintenance credentials.
- An Agent MUST NOT be able to delete or overwrite existing repository objects.
- An Agent necessarily has read-and-create access to its own Restic repository; do not describe append-only as write-only.
- V1 MUST default to one Repository per Host, with a distinct Restic repository password and gateway identity per Host.
- A compromised Host MUST NOT expose another Host's repository contents or credentials.
- Repository `forget`, `prune`, `check`, `repair`, key management, migration, and `unlock` MUST execute only in the central Maintenance Worker.
- Agent private keys MUST be generated locally and MUST NOT leave the Agent host.
- Enrollment tokens MUST be random, single-use, short-lived, stored only as hashes, and consumed atomically.
- The Agent control connection MUST be outbound from Agent to Server and authenticated with mTLS after enrollment.
- Backup traffic MUST NOT proxy through the Control API or gRPC Agent channel.
- Scheduled backups MUST continue from the last accepted configuration while the Control Plane is unavailable.
- Secrets MUST NOT appear in logs, process arguments, API responses, audit payloads, metrics, or error messages.
- Restic passwords and gateway passwords MUST be supplied through protected files or child-process environment, never CLI arguments.
- TLS verification MUST NOT be disabled in production paths. `--insecure-tls` is forbidden outside explicit tests.
- User-supplied paths MUST be canonicalized and validated. Snapshot paths MUST NOT be interpolated into a shell command.
- The Server MUST execute Restic and rclone with argv APIs, never through `sh -c`.
- Restore MUST default to a staging directory. In-place restore is out of V1 scope.
- Central maintenance MUST be serialized per Repository and coordinated against active Agent backup leases.
- Audit events MUST be written for enrollment, credential changes, plan changes, restore/download actions, maintenance, login, and authorization failures.

## Architecture rules

- Keep domain logic independent from HTTP, gRPC, SQL, and subprocess adapters.
- Use a modular monolith for V1: one Server binary may host APIs and workers, but component boundaries in `01-architecture.md` must remain explicit.
- PostgreSQL is the authoritative control-plane store. Do not add Redis, NATS, Kafka, or another required service in V1.
- Use a transactional outbox / jobs table for durable dispatch. Do not rely on in-memory queues for authoritative jobs.
- The Agent uses bbolt for local durable state and must build with `CGO_ENABLED=0` for `linux/amd64` and `linux/arm64`.
- REST API behavior is contract-first. Update OpenAPI and contract tests before or with handlers.
- gRPC messages are versioned protobuf contracts. Unknown fields and unknown enum values must fail safely or be ignored according to the protocol spec.
- All persisted timestamps are UTC. Schedules retain an explicit IANA timezone.
- Public IDs use UUIDv7. Restic snapshot IDs remain their native SHA-256 strings.
- Resource updates use optimistic concurrency (`revision` and `If-Match`) where specified.
- Operation state transitions must be validated centrally; direct arbitrary status writes are forbidden.
- Treat Restic JSON as forward-compatible: ignore unknown fields/message types, reject malformed required fields, and treat unknown exit codes as failure.

## Repository layout target

```text
cmd/
  restfleet-server/
  restfleet-gateway/
  restfleet-agent/
internal/
  server/
  agent/
  domain/
  persistence/
  restic/
  rclone/
  security/
api/
  openapi/
  proto/
web/
deploy/
  compose/
  systemd/
docs/
  spec/
  adr/
```

## Coding and testing

- Go code must pass `go test ./...`, `go vet ./...`, race tests for concurrency-sensitive packages, and the configured linter.
- Web code must pass type-checking, unit tests, linting, and production build.
- Every state transition, authorization boundary, secret-redaction path, and subprocess adapter needs tests.
- Use fake Restic/rclone executables for unit tests and pinned real binaries for integration tests.
- Integration tests must cover Control Plane outage, network interruption, duplicate delivery, process cancellation, repository lock contention, and partial Restic exit code `3`.
- Security-sensitive changes require negative tests showing the forbidden behavior is rejected.
- Never weaken an acceptance test to make an implementation pass without documenting the changed requirement.

## Scope discipline

- Implement one milestone at a time.
- Do not add Kubernetes, multi-tenancy, HA, arbitrary remote shell, Agent auto-update, or shared repositories to V1 unless the specification is intentionally revised.
- Prefer the smallest implementation that satisfies the current milestone and preserves future interfaces.
- Avoid speculative abstractions for storage providers not in V1. OneDrive through rclone is the required V1 backend, while adapter boundaries must remain testable.

## Documentation rules

- Keep user-facing docs in Chinese unless a protocol identifier or code term is clearer in English.
- Use RFC 2119 terms (`MUST`, `SHOULD`, `MAY`) for normative requirements.
- Add an ADR for decisions that change trust boundaries, persistent schemas, public APIs, deployment topology, or protocol compatibility.
- Do not commit credentials, generated certificates, local databases, runtime logs, or real hostnames.

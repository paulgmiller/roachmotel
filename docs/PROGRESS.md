# Implementation progress

## Implemented

- Revised plan, pins, renderer, and official three-node operator values.
- Retained PVCs, verified node TLS, worker distribution, infrastructure certificates.
- Protected namespace Database/User CRs and retained source credential Secrets.
- One generated 48-character password and full connection-string Secret per namespace.
- Namespace Secret cloning, non-root pod mounts, first-init authentication/grant gate,
  and an example application reading the mounted connection string.
- Verified TLS by default; explicit plaintext SQL preserves authentication/node TLS.
- Removed CSI dependency and application certificate issuance/RBAC policies.
- Platform-only database CR protection, CA bootstrap helper, printed install commands.

## Remaining before activation

1. Resolve upstream failed-initSQL retry defect.
2. Validate Kubernetes generation/Secret races, ownership, retained credentials,
   reconciliation order, file permissions, both transport modes, certificate renewal.
3. Verify three-node availability during a node outage.

Automatic password rotation is disabled for the single-user design.
No deployment, namespace activation, or blanket pod eviction was performed.

## Verification

- Eight renderer unit tests cover identity, retention, credentials, transport, and replicas.
- Real Kyverno mutation/reinvocation checks cover mounts, environment replacement,
  init ordering, exclusions, escaped Go templates, and privileged tenant SQL rejection.
- Nine custom resources validate against pinned schemas with unknown fields rejected.
- Local SQL checks cover password authentication, readiness, password-preserving
  idempotency, CRUD/DDL/new schemas, namespace isolation, and privilege denial.
  Both verified TLS and explicitly enabled plaintext SQL pass these checks.
- The official CockroachDB chart renders three secure nodes in both modes and
  includes --accept-sql-without-tls only when application TLS is disabled.

Offline CLI checks cannot establish runtime generation/discovery/RBAC. Kubernetes
integration remains required. SQL tests use temporary single-node processes;
deployment values always request three managed nodes.

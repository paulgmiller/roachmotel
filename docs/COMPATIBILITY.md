# Compatibility status

Candidate pins: CockroachDB/chart 26.2.3, CockroachDB operator 1.1.0, DB Provision
Operator 0.14.0, Kyverno/chart 1.19.1/3.9.1, cert-manager 1.21.2, approver-policy
0.28.0. CSI has been removed. These pins are not Kubernetes integration claims.

## Source-level findings

Database.initSQL supports failurePolicy: Block and database retention. SQL names
must be identifiers; Roachmotel uses deterministic namespace hashes.

DatabaseUser supports generated passwords and Secret templates. Its v0.14.0
controller writes/reads credentials in the CR namespace despite the API exposing
passwordSecret.secretNamespace. User CRs/source Secrets therefore stay in
infrastructure and Kyverno clones credentials into application namespaces.

The controller reads a templated Secret's password through a plaintext annotation.
Protect Secret metadata as confidential. If a source Secret is lost, it generates
and synchronizes a new password, so retain source Secrets to preserve credentials.
Bootstrap may create the SQL user first but must never reset its password.
User retention/protection use annotations, not Database-style spec fields.

DatabaseInstance uses spec.tls.enabled and spec.tls.mode. Administrator certificate
material remains restricted to infrastructure in both workload transport modes.

## Blocking retry defect

ApplyInitSQL skips unchanged SQL whenever its hash matches the saved hash, including
failed attempts. failurePolicy: Block does not implement required retries.
Transactional bootstrap prevents partial grants but cannot fix skipped retries.

Activation waits for an upstream fix and Kubernetes verification that transient
bootstrap failure recovers without SQL edits or status clearing. No fork or
replacement controller has been introduced.

Sources inspected at v0.14.0:

- https://github.com/panteparak/db-provision-operator/blob/v0.14.0/internal/features/database/handler.go
- https://github.com/panteparak/db-provision-operator/blob/v0.14.0/internal/features/user/controller.go
- https://github.com/panteparak/db-provision-operator/blob/v0.14.0/internal/secret/manager.go
- https://github.com/panteparak/db-provision-operator/blob/v0.14.0/internal/adapter/cockroachdb/user.go

## Kubernetes integration still required

- Password/template persistence across reconciliation order and controller restarts.
- Credential clone retries before source creation, update synchronization, and
  removal of invalid cross-namespace source owner references.
- Retained databases, users, source Secrets, passwords, and data on namespace recreation.
- Non-root mounts, injection, and authentication/grant readiness before migrations.
- Infrastructure-only approval and renewal; reject privileged certificate requests
  from application service accounts.
- Operator handling of --accept-sql-without-tls without disabling node TLS or auth.
- Three-node placement/failover and provisioner connections after certificate renewal.

Only platform administrators may edit infrastructure resources or policies.
Application namespace admins control their namespace's data/credentials.
Automatic password rotation is disabled; single-user rotation requires coordinated
SQL/Secret/application changes.

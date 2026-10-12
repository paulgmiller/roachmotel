# Compatibility status

The initial pins are CockroachDB/chart 26.2.3, CockroachDB operator 1.1.0,
DB Provision Operator 0.14.0, Kyverno/chart 1.19.1/3.9.1, cert-manager 1.21.2,
CSI driver 0.16.0, and approver-policy 0.28.0. Chart/API inspection is not a
claim that this combination has passed Kubernetes integration tests.

## Confirmed source-level findings

DB Provision Operator 0.14.0 includes `Database.initSQL`, `failurePolicy: Block`,
and explicit database retention. Its database CRD accepts SQL identifier names
only: namespace names cannot be used verbatim. Roachmotel maps them to `rm_`
followed by the first 32 hexadecimal SHA-256 characters.

`DatabaseUser` creation requires a nonempty password in
`internal/features/user/handler.go`. Roachmotel instead uses privileged initSQL to
create passwordless users; no per-namespace password Secret is distributed.

Its `DatabaseInstance` API has `spec.tls.enabled` and `spec.tls.mode`, not the
`spec.connection.sslMode` field shown in some upstream examples.

## Blocking retry defect

In `internal/features/database/handler.go`, `ApplyInitSQL` skips unchanged SQL
whenever its hash matches the saved hash, **including failed attempts**. Merely
setting `failurePolicy: Block` does not implement the retry semantics required
by PLAN.md. A SQL transaction prevents partial grants, but does not fix retries.

Automatic namespace provisioning must not be considered ready until an upstream
release fixes this and an integration test proves that transient bootstrap failure
recovers without an administrator modifying SQL or clearing status. No fork or
replacement controller has been introduced.

Sources inspected at tag `v0.14.0`:

- https://github.com/panteparak/db-provision-operator/blob/v0.14.0/internal/features/database/handler.go
- https://github.com/panteparak/db-provision-operator/blob/v0.14.0/internal/features/user/handler.go
- https://github.com/panteparak/db-provision-operator/blob/v0.14.0/charts/db-provision-operator/crds/dbops.dbprovision.io_databaseinstances.yaml

## Kubernetes integration still required

- Certificate approval: reject foreign namespace CNs, root/node/provisioner CNs,
  CA requests, DNS identities, excessive lifetimes, and extra subject identities.
- Fresh pods during namespace creation must wait through resource/issuer/RBAC
  races and successful database bootstrap.
- Verify issuer policy RBAC, CSI service-account token requests, renewal, and
  certificate files' group permissions in actual non-root pods.
- Verify retained resources and SQL data survive namespace deletion/recreation.
- Verify resource placement and SQL availability after losing one of three nodes.
- Verify platform controllers' renewal and database operator administrator
  connections after provisioner certificates rotate.

Only platform administrators may edit the infrastructure namespace, generated
databases/initSQL, policies, issuers, or approval RBAC. Application namespace
administrators remain responsible for their own namespace's data and credentials.
URI SANs identify pod requests diagnostically; they do not constitute per-pod SQL
authorization or revocation. Certificates copied out of pods remain usable until
expiry; deleting a pod is not immediate certificate revocation.

# Initial implementation

## Implemented

- Local Git repository and committed plan.
- Deterministic, validated configuration renderer and dependency pins.
- Official operator Helm values for three CockroachDB nodes, retained PVCs,
  verified TLS, locality, and distribution across worker nodes.
- Infrastructure issuer, restricted certificate approvals, and certificate-based
  database administrator connection.
- Namespace database generation in a protected namespace, retention, atomic SQL
  bootstrap, and conflicting database ownership rejection.
- Namespace-scoped certificate request/use RBAC and strict identity approval.
- CSI pod injection, non-root filesystem groups, server CA distribution,
  first-init database wait, PostgreSQL configuration, and repeatable mutation.
- Policy preventing tenant database CRs from submitting privileged SQL or
  cross-namespace provisioning requests.
- Explicit CA bootstrap helper, printed installation commands, and example app.
- Unit, real Kyverno mutation/validation, CRD schema, and local secure SQL tests.

## Remaining before activation

1. Resolve the pinned DB Provision Operator's failed-initSQL retry defect.
2. Validate all releases in a disposable Kubernetes cluster, including namespace
   generation, RBAC aggregation, certificate approval, bootstrap races, renewal,
   non-root file access, and retained data reattachment.
3. Test three-node availability during a node outage.
4. Pin an upstream release with correct bootstrap retry behavior after those tests.

Rendering and tests do not install into the current Kubernetes context. The local
secure SQL test uses one temporary process solely for SQL/certificate validation;
the deployment values always request three managed database nodes.

## Verification performed

- Seven unit tests pass.
- Pinned Kyverno 1.19.1 passes pod mutation/reinvocation and privileged tenant SQL
  rejection tests, including replacement of an existing Secret-based PGUSER value.
- Nine custom resources validate against pinned DB Provision, approver-policy,
  and rendered Kyverno CRDs, rejecting unknown fields.
- CockroachDB 26.2.3 passes local secure SQL tests for atomic/idempotent bootstrap,
  CRUD/DDL/new schemas, cross-namespace denial, privileged operation denial,
  certificate username enforcement, and conflicting database ownership rejection.
- Official CockroachDB, CockroachDB operator, DB Provision Operator, and Kyverno
  charts render using the generated values.

The offline Kyverno CLI cannot currently validate the full namespace generation
policy with its resource discovery context (it reports cluster-scoped generation
targets as requiring namespaces). CRD/schema validation passes; generation,
discovery, and authorization remain explicit Kubernetes integration requirements.

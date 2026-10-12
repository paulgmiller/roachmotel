# Roachmotel implementation plan

Deploy three persistent CockroachDB nodes using the official CockroachDB Kubernetes
operator. Compose existing controllers rather than writing a new Go operator:
DB Provision Operator reconciles databases, Kyverno provisions namespace resources
and injects pod configuration, and cert-manager CSI plus approver-policy issue
namespace-restricted client certificates.

## Database provisioning

- Automatically provision existing and future application namespaces; exclude
  Kubernetes and platform infrastructure namespaces.
- Keep one deterministic `Database` resource per namespace in a protected
  infrastructure namespace. Retain this resource, its SQL database, and its SQL
  user when the application namespace is deleted. Recreating the namespace restores
  access to the retained data.
- Derive stable SQL-safe database/user names and record the namespace mapping.
- Use `Database.initSQL` with `failurePolicy: Block` for idempotent passwordless
  user creation, schema ownership/grants, and removal of permissive PUBLIC grants.
  Keep database ownership with the provisioning identity. Namespace users get
  read/write and table/schema DDL permissions without cluster administration.
- Do not use `DatabaseUser`: its current creation handler requires a password.
- Keep privileged instance configuration, credentials, and generated SQL away
  from application namespace administrators.

## Pod certificates

- Inject a read-only cert-manager CSI volume at `/var/run/cockroachdb`, the server
  CA bundle, and PostgreSQL connection settings into containers and init containers.
- Each pod receives a distinct certificate/key. The certificate CN authenticates
  a shared namespace SQL identity. Use 24-hour certificates renewed eight hours
  before expiry and `sslmode=verify-full`.
- Add a first init container that retries an authenticated SQL check until the
  database and permissions are ready. Provide an application example that reloads
  rotated credentials for new connections.

## Certificate approval

- Enable CSI service-account token requests with namespace-scoped request RBAC.
- Disable cert-manager's unrestricted default approval. Generate approval policies
  bound to the requesting namespace's service accounts and restricted to its
  exact SQL identity, client-auth usage, issuer, duration, and namespace URI SANs.
- Configure separate restrictive approval for infrastructure bootstrap/renewal.
- Pod URI SANs are diagnostic; authorization depends on the authenticated service
  account namespace and the approved SQL identity.

## Delivery and validation

- Pin Helm releases and controller/container versions. Supply deployment values,
  Kyverno policies, approval policies, bootstrap SQL, installation instructions,
  an example application, and repeatable tests.
- Validate the chosen DB Provision release against secure CockroachDB before
  relying on its initSQL/passwordless-user and retention behavior. Report an
  incompatibility before introducing a fork or custom controller.
- Test positive CRUD/DDL, cross-namespace denial, forged certificate requests,
  non-root access, distinct replica keys, renewal, retries, deletion/recreation,
  and availability after losing one CockroachDB node.

## Defaults and assumptions

One Kubernetes cluster, three CockroachDB nodes with persistent volumes on distinct
Kubernetes nodes, all application namespaces enabled automatically, retained data
reattached by namespace name, and application drivers configured to consume and
reload the injected certificate files. Infrastructure installation is separate
from workload deployment. No existing cluster is changed without an explicit
installation action.

# Roachmotel

A Go Kubernetes operator that connects to an existing CockroachDB as root/admin and
creates one database, SQL username/password, and connection-string Secret for each
application namespace. No CRDs, Kyverno, cert-manager, or DB Provision Operator.

CockroachDB runs separately. Its official Kubernetes operator can continue managing
three database replicas; Roachmotel does not create or resize database nodes.

## Install

Build and publish `docker build -t YOUR_REGISTRY/roachmotel:TAG .`, then set that image
and `APPLICATION_URL_TEMPLATE` in [deploy/operator.yaml](deploy/operator.yaml).
The template specifies the application endpoint and TLS settings, without credentials.

Create the operator namespace and supply a Secret from a protected local file
containing the admin PostgreSQL URL:

```sh
kubectl create namespace roachmotel-system
kubectl -n roachmotel-system create secret generic roachmotel-admin \
  --from-file=connection-string=/secure/admin-url \
  --from-file=ca.crt=/secure/ca.crt
kubectl apply -f deploy/operator.yaml
```

Admin URLs can use password authentication or externally supplied client certificates.
For client certificates, add the required files to the admin Secret and refer to
`/var/run/admin/FILE` in its URL. No certificates are issued by Roachmotel.
For verified application TLS, `ca.crt` must trust the application endpoint; the
operator copies this public CA into each application Secret. Applications mount
that Secret at `/var/run/roachmotel` as shown in the example.

Plaintext application SQL is optional: explicitly set `sslmode=disable` in the
template and configure the existing CockroachDB to accept plaintext SQL with password
authentication. In that mode `ca.crt` is optional. CockroachDB `--insecure` disables
password authentication and cannot provide namespace isolation.

## Behavior

The controller reconciles namespaces every 30 seconds, including existing ones.
It skips `kube-*`, its own namespace, `EXCLUDED_NAMESPACES`, terminating namespaces,
and namespaces labeled `roachmotel.dev/enabled=false`. Exclusion stops reconciliation;
it does not revoke existing access or remove data. Exclude other infrastructure namespaces
explicitly. Namespace/database identifiers are `rm_` plus 32 SHA-256 hex characters.

Each namespace receives `roachmotel-credentials`, containing `connection-string`
and, with verified TLS, `ca.crt`. Pods reference the Secret explicitly; there is no
pod injection, webhook, or automatic restart. [examples/deployment.yaml](examples/deployment.yaml)
mounts it for the sample application. Kubernetes waits for this required Secret
before starting the container. The application retries until SQL is available.

Credentials are generated using 256 bits of cryptographic randomness and retained
as Secrets in the operator namespace **before** SQL changes. Failed SQL and Secret
operations retry. Namespace deletion retains its database, SQL user, and master
credential; recreating the same namespace restores access to the same data/password.
Back up these master Secrets along with CockroachDB. The operator refuses to adopt
an existing database/user without its master credential, or overwrite an unrelated
application Secret. Retained credentials have no namespace owner references.

Applications can create schemas/tables and read/write their own data. They are not
admins and cannot drop the database. Public access is revoked on managed databases;
Roachmotel does not change grants on pre-existing databases. Review those grants
when connecting to a shared existing cluster.

Run exactly **one** operator replica (`Recreate` deployment strategy). There is no
leader election. The operator holds SQL administration credentials and broad Secret
RBAC: keep its namespace and service account restricted to platform administrators.
No automatic password rotation or zero-downtime rotation is implemented. Do not edit
master credentials to rotate passwords; coordinated rotation remains future work.

## Development

```sh
go test ./...
go build ./cmd/roachmotel
COCKROACH=/path/to/cockroach go test ./internal/operator -run TestSQLIntegration -v
```

The optional SQL integration test starts a temporary, password-authenticated local
CockroachDB with supplied/generated certificates, then checks provisioning, retries,
DDL, authentication, and tenant isolation. Kubernetes reconciliation uses a fake
API client in unit tests. This revision has not been deployed into Kubernetes;
the previous kind cluster was deleted at your request.

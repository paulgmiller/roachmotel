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
Use a namespace-qualified Kubernetes Service hostname, such as
`cockroachdb-public.database.svc.cluster.local`. The Service must select the
CockroachDB pods and expose the template's SQL port (5432 when omitted). Roachmotel
uses its selector and target port to construct NetworkPolicies.

Create the operator namespace and supply a Secret from a protected local file
containing the admin PostgreSQL URL:

```sh
kubectl create namespace roachmotel-system
kubectl -n roachmotel-system create secret generic roachmotel-admin \
  --from-file=connection-string=/secure/admin-url \
  --from-file=ca.crt=/secure/ca.crt
kubectl apply -f deploy/operator.yaml
```

For the Zebu cluster, the configuration in `deploy/kustomization.yaml` uses
`cockroachdb-secure-public.cockroach.svc.cluster.local:26257` with TLS
and the Fluent Bit collector. Build and publish the updated controller before installing:

```sh
docker build -t paulgmiller/roachmotel:fluent-bit .
docker push paulgmiller/roachmotel:fluent-bit
# Supply roachmotel-admin as described above, then:
kubectl --context zebu apply -k deploy
kubectl --context zebu -n roachmotel-system rollout status deployment/roachmotel
```

The admin Secret must contain a working admin URL and any CA/client files it
references. SQL ingress policies are reconciled by Roachmotel; no opt-in label is
required. The Zebu configuration excludes infrastructure namespaces.

## Cluster container logs

`deploy/` includes a Fluent Bit DaemonSet in a dedicated `logs` namespace. The
controller setting `LOGS_NAMESPACE=logs` reserves SQL database/user `logs` for
that namespace; other namespaces retain their hashed identities. The controller
creates its usual `logs/roachmotel-credentials` Secret with a `connection-string`
key (this is the generated credential referred to as roachkey). No separate
password or admin credential is supplied to Fluent Bit. The collector waits for
this required Secret before starting, and uses the controller's namespace SQL
NetworkPolicy. Deploy the updated controller image before the collector.

The init container reads the generated URI and creates a private libpq service
file in a memory-backed volume, preserving TLS and connection settings. The
password is excluded from the Fluent Bit config, environment, and arguments.
Restart the DaemonSet after manually changing credentials or connection settings.

The DaemonSet tails `/var/log/containers/*.log` on every Linux node, including
tainted nodes and all namespaces, parses Docker/CRI records and adds Kubernetes
metadata. It stores records in `logs.public.containers` (`tag`, `time`, `data JSONB`).
Offsets and pending chunks persist on each node under
`/var/lib/roachmotel-fluent-bit`. Retries are unlimited, with a 1 GiB queue per node;
when full, oldest queued chunks are dropped. Lines larger than 1 MiB are skipped.
Collection starts at the beginning of available files on first install. Logs
already rotated away, host journal files, and application file-only logs are not
collected. Delivery can duplicate records after retries. Database retention is
not configured; manage it separately to limit growth.

For a generic installation, set `LOGS_NAMESPACE=logs` on the updated operator,
then apply `kubectl apply -k deploy/fluent-bit`. The collector derives the SQL
endpoint from the generated Secret. The node layout assumes container log symlinks
resolve within `/var/log` (as on containerd). Docker installations using
`/var/lib/docker/containers` need an additional read-only host mount.

Use a **new** collector namespace. If `logs` was already provisioned with a hashed
identity, the controller refuses to switch it silently; choose a fresh namespace
and update `LOGS_NAMESPACE` and the collector manifests together, or explicitly
migrate the retained credentials/data. A pre-existing unmanaged SQL `logs` database
or user also requires explicit migration. The collector namespace must not be
excluded from reconciliation. Opt-out/re-enable retains the usual credentials
and SQL/network revocation behavior. Keep the collector namespace restricted to
administrators: its credential can read logs from every collected namespace.

Admin URLs can use password authentication or externally supplied client certificates.
For client certificates, add the required files to the admin Secret and refer to
`/var/run/admin/FILE` in its URL. No certificates are issued by Roachmotel.
Application URLs use `sslmode=require`: connections use TLS without verifying
server certificates, so applications need no certificate files. The application
Secret contains only `connection-string` and is mounted at `/var/run/roachmotel`
as shown in the example. Admin connections can still use their own CA or client
certificate files through the admin URL; these are never copied to application Secrets.
CockroachDB must use password authentication; `--insecure` disables it and cannot
provide namespace isolation.

## Behavior

The controller reconciles namespaces every 30 seconds, including existing ones.
It skips `kube-*`, its own namespace, `EXCLUDED_NAMESPACES`, terminating namespaces,
and the CockroachDB Service namespace. Retained credential Secret
names use `rm-` plus the namespace hash to satisfy Kubernetes naming rules.
Infrastructure exclusions stop reconciliation without revoking existing access.
Exclude other infrastructure namespaces explicitly.

Namespaces opt out using the annotation `roachmotel.dev/enabled=false`:

```sh
kubectl annotate namespace my-app roachmotel.dev/enabled=false
# Restore access to the same database and credentials:
kubectl annotate namespace my-app roachmotel.dev/enabled-
```

On the next reconciliation, opting out disables the SQL user's login, cancels its
sessions, revokes its database grants, and removes its managed NetworkPolicy and
application credential Secret. **The database, tables, SQL user, and retained master
credential are preserved.** Removing the annotation restores access. The legacy
`roachmotel.dev/enabled=false` label also revokes access; remove it to re-enable.

Each enabled namespace gets an ingress NetworkPolicy in the CockroachDB Service
namespace allowing all its pods to reach only the SQL target port. A persistent
infrastructure policy allows traffic from within the database namespace and SQL
traffic from the operator pods. This remains when all tenants opt out, keeping
CockroachDB ingress isolated. Use a network plugin that enforces NetworkPolicy.
Existing egress restrictions must independently allow SQL traffic. Other ingress
policies are additive: remove old static tenant allowlists, including the tenant
rules in an already-installed `roachmotel-sql-access` policy, to enforce opt-outs.
Policies for external monitoring or database administration must be supplied
separately. Namespace/database identifiers are `rm_` plus 32 SHA-256 hex characters.

Each namespace receives `roachmotel-credentials`, containing only `connection-string`.
Pods reference the Secret explicitly; there is no
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
and NetworkPolicy RBAC: keep its namespace and service account restricted to platform administrators.
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
API client in unit tests. The Zebu installation is configured in `deploy/`.


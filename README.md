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
and a pinned controller image, plus the Fluent Bit collector.

Supply `roachmotel-system/roachmotel-admin` as above, then run:

```sh
scripts/deploy.sh
# Preview manifests or choose an image tag/context:
scripts/deploy.sh --image paulgmiller/roachmotel:my-tag --render-only
scripts/deploy.sh --context zebu --image paulgmiller/roachmotel:my-tag
```

The script runs Go tests, builds and pushes a `linux/amd64` image using Docker
buildx, deploys the pushed digest, and waits for rollout. It defaults to context
`zebu` and a timestamp/Git revision tag in `paulgmiller/roachmotel`. Registry login,
Go, Python 3, Docker buildx, and kubectl are required. Use `--platform` to change
the image platform and `--timeout` to change the rollout wait (default `180s`).
The checked-in image pin stays unchanged. SQL ingress policies are reconciled
by Roachmotel for each application namespace; no opt-in label is required.
CockroachDB, its operator, Traefik, WireGuard, and Whoami namespaces are excluded
from provisioning by this configuration.

Admin URLs can use password authentication or externally supplied client certificates.
For client certificates, add the required files to the admin Secret and refer to
`/var/run/admin/FILE` in its URL. No certificates are issued by Roachmotel.
Application URLs use `sslmode=require`: connections use TLS without verifying
server certificates, so applications need no certificate files. The application
Secret contains only `DATABASE_URL` and is mounted at `/var/run/roachmotel`
as shown in the example. Admin connections can still use their own CA or client
certificate files through the admin URL; these are never copied to application Secrets.
CockroachDB must use password authentication; `--insecure` disables it and cannot
provide namespace isolation.

## Cluster container logs

`deploy/` includes a Fluent Bit DaemonSet in the `logs` namespace. Its native
[`pgsql` output](https://docs.fluentbit.io/manual/data-pipeline/outputs/postgresql)
sends logs directly to CockroachDB with `CockroachDB On`, using the generated
`roachkey` Secret's `DATABASE_URL`. No additional service or custom image is needed.
The controller provisions database/user `logs` and the namespace's SQL NetworkPolicy.

The DaemonSet tails `/var/log/containers/*.log` on every Linux node, including
tainted nodes and all namespaces, parses Docker/CRI records, and adds Kubernetes
metadata. All records go into **`logs.public.containers`**, which the output plugin
creates with `tag`, `time`, and `data` (JSONB) columns. Namespace information stays
in the source tag and Kubernetes metadata; there are no per-namespace tables.
No default retention is imposed.

Offsets and pending chunks persist on each node under
`/var/lib/roachmotel-fluent-bit`. Retries are unlimited, with a 1 GiB queue per node;
when full, oldest queued chunks are dropped. Lines larger than 1 MiB are skipped.
Collection starts at the beginning of available files on first install. Logs
already rotated away, host journal files, and application file-only logs are not
collected. Delivery can duplicate records after retries.

For a generic installation, apply `kubectl apply -k deploy/fluent-bit`. An init
container derives the plugin's required host/port settings from `DATABASE_URL`;
credentials and TLS settings come from that same Secret. No endpoint is configured
separately. Output logging is restricted to errors because the plugin includes its
Database setting in informational messages. Restart the DaemonSet after updating
credentials: `kubectl -n logs rollout restart daemonset/fluent-bit`.
The node layout assumes container log symlinks resolve within `/var/log` (as on
containerd). Docker installations using `/var/lib/docker/containers` need an
additional read-only host mount.

The collector namespace must not be excluded from reconciliation. Keep it
restricted to administrators: its credential can read logs from every collected
namespace. Opt-out/re-enable retains the usual credentials and SQL/network
revocation behavior.

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
separately. The SQL username and database name exactly match the application
namespace, including hyphens (for example, `tenant-a`). SQL identifiers are quoted.
Existing databases and users with hashed names are not automatically renamed or
migrated; migrate existing tenant data before switching applications to these names.

Each namespace receives `roachkey`, containing only `DATABASE_URL`.
The controller ignores `roachmotel-credentials` entirely, including during opt-out.
Pods reference the Secret explicitly; there is no
pod injection, webhook, or automatic restart. [examples/deployment.yaml](examples/deployment.yaml)
mounts it for the sample application. Kubernetes waits for this required Secret
before starting the container. The application retries until SQL is available.

Credentials are generated using 256 bits of cryptographic randomness and retained
as Secrets in the operator namespace **before** SQL changes. Failed SQL and Secret
operations retry. Namespace deletion retains its database, SQL user, and master
credential; recreating the same namespace restores access to the same data/password.
Back up these master Secrets along with CockroachDB. The operator refuses to adopt
an existing user without its master credential, or overwrite an unrelated
application Secret. Retained credentials have no namespace owner references.

Applications can create schemas/tables and read/write their own data. They are not
admins and cannot drop the database. Public access is revoked on newly created
databases. If a namespace database already exists, Roachmotel preserves its data,
ownership, existing grants, and default privileges. It creates the namespace user
and adds database connect/create, public schema usage/create, public table data
access, and sequence access. Grants on existing public tables and sequences are
refreshed on reconciliation. Other schemas and changes to existing objects require
grants from their owners. Review public grants when reusing an existing database.

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

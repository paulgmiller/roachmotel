# Roachmotel

One CockroachDB database and SQL username/password per Kubernetes application
namespace, with a Secret containing its full connection string.

The official CockroachDB Kubernetes operator manages **three persistent replicas**.
DB Provision Operator creates databases/users and generates credentials; Kyverno
provisions namespace resources and injects pod configuration. Cert-manager and
approver-policy manage infrastructure certificates only. No workload certificates
or CSI driver are required.

**Status: initial implementation; not ready for cluster activation.** An upstream
bootstrap retry defect remains blocking. See [compatibility](docs/COMPATIBILITY.md)
and the [plan](PLAN.md).

## Render and inspect

Use Python 3.11+:

```sh
python3 -m venv .venv
.venv/bin/pip install -r requirements-test.txt
.venv/bin/python -m roachmotel.render
.venv/bin/python -m unittest discover -s tests -v
```

Edit config.yaml for storage, region, excluded namespaces, and transport settings.
Rendering writes build/ only and never accesses kubeconfig or applies resources.
Dependencies are pinned in dependencies.yaml.

Database names/usernames are `rm_` plus the first 32 SHA-256 hex characters of the
namespace name. Print the mapping with `python3 -m roachmotel.render --identity tenant-a`.
Database/User CRs and source credential Secrets stay in infrastructure. Namespace
Secrets are clones of their own credentials; namespace deletion/recreation should
restore retained data and passwords. Kubernetes integration validation is pending.

## Transport choices

Default `workloadTLS: true` generates sslmode=verify-full URLs and mounts the public
CA at /var/run/cockroachdb-trust/ca.crt. This CA is public trust material, not an
application certificate or private key; its distribution does not need cert-manager.

Set `workloadTLS: false` and rerender to allow plaintext application SQL. This adds
--accept-sql-without-tls to database node start flags and sslmode=disable to application
URLs. Application CA mounts are removed, but password authentication, node TLS,
and verified infrastructure connections remain enabled. External transport
protection is the deployment operator's responsibility in this mode.

Do not use CockroachDB --insecure: it permits clients to log in as root without a
password and defeats namespace isolation. Cert-manager could be replaced with
externally supplied infrastructure certificates; that option is not implemented.
Secure database nodes still need certificates.

## Installation for integration testing

Use a disposable Kubernetes cluster with three schedulable workers, a StorageClass,
Helm 3, kubectl, and platform credentials. The baseline requests 2 CPUs and 4 GiB
memory per database node. Align region with worker region labels.

1. Render and print controller installation commands:

   ```sh
   python3 scripts/commands.py controllers
   ```

   Run the printed commands explicitly. These install cert-manager with unrestricted
   auto-approval disabled, restricted approver-policy, Kyverno, and both database
   operators. This targets a new installation; existing certificate consumers
   would need their own approval policies. Application accounts get no approval RBAC.

2. Install an administrator-provided PEM CA certificate/key, keeping the key out
   of this repo and ensuring its lifetime exceeds issued certificate lifetimes:

   ```sh
   PYTHONPATH=. python3 scripts/bootstrap_trust.py --ca-cert /secure/ca.crt --ca-key /secure/ca.key
   ```

   The key goes only to cert-manager's namespace; the public CA goes into the
   infrastructure ConfigMap. The helper refuses to replace an existing authority.

3. Protect provisioning APIs, install infrastructure, and print the cluster command:

   ```sh
   kubectl apply -f build/protection-policy.yaml
   kubectl apply -f build/infrastructure.yaml
   python3 scripts/commands.py cluster
   ```

   Review/run the printed Helm command. Restricted infrastructure approval permits
   root/node/provisioner certificates only. The provisioning identity connects
   using verified mTLS irrespective of workloadTLS.

4. **Namespace activation remains gated.** Candidate namespace-policy.yaml and
   pod-policy.yaml in build/ must wait for the upstream fix and Kubernetes tests.
   No script automatically activates them in your current context.

## Workload behavior

Pods receive /var/run/roachmotel/connection-string as a read-only Secret file,
ROACHMOTEL_DATABASE_URL from the same Secret, and ROACHMOTEL_DATABASE_URL_FILE
pointing to that file. All pods in a namespace share the same username/password.
These variables, injected volume names/mount paths, and roachmotel-wait are reserved.
DATABASE_URL and other PostgreSQL variables are not overwritten: applications must
explicitly consume the Roachmotel URL or file.

The first init container waits for authentication and database/schema CREATE
privileges before migrations start. Secret permissions are 0440. Existing fsGroup
is preserved, otherwise group 2000 is supplied. Pods wait for Secrets to be present.
Apps get read/write and DDL within their own database without cluster administration.

Passwords are generated as 48 alphanumeric characters. The pinned operator keeps a
read-back password in a Secret annotation as well as the URL in Secret data. Treat
**the entire Secret, including metadata, as confidential**. The template contains
no infrastructure TLS private material.

Automatic rotation is disabled. Mounted file consumers must reread credentials and
refresh pools; environment consumers need restart. Single-user SQL/Secret changes
require coordination and may briefly interrupt new connections. No automatic or
zero-downtime rotation is claimed.

The example reads the mounted URL for each new connection. Build with
`docker build -t roachmotel-demo:0.1.0 examples` and make the image available to the
cluster before deploying the example.

## Validation

With pinned binaries and CRDs available:

```sh
KYVERNO=/path/to/kyverno make policy-test
COCKROACH=/path/to/cockroach make sql-test
PYTHONPATH=. COCKROACH=/path/to/cockroach python3 tests/check_sql.py --plaintext
PYTHONPATH=. python3 tests/check_schemas.py --crd-dir /path/to/pinned/crds
```

Policy tests exercise real mutation/template substitution and API protection.
Local SQL tests use temporary single-node processes with password authentication,
verified TLS by default, and optional plaintext tested separately. They do not
exercise Kubernetes or three-node availability. Schema checks reject unknown
fields; CEL, RBAC, Secret cloning, and generation require actual cluster tests.

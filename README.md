# Roachmotel

One CockroachDB database per Kubernetes application namespace, with individual
pod client certificates and namespace-scoped SQL permissions.

The official CockroachDB Kubernetes operator manages **three persistent replicas**.
DB Provision Operator manages logical databases, Kyverno generates resources and
injects pod configuration, and cert-manager CSI plus approver-policy issue client
certificates. There is no custom Go controller.

**Status: initial implementation; not yet ready for cluster activation.** A
confirmed DB Provision Operator retry defect blocks the planned automatic recovery
behavior. See [compatibility status](docs/COMPATIBILITY.md) and the [plan](PLAN.md).

## Render and inspect

Use Python 3.11+ and install `requirements-test.txt` in a virtual environment.

```sh
python3 -m venv .venv
.venv/bin/pip install -r requirements-test.txt
.venv/bin/python -m roachmotel.render
.venv/bin/python -m unittest discover -s tests -v
```

Edit `config.yaml` for your storage class, region/node labels, namespaces, and
excluded infrastructure. Rendering only writes `build/`; it never reads kubeconfig
or applies anything. `dependencies.yaml` records exact chart versions.

The database and SQL username are both `rm_` followed by the first 32 SHA-256 hex
characters of the namespace name. This meets the upstream database CRD's naming
rules, handles numeric/hyphenated/long namespace names, and stays stable across
deletion/recreation:

```sh
python3 -m roachmotel.render --identity tenant-a
```

The injected `roachmotel.dev/database` pod annotation records the mapping. Database
CRs live in the infrastructure namespace, with Retain, deletion protection, and
unsynchronized generation, so deleting an application namespace does not delete
the resource or its SQL data. A recreated namespace regains access by name.

## Installation sequence for integration testing

Installation requires a test cluster with at least three schedulable worker nodes,
a working default/configured StorageClass, Helm 3, kubectl, and platform credentials.
Align `region` with node region labels when present. Three nodes with at least 2
available CPUs and 4 GiB memory each are the rendered database baseline.

1. Render, then print and review controller installation commands:

   ```sh
   python3 scripts/commands.py controllers
   ```

   Run the printed commands explicitly. These install cert-manager with its default
   auto-approver **disabled**, restricted approver-policy, CSI service-account token
   requests, Kyverno, and the two database operators. On an existing cluster,
   disabling auto-approval also requires policies for its unrelated certificates.
   This project targets a new installation.

2. Install an administrator-provided PEM CA certificate/key. The authority must
   be suitable for TLS signing and remain valid longer than the issued certificates.
   Keep the signing key outside this repository:

   ```sh
   PYTHONPATH=. python3 scripts/bootstrap_trust.py --ca-cert /secure/ca.crt --ca-key /secure/ca.key
   ```

   The key goes only to a Secret in cert-manager's namespace. Only the public CA
   goes into the infrastructure ConfigMap. Existing authorities are never replaced
   by this script; coordinated CA rotation is a separate operation.

3. Apply the platform-only database API protection **before** enabling provisioned
   workloads, then infrastructure resources, and review/run cluster installation:

   ```sh
   kubectl apply -f build/protection-policy.yaml
   kubectl apply -f build/infrastructure.yaml
   python3 scripts/commands.py cluster
   ```

   Infrastructure approval policies permit the cert-manager controller to request
   only root/node/provisioner certificates in the infrastructure namespace. No
   application service account is bound to these policies. Cluster post-init SQL
   creates the privileged provisioning user; its certificate remains in the
   infrastructure namespace. The `DatabaseInstance` uses verified mTLS.

4. **Namespace activation is pending the compatibility gate.** The candidate
   policies are `build/namespace-policy.yaml` and `build/pod-policy.yaml`. Do not
   treat them as a validated deployment until the failed-initSQL retry defect is
   fixed and the Kubernetes scenarios in `docs/COMPATIBILITY.md` pass. No script
   automatically activates these policies or deploys into your current context.

## Workload behavior

Once activated, each pod gets its own CSI certificate/key, mounted read-only at
`/var/run/cockroachdb`; the public server CA is at `/var/run/cockroachdb-trust/ca.crt`.
The SQL identity is shared within the namespace. Issuance is restricted to that
namespace's exact CN and service-account group. No workload password is generated.

The policy configures `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGSSLMODE`,
`PGSSLROOTCERT`, `PGSSLCERT`, `PGSSLKEY`, and `ROACHMOTEL_DATABASE_URL`. These names,
the two volume names/mount paths, and `roachmotel-wait` are reserved. Existing values
for those environment variables are replaced. `DATABASE_URL` is not overwritten;
apps must explicitly use the Roachmotel URL or the PostgreSQL environment settings.

The first init container waits for an authenticated SQL connection before any
application migration init container starts. Existing filesystem groups are
preserved; otherwise the policy defaults to group 2000. Pods remain pending while
certificate/volume provisioning is unavailable.

Certificates last 24 hours and renew eight hours before expiry. Application drivers
must reload the certificate files for fresh connections. The example under
`examples/` opens a new connection per iteration, allowing renewal without restarting
the pod. Build it with `docker build -t roachmotel-demo:0.1.0 examples` and make the
image available to your test cluster before applying the example Deployment.

## Additional validation

With the pinned Kyverno CLI and CockroachDB binary installed:

```sh
KYVERNO=/path/to/kyverno make policy-test
COCKROACH=/path/to/cockroach make sql-test
```

`policy-test` checks the real mutation engine: namespace identity, non-root group
access settings, init ordering, exclusions, and reinvocation. `sql-test` creates
and cleans up an isolated **local** secure CockroachDB process with temporary keys;
it checks idempotent bootstrap, CRUD/DDL, new schemas, cross-namespace denial,
database/user creation denial, and rejection of a root login using a tenant cert.
It does not test the Kubernetes operator or three-node availability.

Validate generated custom resources against downloaded pinned upstream CRDs:

```sh
PYTHONPATH=. python3 tests/check_schemas.py --crd-dir /path/to/pinned/crds
```

The validator rejects unknown fields; Kubernetes CEL validations and admission
authorization still require a real test cluster. Namespace generation and approval
RBAC require cluster integration tests; the CLI alone cannot establish them.

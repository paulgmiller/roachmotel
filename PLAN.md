# Roachmotel implementation plan

Deploy three persistent CockroachDB nodes with the official Kubernetes operator.
DB Provision Operator manages logical databases and password users; Kyverno
provisions namespace resources and injects application connection configuration.
Applications share one SQL username/password per namespace.

## Provisioning and credentials

- Automatically provision existing/future application namespaces, excluding system
  and infrastructure namespaces. Derive stable SQL-safe names from namespace hashes.
- Retain protected Database and DatabaseUser CRs and source credential Secrets in
  infrastructure when application namespaces are deleted. Recreating the same
  namespace restores access to its retained database with the same password.
- DatabaseUser generates a cryptographically random 48-character alphanumeric
  password and a Secret containing the full connection string. Alphanumeric
  passwords avoid URI userinfo encoding ambiguities.
- Clone each namespace's own credential Secret to `roachmotel-credentials` in that
  namespace. Never distribute infrastructure credentials or TLS private material.
- Transactional Database.initSQL removes PUBLIC privileges, grants read/write/DDL,
  and assigns schema ownership. Keep database ownership with the provisioner.
  Bootstrap never resets a managed password. Namespace users cannot create users
  or databases, administer the cluster, or drop their whole database.

## Pod configuration

- Mount the connection-string Secret read-only at
  `/var/run/roachmotel/connection-string` in containers and init containers.
- Inject ROACHMOTEL_DATABASE_URL from the Secret and ROACHMOTEL_DATABASE_URL_FILE
  pointing to that file. Apps explicitly choose how to consume these settings.
- Preserve fsGroup when present, otherwise default to group 2000. Secret mode 0440.
- A first init container checks authentication plus schema/database CREATE
  privileges before application migrations start. Suppress SQL error details to
  avoid accidentally logging credentials.
- Supply an example reading the mounted connection string for each new connection.

## Transport

- Default `workloadTLS: true`: sslmode=verify-full with a public CA ConfigMap at
  `/var/run/cockroachdb-trust/ca.crt`. Distributing public trust needs no cert-manager.
- Explicit `workloadTLS: false`: add --accept-sql-without-tls to database start flags
  and sslmode=disable to application URLs. Password authentication remains active;
  external transport protection is the deployment operator's responsibility.
- Never use --insecure, which bypasses password authentication and namespace isolation.
- Keep database node TLS and verified infrastructure administrator connections in
  both modes. Retain cert-manager and restricted approver-policy only for database
  root/node/provisioner certificates. Remove CSI and all workload certificate policies.
- External certificate provisioning could replace infrastructure cert-manager later;
  secure database nodes still need certificates. This option is not implemented.

## Rotation

Automatic password rotation is disabled in this simpler version. Changing one
user's password and publishing a Kubernetes Secret is not atomic; old and new
passwords cannot overlap. Rotation requires an explicit coordinated operation
covering SQL, Secret publication, application reload/replacement, and verification.
No periodic rotation or blanket pod eviction is enabled. Zero-downtime rotation
would require additional login identities or a credential broker.

## Validation and activation

- Pin releases and supply values, policies, installation instructions, bootstrap
  helper, example application, and tests. Rendering never accesses kubeconfig.
- Verify password/template persistence, bootstrap reconciliation order, Secret clone
  races, removal of cross-namespace source owner references, non-root access,
  retained credentials/data on namespace recreation, and both transport modes.
- Verify password acceptance/rejection, CRUD/DDL, namespace and privilege isolation,
  infrastructure certificate approval/renewal, and three-node failover.
- Resolve the existing upstream failed-initSQL retry defect before activation.
  Do not silently introduce a fork or replacement controller.

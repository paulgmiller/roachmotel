# Roachmotel implementation plan

Replace the policy composition with one Go operator. The existing CockroachDB is
provided by the administrator; the official CockroachDB operator remains responsible
for its three replicas if it is running in Kubernetes.

1. Supply an admin URL and optional external TLS files in `roachmotel-admin`.
2. Reconcile non-system namespaces periodically, with opt-out labels/exclusions.
3. Persist a random password in a retained operator-namespace Secret before SQL.
4. Create the database/user, revoke PUBLIC grants, and grant namespace-local DDL/data access.
5. Publish `roachmotel-credentials` with a complete application URL and public CA if needed.
6. Retry partial failures and restore access after namespace recreation without deleting data.
7. Apps explicitly mount/reference Secrets. No injection, certificate issuance, or rotation.
8. Verify retries, conflict handling, namespace retention, SQL grants, and authentication.

Future work: leader election and watch/workqueue scaling, structured status/events,
coordinated password rotation, and Kubernetes integration validation. No automatic
cluster deployment is requested for this revision.

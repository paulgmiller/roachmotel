"""Generate ordinary Helm values and Kyverno manifests without Helm escaping.

Namespace identity comes from the admission request, never a workload-supplied
label. SQL identifiers contain a fixed prefix and 128 bits of namespace SHA-256.
"""

import argparse
import copy
import hashlib
from pathlib import Path
import re

import yaml

ISSUER = {"name": "roachmotel-ca", "kind": "ClusterIssuer", "group": "cert-manager.io"}
NS = "{{ ns_name }}"
IDENTITY = "{{ sql_name }}"
CREDENTIAL_PATH = "/var/run/roachmotel"
CREDENTIAL_SECRET = "roachmotel-credentials"
TRUST_PATH = "/var/run/cockroachdb-trust"
LABEL = {"app.kubernetes.io/part-of": "roachmotel"}


def identity(namespace):
    if not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", namespace):
        raise ValueError("invalid Kubernetes namespace")
    return "rm_" + hashlib.sha256(namespace.encode()).hexdigest()[:32]


def resource(api, kind, name, namespace=None, **fields):
    meta = {"name": name, "labels": copy.deepcopy(LABEL)}
    if namespace:
        meta["namespace"] = namespace
    return {"apiVersion": api, "kind": kind, "metadata": meta, **fields}


def role(name, rules, namespace=None):
    return resource("rbac.authorization.k8s.io/v1", "Role" if namespace else "ClusterRole",
                    name, namespace, rules=rules)


def binding(name, role_name, subjects, namespace=None, cluster_role=True):
    return resource("rbac.authorization.k8s.io/v1", "RoleBinding" if namespace else "ClusterRoleBinding",
                    name, namespace, roleRef={"apiGroup": "rbac.authorization.k8s.io",
                    "kind": "ClusterRole" if cluster_role else "Role", "name": role_name}, subjects=subjects)


def excluded(config):
    return sorted(set(config["excludedNamespaces"] + [config["infrastructureNamespace"],
                      config["certManagerNamespace"], config["kyvernoNamespace"]]))


def validate_config(config):
    for key in ("infrastructureNamespace", "certManagerNamespace", "kyvernoNamespace"):
        identity(config[key])
    if len({config[k] for k in ("infrastructureNamespace", "certManagerNamespace", "kyvernoNamespace")}) != 3:
        raise ValueError("infrastructure, certificate, and policy namespaces must be distinct")
    for name in config["excludedNamespaces"]:
        identity(name)
    if not re.fullmatch(r"[a-z0-9.-]+", config["clusterDomain"]):
        raise ValueError("invalid clusterDomain")
    if not re.fullmatch(r"[a-zA-Z0-9_-]+", config["region"]):
        raise ValueError("invalid region")
    if not isinstance(config["workloadTLS"], bool):
        raise ValueError("workloadTLS must be a boolean")
    if not isinstance(config["defaultFsGroup"], int) or config["defaultFsGroup"] <= 0:
        raise ValueError("defaultFsGroup must be a positive integer")


def namespace_context(pod=False):
    return [
        {"name": "ns_name", "variable": {"jmesPath": "request.namespace" if pod else "request.object.metadata.name"}},
        {"name": "sql_name", "variable": {"jmesPath": "join('', ['rm_', truncate(sha256(ns_name), `32`)])"}},
    ]


def bootstrap_sql(sql_name):
    # All interpolated identifiers are generated, never arbitrary namespace text.
    # One transaction prevents a partial bootstrap from exposing PUBLIC privileges.
    return "\n".join([
        "BEGIN;",
        # Refuse to adopt an unrelated database with conflicting ownership.
        "SELECT 1 / count(*) FROM [SHOW DATABASES] WHERE database_name = current_database() AND owner = current_user;",
        f'CREATE USER IF NOT EXISTS "{sql_name}";',
        # DatabaseUser assigns the password independently. Never reset it here.
        f'REVOKE ALL ON DATABASE "{sql_name}" FROM public;',
        "REVOKE ALL ON SCHEMA public FROM public;",
        "ALTER DEFAULT PRIVILEGES FOR ALL ROLES REVOKE ALL ON TABLES FROM public;",
        f'GRANT CONNECT, CREATE ON DATABASE "{sql_name}" TO "{sql_name}";',
        f'ALTER SCHEMA public OWNER TO "{sql_name}";',
        "COMMIT;",
    ])


def approval(name, namespace, common_name, duration, uris=None, dns=None, infrastructure=False):
    allowed = {"commonName": {"value": common_name, "required": True}, "isCA": False,
               "usages": ["digital signature", "key encipherment", "client auth"]}
    if uris:
        allowed["uris"] = {"values": uris, "required": True}
    if infrastructure:
        allowed["subject"] = {"organizations": {"values": ["Cockroach"]}}
    if dns:
        allowed["dnsNames"] = {"values": dns, "required": True}
        allowed["usages"].append("server auth")
    return resource("policy.cert-manager.io/v1alpha1", "CertificateRequestPolicy", name,
                    spec={"selector": {"issuerRef": ISSUER, "namespace": {"matchNames": [namespace]}},
                          "allowed": allowed, "constraints": {"minDuration": duration, "maxDuration": duration}})


def generate_rule(name, obj, config, synchronize=False):
    return {"name": name,
            "match": {"any": [{"resources": {"kinds": ["Namespace"]}}]},
            "exclude": {"any": [{"resources": {"names": excluded(config)}}]},
            "context": namespace_context(),
            "generate": {"generateExisting": True, "synchronize": synchronize,
                         "orphanDownstreamOnPolicyDelete": True,
                         "apiVersion": obj["apiVersion"], "kind": obj["kind"],
                         "name": obj["metadata"]["name"],
                         **({"namespace": obj["metadata"]["namespace"]} if obj["metadata"].get("namespace") else {}),
                         "data": obj}}


def namespace_policy(config):
    infra = config["infrastructureNamespace"]
    database = resource("dbops.dbprovision.io/v1alpha1", "Database", NS, infra,
        spec={"instanceRef": {"name": "roachmotel"}, "name": IDENTITY,
              "deletionPolicy": "Retain", "deletionProtection": True,
              "initSQL": {"inline": [bootstrap_sql(IDENTITY)], "failurePolicy": "Block"}})
    database["metadata"]["annotations"] = {"roachmotel.dev/namespace": NS}
    user = resource("dbops.dbprovision.io/v1alpha1", "DatabaseUser", NS + "-user", infra,
        spec={"instanceRef": {"name": "roachmotel"}, "username": IDENTITY,
              "passwordSecret": {"generate": True, "length": 48, "includeSpecialChars": False,
                  "secretName": NS + "-credentials", "secretTemplate": {
                      # Backslash escapes the Go template from Kyverno substitution.
                      # Generated alphanumeric passwords are safe in URI userinfo.
                      "data": {"connection-string": connection_string(config, r"\{{ .Password }}")}}},
              "passwordRotation": {"enabled": False}})
    user["metadata"]["annotations"] = {"roachmotel.dev/namespace": NS,
        "dbops.dbprovision.io/deletion-policy": "Retain",
        "dbops.dbprovision.io/deletion-protection": "true"}
    objs = [("database", database, False), ("user", user, False)]
    rules = [generate_rule(name, obj, config, sync) for name, obj, sync in objs]
    clone = generate_rule("credentials", resource("v1", "Secret", CREDENTIAL_SECRET, NS), config, True)
    del clone["generate"]["data"]
    clone["generate"]["clone"] = {"namespace": infra, "name": NS + "-credentials"}
    rules.append(clone)
    if config["workloadTLS"]:
        trust = generate_rule("server-trust", resource("v1", "ConfigMap", "roachmotel-trust", NS), config, True)
        del trust["generate"]["data"]
        trust["generate"]["clone"] = {"namespace": infra, "name": "roachmotel-trust"}
        rules.append(trust)
    return resource("kyverno.io/v1", "ClusterPolicy", "roachmotel-namespaces", spec={"rules": rules})


def connection_string(config, password):
    host = f"cockroachdb-public.{config['infrastructureNamespace']}.svc.{config['clusterDomain']}"
    tls = f"sslmode=verify-full&sslrootcert={TRUST_PATH}/ca.crt" if config["workloadTLS"] else "sslmode=disable"
    return f"postgresql://{IDENTITY}:{password}@{host}:26257/{IDENTITY}?{tls}"


def connection_env():
    return [{"name": "ROACHMOTEL_DATABASE_URL", "valueFrom": {"secretKeyRef": {
        "name": CREDENTIAL_SECRET, "key": "connection-string"}}},
        {"name": "ROACHMOTEL_DATABASE_URL_FILE", "value": f"{CREDENTIAL_PATH}/connection-string"}]


def mounts(config):
    result = [{"name": "roachmotel-credentials", "mountPath": CREDENTIAL_PATH, "readOnly": True}]
    if config["workloadTLS"]:
        result.append({"name": "roachmotel-trust", "mountPath": TRUST_PATH, "readOnly": True})
    return result


def pod_policy(config, image):
    match = {"any": [{"resources": {"kinds": ["Pod"], "operations": ["CREATE"]}}]}
    exclude = {"any": [{"resources": {"namespaces": excluded(config)}}]}
    def rule(name, mutation):
        return {"name": name, "match": match, "exclude": exclude,
                "context": namespace_context(pod=True), "mutate": mutation}
    volumes = [{"name": "roachmotel-credentials", "secret": {"secretName": CREDENTIAL_SECRET,
        "defaultMode": 0o440, "items": [{"key": "connection-string", "path": "connection-string"}]}}]
    if config["workloadTLS"]:
        volumes.append({"name": "roachmotel-trust", "configMap": {"name": "roachmotel-trust"}})
    volume_rule = rule("volumes", {"patchStrategicMerge": {
        "metadata": {"annotations": {"roachmotel.dev/database": IDENTITY}},
        "spec": {"securityContext": {"+(fsGroup)": config["defaultFsGroup"]}, "volumes": volumes}}})
    wait = {"name": "roachmotel-wait", "image": image, "command": ["/bin/sh", "-ec"],
            "args": ['until /cockroach/cockroach sql --url="$(cat "$ROACHMOTEL_DATABASE_URL_FILE")" --execute="SELECT 1 / CASE WHEN has_schema_privilege(\'public\', \'CREATE\') AND has_database_privilege(current_database(), \'CREATE\') THEN 1 ELSE 0 END" >/dev/null 2>&1; do sleep 5; done'],
            "env": connection_env(), "volumeMounts": mounts(config),
            "securityContext": {"runAsNonRoot": True, "runAsUser": 2000, "allowPrivilegeEscalation": False,
                                "capabilities": {"drop": ["ALL"]}},
            "resources": {"requests": {"cpu": "10m", "memory": "64Mi"}, "limits": {"memory": "256Mi"}}}
    # Put wait first in the patch order and preserve application init containers.
    wait_rule = rule("wait-before-application-init", {"patchStrategicMerge": {
        "spec": {"initContainers": "{{ desired_init }}"}}})
    wait_rule["context"].extend([
        {"name": "wait_container", "variable": {"value": wait}},
        {"name": "desired_init", "variable": {"jmesPath":
            "[wait_container, request.object.spec.initContainers[?name != 'roachmotel-wait'] || `[]`][]"}},
    ])
    foreach = []
    env_patch = [{**item, **({"value": None} if "valueFrom" in item else {"valueFrom": None})}
                 for item in connection_env()]
    for field in ["containers", "initContainers"]:
        foreach.append({"list": f"request.object.spec.{field}[]", "patchStrategicMerge": {"spec": {field: [
            {"(name)": "{{ element.name }}", "env": env_patch, "volumeMounts": mounts(config)}]}}})
    container_rule = rule("container-configuration", {"foreach": foreach})
    return resource("kyverno.io/v1", "ClusterPolicy", "roachmotel-pods", spec={
        "background": False, "failurePolicy": "Fail", "webhookTimeoutSeconds": 10,
        "rules": [volume_rule, wait_rule, container_rule]})


def infrastructure(config):
    infra, cm, domain = config["infrastructureNamespace"], config["certManagerNamespace"], config["clusterDomain"]
    names = ["root", "node", "roachmotel_provisioner"]
    dns = ["localhost", "127.0.0.1"]
    for base in ["cockroachdb-public", "*.cockroachdb", "cockroachdb-join"]:
        dns.extend([base, f"{base}.{infra}", f"{base}.{infra}.svc.{domain}"])
    docs = [resource("v1", "Namespace", n) for n in [infra, cm, config["kyvernoNamespace"]]]
    for name in names:
        docs.append(approval(f"roachmotel-infra-{name}", infra, name, "720h",
                             dns=dns if name == "node" else None, infrastructure=True))
    docs.append(role("roachmotel-infra-approval", [{"apiGroups": ["policy.cert-manager.io"],
        "resources": ["certificaterequestpolicies"], "verbs": ["use"],
        "resourceNames": [f"roachmotel-infra-{n}" for n in names]}]))
    docs.append(binding("roachmotel-infra-approval", "roachmotel-infra-approval", [{
        "kind": "ServiceAccount", "name": "cert-manager", "namespace": cm}]))
    docs.append(resource("cert-manager.io/v1", "ClusterIssuer", ISSUER["name"], spec={"ca": {"secretName": "roachmotel-ca"}}))
    docs.append(resource("cert-manager.io/v1", "Certificate", "roachmotel-provisioner", infra, spec={
        "commonName": "roachmotel_provisioner", "secretName": "roachmotel-provisioner", "issuerRef": ISSUER,
        "duration": "720h", "renewBefore": "240h", "usages": ["digital signature", "key encipherment", "client auth"]}))
    docs.append(resource("v1", "Secret", "roachmotel-connection", infra, type="Opaque",
                         stringData={"username": "roachmotel_provisioner", "password": ""}))
    docs.append(resource("dbops.dbprovision.io/v1alpha1", "DatabaseInstance", "roachmotel", infra, spec={
        "engine": "cockroachdb", "connection": {
            "host": f"cockroachdb-public.{infra}.svc.{domain}", "port": 26257, "database": "defaultdb",
            "secretRef": {"name": "roachmotel-connection"}},
        "tls": {"enabled": True, "mode": "verify-full", "secretRef": {"name": "roachmotel-provisioner",
            "keys": {"ca": "ca.crt", "cert": "tls.crt", "key": "tls.key"}}}}))
    # Extend Kyverno's built-in roles by aggregation. Generate RBAC is intentionally
    # privileged and must remain restricted to the platform's policy administrators.
    docs.append(role("roachmotel-kyverno-background", [
        {"apiGroups": ["dbops.dbprovision.io"], "resources": ["databases", "databaseusers"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
        {"apiGroups": [""], "resources": ["configmaps", "secrets"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
    ]))
    docs[-1]["metadata"]["labels"]["rbac.kyverno.io/aggregate-to-background-controller"] = "true"
    return docs


def protection_policy(config):
    # Namespace administrators must not be able to submit privileged initSQL or
    # cross-namespace instance references through the database operator's CRDs.
    kinds = ["Database", "DatabaseInstance", "DatabaseUser", "DatabaseRole", "DatabaseGrant",
             "DatabaseBackup", "DatabaseBackupSchedule", "DatabaseRestore"]
    return resource("kyverno.io/v1", "ClusterPolicy", "roachmotel-protect-database-api", spec={
        "validationFailureAction": "Enforce", "background": False, "failurePolicy": "Fail",
        "rules": [{"name": "database-api-is-platform-only", "match": {"any": [{"resources": {
            "kinds": [f"dbops.dbprovision.io/v1alpha1/{k}" for k in kinds], "operations": ["CREATE", "UPDATE"]}}]},
            "exclude": {"any": [{"resources": {"namespaces": [config["infrastructureNamespace"]]}}]},
            "validate": {"message": "Database provisioning resources are restricted to the Roachmotel infrastructure namespace.",
                         "deny": {}}}]})


def helm_values(config, image):
    infra = config["infrastructureNamespace"]
    pvc = {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": config["storageSize"]}}}
    if config["storageClass"]:
        pvc["storageClassName"] = config["storageClass"]
    return {
        "cert-manager": {"crds": {"enabled": True}, "disableAutoApproval": True,
                         "serviceAccount": {"name": "cert-manager"}},
        "approver-policy": {"crds": {"enabled": True}, "app": {
            "approveSignerNames": ["clusterissuers.cert-manager.io/roachmotel-ca"]}},
        "kyverno": {"admissionController": {"replicas": 3}, "backgroundController": {"replicas": 2}},
        "cockroach-operator": {"numReplicas": 3, "cloudRegion": config["region"], "watchNamespaces": infra},
        "db-provision-operator": {"replicaCount": 2, "leaderElect": True, "instanceId": "roachmotel",
                                  "metrics": {"enabled": True}},
        "cockroachdb": {"k8s": {"fullnameOverride": "cockroachdb"}, "cockroachdb": {
            "clusterDomain": config["clusterDomain"],
            "tls": {"enabled": True, "selfSigner": {"enabled": False}, "certManager": {"enabled": True,
                "caConfigMap": "roachmotel-trust", "issuer": {**ISSUER, "clientCertDuration": "720h",
                    "clientCertExpiryWindow": "240h", "nodeCertDuration": "720h", "nodeCertExpiryWindow": "240h"}}},
            "crdbCluster": {"image": {"name": image}, "dataStore": {"volumeClaimTemplate": {"spec": pvc}},
                "startFlags": {} if config["workloadTLS"] else {"upsert": ["--accept-sql-without-tls"]},
                "regions": [{"code": config["region"], "nodes": 3, "cloudProvider": "", "namespace": infra}],
                "postInitSQL": {"inline": ["CREATE USER IF NOT EXISTS roachmotel_provisioner;",
                    "GRANT admin TO roachmotel_provisioner;",
                    "REVOKE ALL ON DATABASE defaultdb FROM public;", "REVOKE ALL ON SCHEMA defaultdb.public FROM public;",
                    "REVOKE ALL ON DATABASE postgres FROM public;", "REVOKE ALL ON SCHEMA postgres.public FROM public;"]},
                "podTemplate": {"spec": {"topologySpreadConstraints": [{"maxSkew": 1,
                    "topologyKey": "kubernetes.io/hostname", "whenUnsatisfiable": "DoNotSchedule",
                    "labelSelector": {"matchLabels": {"app.kubernetes.io/component": "cockroachdb"}}}],
                    "containers": [{"name": "cockroachdb", "resources": {
                        "requests": {"cpu": "2", "memory": "4Gi"}, "limits": {"memory": "8Gi"}}}]}},
                "persistentVolumeClaimRetentionPolicy": {"whenDeleted": "Retain", "whenScaled": "Retain"}}}},
    }


def build(config, dependencies):
    validate_config(config)
    policies = namespace_policy(config)
    for rule in policies["spec"]["rules"]:
        if rule["generate"]["kind"] in ["Database", "DatabaseUser"]:
            rule["generate"]["data"]["metadata"]["labels"]["dbops.dbprovision.io/operator-instance-id"] = "roachmotel"
    docs = infrastructure(config)
    instance = next(doc for doc in docs if doc["kind"] == "DatabaseInstance")
    instance["metadata"]["labels"]["dbops.dbprovision.io/operator-instance-id"] = "roachmotel"
    namespaces = [d for d in docs if d["kind"] == "Namespace"]
    docs = [d for d in docs if d["kind"] != "Namespace"]
    return {"namespaces.yaml": namespaces, "infrastructure.yaml": docs, "namespace-policy.yaml": [policies],
            "protection-policy.yaml": [protection_policy(config)],
            "pod-policy.yaml": [pod_policy(config, dependencies["images"]["cockroachdb"])],
            **{f"values/{name}.yaml": [values] for name, values in helm_values(config, dependencies["images"]["cockroachdb"]).items()}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=Path("config.yaml"))
    parser.add_argument("--dependencies", type=Path, default=Path("dependencies.yaml"))
    parser.add_argument("--output", type=Path, default=Path("build"))
    parser.add_argument("--identity", help="print the SQL identity for a namespace")
    args = parser.parse_args()
    if args.identity:
        print(identity(args.identity))
        return
    config, deps = yaml.safe_load(args.config.read_text()), yaml.safe_load(args.dependencies.read_text())
    artifacts = build(config, deps)
    for name, docs in artifacts.items():
        target = args.output / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("# Generated by python3 -m roachmotel.render; do not edit.\n" + yaml.safe_dump_all(docs, sort_keys=False))
    print(f"Rendered {len(artifacts)} files to {args.output}; nothing applied to Kubernetes.")


if __name__ == "__main__":
    main()

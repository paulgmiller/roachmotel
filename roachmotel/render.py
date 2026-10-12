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
POLICY = "roachmotel-{{ sql_name }}"
TLS_PATH = "/var/run/cockroachdb"
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
    hours = lambda v: int(v[:-1]) if re.fullmatch(r"[1-9][0-9]*h", v) else 0
    duration, renewal = hours(config["certificateDuration"]), hours(config["renewBefore"])
    if not 0 < renewal < duration <= 24:
        raise ValueError("require 0 < renewBefore < certificateDuration <= 24h")
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
        f'ALTER USER "{sql_name}" WITH PASSWORD NULL;',
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
    policy = approval(POLICY, NS, IDENTITY, config["certificateDuration"],
                      uris=[f"spiffe://roachmotel.local/ns/{NS}/pod/*"])
    group = [{"kind": "Group", "name": f"system:serviceaccounts:{NS}", "apiGroup": "rbac.authorization.k8s.io"}]
    use_role = role(POLICY, [{"apiGroups": ["policy.cert-manager.io"],
        "resources": ["certificaterequestpolicies"], "resourceNames": [POLICY], "verbs": ["use"]}])
    request_role = role("roachmotel-certificate-request", [{"apiGroups": ["cert-manager.io"],
        "resources": ["certificaterequests"], "verbs": ["create"]}], NS)
    objs = [("database", database, False), ("approval-policy", policy, False),
            ("approval-use-role", use_role, False),
            ("approval-use-binding", binding(POLICY, POLICY, group), False),
            ("request-role", request_role, True),
            ("request-binding", binding("roachmotel-certificate-request", "roachmotel-certificate-request", group,
                                       NS, cluster_role=False), True)]
    rules = [generate_rule(name, obj, config, sync) for name, obj, sync in objs]
    clone = generate_rule("server-trust", resource("v1", "ConfigMap", "roachmotel-trust", NS), config, True)
    del clone["generate"]["data"]
    clone["generate"]["clone"] = {"namespace": infra, "name": "roachmotel-trust"}
    rules.append(clone)
    return resource("kyverno.io/v1", "ClusterPolicy", "roachmotel-namespaces", spec={"rules": rules})


def connection_env(config):
    host = f"cockroachdb-public.{config['infrastructureNamespace']}.svc.{config['clusterDomain']}"
    url = (f"postgresql://{IDENTITY}@{host}:26257/{IDENTITY}?sslmode=verify-full"
           f"&sslrootcert={TRUST_PATH}/ca.crt&sslcert={TLS_PATH}/tls.crt&sslkey={TLS_PATH}/tls.key")
    return [{"name": k, "value": v} for k, v in {
        "PGHOST": host, "PGPORT": "26257", "PGDATABASE": IDENTITY, "PGUSER": IDENTITY,
        "PGSSLMODE": "verify-full", "PGSSLROOTCERT": f"{TRUST_PATH}/ca.crt",
        "PGSSLCERT": f"{TLS_PATH}/tls.crt", "PGSSLKEY": f"{TLS_PATH}/tls.key",
        "ROACHMOTEL_DATABASE_URL": url,
    }.items()]


def mounts():
    return [{"name": "roachmotel-client", "mountPath": TLS_PATH, "readOnly": True},
            {"name": "roachmotel-trust", "mountPath": TRUST_PATH, "readOnly": True}]


def pod_policy(config, image):
    match = {"any": [{"resources": {"kinds": ["Pod"], "operations": ["CREATE"]}}]}
    exclude = {"any": [{"resources": {"namespaces": excluded(config)}}]}
    def rule(name, mutation):
        return {"name": name, "match": match, "exclude": exclude,
                "context": namespace_context(pod=True), "mutate": mutation}
    attrs = {"csi.cert-manager.io/issuer-name": ISSUER["name"],
             "csi.cert-manager.io/issuer-kind": ISSUER["kind"],
             "csi.cert-manager.io/common-name": IDENTITY,
             "csi.cert-manager.io/uri-sans": "spiffe://roachmotel.local/ns/${POD_NAMESPACE}/pod/${POD_NAME}/${POD_UID}",
             "csi.cert-manager.io/key-usages": "digital signature,key encipherment,client auth",
             "csi.cert-manager.io/duration": config["certificateDuration"],
             "csi.cert-manager.io/renew-before": config["renewBefore"],
             "csi.cert-manager.io/fs-group": "{{ to_string(request.object.spec.securityContext.fsGroup || `" + str(config["defaultFsGroup"]) + "`) }}"}
    volume_rule = rule("volumes", {"patchStrategicMerge": {
        "metadata": {"annotations": {"roachmotel.dev/database": IDENTITY}},
        "spec": {"securityContext": {"+(fsGroup)": config["defaultFsGroup"]}, "volumes": [
            {"name": "roachmotel-client", "csi": {"driver": "csi.cert-manager.io", "readOnly": True, "volumeAttributes": attrs}},
            {"name": "roachmotel-trust", "configMap": {"name": "roachmotel-trust"}},
        ]}}})
    wait = {"name": "roachmotel-wait", "image": image, "command": ["/bin/sh", "-ec"],
            "args": ['until /cockroach/cockroach sql --url="$ROACHMOTEL_DATABASE_URL" --execute="SELECT 1"; do sleep 5; done'],
            "env": connection_env(config), "volumeMounts": mounts(),
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
    env_patch = [{**item, "valueFrom": None} for item in connection_env(config)]
    for field in ["containers", "initContainers"]:
        foreach.append({"list": f"request.object.spec.{field}[]", "patchStrategicMerge": {"spec": {field: [
            {"(name)": "{{ element.name }}", "env": env_patch, "volumeMounts": mounts()}]}}})
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
        {"apiGroups": ["dbops.dbprovision.io"], "resources": ["databases"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
        {"apiGroups": ["policy.cert-manager.io"], "resources": ["certificaterequestpolicies"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete", "use"]},
        {"apiGroups": ["cert-manager.io"], "resources": ["certificaterequests"], "verbs": ["create"]},
        {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
        {"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["roles", "rolebindings", "clusterroles", "clusterrolebindings"],
         "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
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
        "cert-manager-csi-driver": {"app": {"driver": {"useTokenRequest": True, "continueOnNotReady": False}}},
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
    policies["spec"]["rules"][0]["generate"]["data"]["metadata"]["labels"]["dbops.dbprovision.io/operator-instance-id"] = "roachmotel"
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

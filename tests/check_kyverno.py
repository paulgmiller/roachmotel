"""Exercise the real pinned Kyverno engine, not an imitation of its mutation rules."""
import os
from pathlib import Path
import subprocess
import tempfile

import yaml

from roachmotel.render import identity, pod_policy

ROOT = Path(__file__).resolve().parents[1]
KYVERNO = os.environ.get("KYVERNO", "kyverno")


def apply(policy, source, target):
    result = subprocess.run([KYVERNO, "apply", str(policy), "--resource", str(source),
        "--set", "request.operation=CREATE", "--output", str(target)], text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(result.stdout + result.stderr)
    # Some CLI versions return an error result while exiting zero.
    if "error: 0" not in result.stdout or "fail: 0" not in result.stdout:
        raise RuntimeError(result.stdout + result.stderr)
    return [d for d in yaml.safe_load_all(target.read_text()) if d]


def main():
    with tempfile.TemporaryDirectory(prefix="roachmotel-policy-") as tmp:
        out = Path(tmp)
        docs = apply(ROOT / "build/pod-policy.yaml", ROOT / "tests/fixtures/pods.yaml", out / "pods.yaml")
        apps = {d["metadata"]["name"]: d for d in docs}
        for name, namespace, group in [("app", "tenant-a", 10001), ("simple", "tenant-b", 2000)]:
            pod = apps[name]
            assert pod["spec"]["securityContext"]["fsGroup"] == group
            assert pod["spec"]["initContainers"][0]["name"] == "roachmotel-wait"
            assert pod["metadata"]["annotations"]["roachmotel.dev/database"] == identity(namespace)
            for container in pod["spec"]["containers"] + pod["spec"]["initContainers"]:
                env = {e["name"]: e for e in container["env"]}
                assert env["ROACHMOTEL_DATABASE_URL"]["valueFrom"]["secretKeyRef"] == {
                    "name": "roachmotel-credentials", "key": "connection-string"}
                assert not env["ROACHMOTEL_DATABASE_URL"].get("value")
                assert env["ROACHMOTEL_DATABASE_URL_FILE"]["value"] == "/var/run/roachmotel/connection-string"
                assert len(container["volumeMounts"]) == 2
            secret_volume = next(v for v in pod["spec"]["volumes"] if v["name"] == "roachmotel-credentials")
            assert secret_volume["secret"]["secretName"] == "roachmotel-credentials"
            assert secret_volume["secret"]["defaultMode"] == 0o440
            assert not any("csi" in v for v in pod["spec"]["volumes"])
        assert [c["name"] for c in apps["app"]["spec"]["initContainers"]] == ["roachmotel-wait", "migrations"]
        assert next(e["value"] for e in apps["app"]["spec"]["containers"][0]["env"] if e["name"] == "APPLICATION_SETTING") == "preserved"
        if "infrastructure" in apps:
            assert "volumes" not in apps["infrastructure"]["spec"]
        second = apply(ROOT / "build/pod-policy.yaml", out / "pods.yaml", out / "twice.yaml")
        assert docs == second, "pod injection must be idempotent"
        # Check Go template escaping through the real substitution engine.
        # Full generate rules still need API discovery in a Kubernetes cluster.
        namespace_policy = yaml.safe_load((ROOT / "build/namespace-policy.yaml").read_text())
        rule = namespace_policy["spec"]["rules"][1]
        template = rule["generate"]["data"]["spec"]["passwordSecret"]["secretTemplate"]["data"]["connection-string"]
        policy = {"apiVersion": "kyverno.io/v1", "kind": "ClusterPolicy", "metadata": {"name": "check-template"},
            "spec": {"rules": [{"name": "template", "match": rule["match"], "context": rule["context"],
                "mutate": {"patchStrategicMerge": {"metadata": {"annotations": {"test.roachmotel.dev/url": template}}}}}]}}
        (out / "template-policy.yaml").write_text(yaml.safe_dump(policy))
        rendered = apply(out / "template-policy.yaml", ROOT / "tests/fixtures/namespaces.yaml", out / "namespaces.yaml")
        for namespace in rendered:
            url = namespace["metadata"]["annotations"]["test.roachmotel.dev/url"]
            assert "{{ .Password }}" in url and r"\{{" not in url
            assert identity(namespace["metadata"]["name"]) in url
        config = yaml.safe_load((ROOT / "config.yaml").read_text())
        dependencies = yaml.safe_load((ROOT / "dependencies.yaml").read_text())
        plaintext = pod_policy({**config, "workloadTLS": False}, dependencies["images"]["cockroachdb"])
        (out / "plaintext-policy.yaml").write_text(yaml.safe_dump(plaintext))
        plaintext_pods = apply(out / "plaintext-policy.yaml", ROOT / "tests/fixtures/pods.yaml", out / "plaintext-pods.yaml")
        for pod in plaintext_pods:
            if pod["metadata"]["namespace"] == config["infrastructureNamespace"]:
                continue
            assert len(pod["spec"]["volumes"]) == 1
            assert "secret" in pod["spec"]["volumes"][0]
            for container in pod["spec"]["containers"] + pod["spec"]["initContainers"]:
                assert len(container["volumeMounts"]) == 1
        print("Kyverno mutation passed: credentials, non-root access, init ordering, exclusions, and reinvocation.")
        protection = subprocess.run([KYVERNO, "apply", str(ROOT / "build/protection-policy.yaml"),
            "--resource", str(ROOT / "tests/fixtures/databases.yaml"), "--set", "request.operation=CREATE"],
            text=True, capture_output=True)
        if protection.returncode != 1 or "fail: 1" not in protection.stdout or "error: 0" not in protection.stdout:
            raise RuntimeError(protection.stdout + protection.stderr)
        assert "privileged-sql-attempt" in protection.stdout
        print("Kyverno validation passed: tenant privileged SQL denied; infrastructure resource excluded.")


if __name__ == "__main__":
    main()

"""Exercise the real pinned Kyverno engine, not an imitation of its mutation rules."""
import os
from pathlib import Path
import subprocess
import tempfile

import yaml

from roachmotel.render import identity

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
                env = {e["name"]: e["value"] for e in container["env"]}
                assert env["PGUSER"] == identity(namespace)
                assert env["PGSSLMODE"] == "verify-full"
                assert len(container["volumeMounts"]) == 2
                for item in container["env"]:
                    if item["name"] == "PGUSER":
                        assert not item.get("valueFrom"), "must clear the old environment source"
            csi = pod["spec"]["volumes"][0]["csi"]["volumeAttributes"]
            assert csi["csi.cert-manager.io/fs-group"] == str(group)
            assert csi["csi.cert-manager.io/common-name"] == identity(namespace)
        assert [c["name"] for c in apps["app"]["spec"]["initContainers"]] == ["roachmotel-wait", "migrations"]
        assert next(e["value"] for e in apps["app"]["spec"]["containers"][0]["env"] if e["name"] == "APPLICATION_SETTING") == "preserved"
        if "infrastructure" in apps:
            assert "volumes" not in apps["infrastructure"]["spec"]
        second = apply(ROOT / "build/pod-policy.yaml", out / "pods.yaml", out / "twice.yaml")
        assert docs == second, "pod injection must be idempotent"
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

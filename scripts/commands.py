"""Print reviewable installation commands. Never execute them or read kubeconfig."""
import argparse
from pathlib import Path
import shlex

import yaml


def command(*args):
    print(shlex.join(map(str, args)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", choices=["controllers", "cluster"])
    parser.add_argument("--config", type=Path, default=Path("config.yaml"))
    args = parser.parse_args()
    config = yaml.safe_load(args.config.read_text())
    deps = yaml.safe_load(Path("dependencies.yaml").read_text())
    names = ["cert-manager", "approver-policy", "cert-manager-csi-driver", "kyverno",
             "cockroach-operator", "db-provision-operator"] if args.stage == "controllers" else ["cockroachdb"]
    if args.stage == "controllers":
        command("kubectl", "apply", "-f", "build/namespaces.yaml")
    for name in names:
        release = deps["releases"][name]
        namespace = config["certManagerNamespace"] if name in ["cert-manager", "approver-policy", "cert-manager-csi-driver"] else (
            config["kyvernoNamespace"] if name == "kyverno" else config["infrastructureNamespace"])
        command("helm", "upgrade", "--install", name, release["chart"], "--version", release["version"],
                "--namespace", namespace, "--values", "build/values/" + name + ".yaml", "--wait", "--timeout", "10m")
    if args.stage == "cluster":
        command("kubectl", "wait", "--for=jsonpath={.status.phase}=Ready", "databaseinstance/roachmotel",
                "--namespace", config["infrastructureNamespace"], "--timeout=10m")


if __name__ == "__main__":
    main()

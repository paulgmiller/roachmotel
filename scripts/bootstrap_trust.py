"""Install an administrator-provided CA without copying its key to tenant namespaces.

An explicit invocation changes Kubernetes. Secrets travel on stdin and are never
printed. The CA is provided externally rather than generated during rendering.
"""
import argparse
import base64
from pathlib import Path
import subprocess

import yaml

from roachmotel.render import resource


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ca-cert", type=Path, required=True)
    parser.add_argument("--ca-key", type=Path, required=True)
    parser.add_argument("--config", type=Path, default=Path("config.yaml"))
    args = parser.parse_args()
    config = yaml.safe_load(args.config.read_text())
    cert = args.ca_cert.read_bytes()
    key = args.ca_key.read_bytes()
    if b"BEGIN CERTIFICATE" not in cert or b"PRIVATE KEY" not in key:
        parser.error("expected PEM certificate and private key")
    cm, infra = config["certManagerNamespace"], config["infrastructureNamespace"]
    # Never accidentally replace an existing authority and invalidate a cluster.
    exists = subprocess.run(["kubectl", "get", "secret", "roachmotel-ca", "-n", cm,
                             "--ignore-not-found", "-o", "name"], capture_output=True, text=True, check=True)
    if exists.stdout.strip():
        parser.error("roachmotel-ca already exists; CA rotation requires a separate coordinated procedure")
    secret = resource("v1", "Secret", "roachmotel-ca", cm, type="kubernetes.io/tls", data={
        "tls.crt": base64.b64encode(cert).decode(), "tls.key": base64.b64encode(key).decode()})
    # CREATE avoids a race overwriting the CA if another administrator installed it.
    subprocess.run(["kubectl", "create", "-f", "-"], input=yaml.safe_dump(secret), text=True, check=True)
    trust = resource("v1", "ConfigMap", "roachmotel-trust", infra, data={"ca.crt": cert.decode()})
    subprocess.run(["kubectl", "apply", "-f", "-"], input=yaml.safe_dump(trust), text=True, check=True)
    print("Installed infrastructure CA Secret and public trust ConfigMap; private key was not logged.")


if __name__ == "__main__":
    main()

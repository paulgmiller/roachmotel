"""Validate rendered custom resources against downloaded, pinned upstream CRDs.

Example: PYTHONPATH=. python3 tests/check_schemas.py --crd-dir /path/to/charts
Requires jsonschema (see requirements-test.txt). This catches fields which the
API server would otherwise prune silently. CEL validations still need Kubernetes.
"""
import argparse
import copy
from pathlib import Path

import jsonschema
import yaml

from roachmotel.render import identity


def strict(schema):
    schema = copy.deepcopy(schema)
    def visit(node):
        if not isinstance(node, dict):
            return
        if "properties" in node and "additionalProperties" not in node:
            node["additionalProperties"] = False
        for value in node.values():
            if isinstance(value, dict):
                visit(value)
            elif isinstance(value, list):
                for item in value:
                    visit(item)
    visit(schema)
    return schema


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--crd-dir", type=Path, action="append", required=True)
    args = parser.parse_args()
    schemas = {}
    for directory in args.crd_dir:
        for path in directory.rglob("*.yaml"):
            try:
                docs = list(yaml.safe_load_all(path.read_text()))
            except yaml.YAMLError:
                continue  # Unrendered Helm templates are not CRDs.
            for crd in docs:
                if not isinstance(crd, dict) or crd.get("kind") != "CustomResourceDefinition":
                    continue
                for version in crd["spec"]["versions"]:
                    schema = version.get("schema", {}).get("openAPIV3Schema")
                    if schema:
                        schemas[(crd["spec"]["group"] + "/" + version["name"], crd["spec"]["names"]["kind"])] = strict(schema)
    docs = []
    for name in ["infrastructure.yaml", "namespace-policy.yaml", "pod-policy.yaml", "protection-policy.yaml"]:
        docs.extend(yaml.safe_load_all((Path("build") / name).read_text()))
    generated = []
    for doc in docs:
        if doc.get("kind") == "ClusterPolicy":
            for rule in doc["spec"]["rules"]:
                data = rule.get("generate", {}).get("data")
                if data:
                    text = yaml.safe_dump(data).replace("{{ ns_name }}", "tenant-a").replace("{{ sql_name }}", identity("tenant-a"))
                    generated.append(yaml.safe_load(text))
    count = 0
    for doc in docs + generated:
        key = (doc["apiVersion"], doc["kind"])
        if doc["apiVersion"].split("/")[0] in ["dbops.dbprovision.io", "policy.cert-manager.io", "kyverno.io"]:
            if key not in schemas:
                raise RuntimeError("Missing pinned CRD for " + str(key))
            jsonschema.Draft7Validator(schemas[key]).validate(doc)
            count += 1
    print(f"Validated {count} custom resources against upstream schemas with unknown fields rejected.")


if __name__ == "__main__":
    main()

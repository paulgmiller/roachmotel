import copy
from pathlib import Path
import unittest

import yaml

from roachmotel.render import build, excluded, identity, validate_config

ROOT = Path(__file__).resolve().parents[1]


class RenderTests(unittest.TestCase):
    def setUp(self):
        self.config = yaml.safe_load((ROOT / "config.yaml").read_text())
        self.deps = yaml.safe_load((ROOT / "dependencies.yaml").read_text())
        self.artifacts = build(self.config, self.deps)

    def test_namespace_identity_handles_kubernetes_names_without_sql_collisions(self):
        names = ["default", "123", "a-b", "ab", "a" * 63]
        ids = [identity(n) for n in names]
        self.assertEqual(len(ids), len(set(ids)))
        for name in ids:
            self.assertRegex(name, r"^rm_[0-9a-f]{32}$")
            self.assertLessEqual(len(name), 63)
        for bad in ["A", "bad_name", "-bad", "a" * 64, 'x";DROP USER root;--']:
            with self.assertRaises(ValueError):
                identity(bad)

    def test_configuration_rejects_unsafe_values_and_shared_infrastructure(self):
        for key, value in [("workloadTLS", "false"),
                           ("defaultFsGroup", 0), ("clusterDomain", "x'; DROP DATABASE x;")]:
            config = copy.deepcopy(self.config)
            config[key] = value
            with self.assertRaises(ValueError):
                validate_config(config)
        config = copy.deepcopy(self.config)
        config["infrastructureNamespace"] = config["certManagerNamespace"]
        with self.assertRaises(ValueError):
            validate_config(config)

    def test_retention_resources_are_outside_the_tenant_namespace(self):
        rules = self.artifacts["namespace-policy.yaml"][0]["spec"]["rules"]
        db = rules[0]["generate"]
        self.assertEqual(db["namespace"], self.config["infrastructureNamespace"])
        self.assertFalse(db["synchronize"])
        self.assertNotIn("ownerReferences", db["data"]["metadata"])
        self.assertEqual(db["data"]["spec"]["deletionPolicy"], "Retain")
        self.assertEqual(db["data"]["spec"]["initSQL"]["failurePolicy"], "Block")
        sql = db["data"]["spec"]["initSQL"]["inline"][0]
        self.assertTrue(sql.startswith("BEGIN;"))
        self.assertTrue(sql.endswith("COMMIT;"))
        self.assertNotIn("PASSWORD", sql, "bootstrap must not reset managed passwords")
        self.assertNotIn("GRANT admin", sql)

    def test_one_retained_password_user_and_connection_string_per_namespace(self):
        rules = self.artifacts["namespace-policy.yaml"][0]["spec"]["rules"]
        user = rules[1]["generate"]["data"]
        self.assertEqual(user["spec"]["username"], "{{ sql_name }}")
        self.assertEqual(user["metadata"]["namespace"], self.config["infrastructureNamespace"])
        self.assertEqual(user["metadata"]["annotations"]["dbops.dbprovision.io/deletion-policy"], "Retain")
        self.assertFalse(user["spec"]["passwordRotation"]["enabled"])
        password = user["spec"]["passwordSecret"]
        self.assertFalse(password["includeSpecialChars"])
        self.assertEqual(password["length"], 48)
        url = password["secretTemplate"]["data"]["connection-string"]
        self.assertIn(r"\{{ .Password }}", url)
        self.assertIn("sslmode=verify-full", url)
        self.assertNotIn("sslcert", url)
        self.assertNotIn("sslkey", url)
        clone = rules[2]["generate"]
        self.assertEqual(clone["kind"], "Secret")
        self.assertEqual(clone["name"], "roachmotel-credentials")
        self.assertEqual(clone["clone"], {"namespace": self.config["infrastructureNamespace"], "name": "{{ ns_name }}-credentials"})
        self.assertTrue(clone["synchronize"])

    def test_no_workload_pki_or_infrastructure_private_keys_are_distributed(self):
        rules = self.artifacts["namespace-policy.yaml"][0]["spec"]["rules"]
        self.assertEqual([r["generate"]["kind"] for r in rules], ["Database", "DatabaseUser", "Secret", "ConfigMap"])
        self.assertNotIn("values/cert-manager-csi-driver.yaml", self.artifacts)
        for rule in rules:
            self.assertEqual(rule["exclude"]["any"][0]["resources"]["names"], excluded(self.config))

    def test_plaintext_is_explicit_and_keeps_cluster_authentication_enabled(self):
        config = {**self.config, "workloadTLS": False}
        artifacts = build(config, self.deps)
        values = artifacts["values/cockroachdb.yaml"][0]["cockroachdb"]
        self.assertTrue(values["tls"]["enabled"])
        self.assertEqual(values["crdbCluster"]["startFlags"]["upsert"], ["--accept-sql-without-tls"])
        rules = artifacts["namespace-policy.yaml"][0]["spec"]["rules"]
        self.assertNotIn("ConfigMap", [r["generate"]["kind"] for r in rules])
        url = rules[1]["generate"]["data"]["spec"]["passwordSecret"]["secretTemplate"]["data"]["connection-string"]
        self.assertIn("sslmode=disable", url)
        self.assertNotIn("sslrootcert", url)

    def test_three_replicas_are_managed_by_the_official_operator(self):
        values = self.artifacts["values/cockroachdb.yaml"][0]["cockroachdb"]
        self.assertTrue(values["tls"]["enabled"])
        self.assertTrue(values["tls"]["certManager"]["enabled"])
        self.assertFalse(values["tls"]["selfSigner"]["enabled"])
        self.assertEqual(values["crdbCluster"]["regions"][0]["nodes"], 3)
        topology = values["crdbCluster"]["podTemplate"]["spec"]["topologySpreadConstraints"][0]
        self.assertEqual(topology["topologyKey"], "kubernetes.io/hostname")
        self.assertEqual(topology["whenUnsatisfiable"], "DoNotSchedule")

    def test_tls_fields_match_the_pinned_database_instance_api(self):
        instance = next(d for d in self.artifacts["infrastructure.yaml"] if d["kind"] == "DatabaseInstance")
        self.assertNotIn("sslMode", instance["spec"]["connection"])
        self.assertTrue(instance["spec"]["tls"]["enabled"])
        self.assertEqual(instance["spec"]["tls"]["mode"], "verify-full")
        self.assertEqual(instance["metadata"]["labels"]["dbops.dbprovision.io/operator-instance-id"], "roachmotel")


if __name__ == "__main__":
    unittest.main()

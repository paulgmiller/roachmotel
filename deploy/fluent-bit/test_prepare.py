import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).parent
spec = importlib.util.spec_from_file_location("prepare", ROOT / "prepare.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class PrepareTest(unittest.TestCase):
    def test_generated_credentials_and_tls_without_password_in_config(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            password = "ab" * 32
            secret = root / "connection-string"
            secret.write_text(f"postgresql://logs:{password}@db.database.svc:26257/logs?sslmode=require&connect_timeout=5")
            module.prepare(secret, ROOT / "fluent-bit.conf", root / "generated")
            config = (root / "generated/fluent-bit.conf").read_text()
            service = root / "generated/pg_service.conf"
            self.assertNotIn(password, config)
            self.assertNotIn("__DB_", config)
            self.assertIn("db.database.svc", config)
            self.assertIn("password=" + password, service.read_text())
            self.assertIn("sslmode=require", service.read_text())
            self.assertIn("connect_timeout=5", service.read_text())
            self.assertEqual(service.stat().st_mode & 0o777, 0o600)

    def test_refuses_wrong_database_insecure_and_injected_options(self):
        password = "ab" * 32
        for suffix in ("rm_old?sslmode=require", "logs?sslmode=disable",
                       "logs?sslmode=require&user=root", "logs?sslmode=require&sslmode=disable"):
            with self.subTest(suffix=suffix), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                secret = root / "connection-string"
                secret.write_text(f"postgresql://logs:{password}@db.database.svc:26257/" + suffix)
                with self.assertRaises(ValueError):
                    module.prepare(secret, ROOT / "fluent-bit.conf", root / "generated")
                self.assertFalse((root / "generated").exists())


if __name__ == "__main__":
    unittest.main()

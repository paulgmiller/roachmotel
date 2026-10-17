import importlib.util
import json
from io import BytesIO
from pathlib import Path
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock

spec = importlib.util.spec_from_file_location("writer", Path(__file__).with_name("writer.py"))
writer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(writer)


class WriterTest(unittest.TestCase):
    def batch(self):
        return json.dumps([{"__fluentbit_time": "2026-10-17T20:00:00Z", "log": "hello"}])

    def test_namespace_tables_are_distinct_and_names_preserved(self):
        for namespace in ("default", "kube-system", "kubesystem", "logs"):
            with self.subTest(namespace=namespace):
                connect = MagicMock()
                cursor = connect.return_value.__enter__.return_value.cursor.return_value.__enter__.return_value
                tag = f"kube.var.log.containers.pod-1_{namespace}_container-abc.log"
                writer.write_batch(connect, tag, self.batch())
                create = cursor.execute.call_args.args[0]
                insert, rows = cursor.executemany.call_args.args
                self.assertIn(f'public."{namespace}"', create)
                self.assertIn(f'public."{namespace}"', insert)
                self.assertEqual(rows[0][0], tag)
                self.assertEqual(json.loads(rows[0][2])["log"], "hello")
                # Existing tables' TTL settings are never changed on subsequent batches.
                self.assertNotIn("ALTER TABLE", create)

    def test_routes_by_source_filename_even_if_metadata_is_missing_or_spoofed(self):
        connect = MagicMock()
        payload = json.loads(self.batch())
        payload[0]["kubernetes"] = {"namespace_name": "other"}
        writer.write_batch(connect, "kube.var.log.containers.pod_real_container.log", json.dumps(payload))
        cursor = connect.return_value.__enter__.return_value.cursor.return_value.__enter__.return_value
        self.assertIn('public."real"', cursor.execute.call_args.args[0])

    def test_rejects_unsafe_names_and_invalid_batches_before_sql(self):
        for tag, payload in (
            ('kube.var.log.containers.pod_bad"name_container.log', self.batch()),
            ("unknown", self.batch()),
            ("kube.var.log.containers.pod_default_container.log", "{}"),
            ("kube.var.log.containers.pod_default_container.log", '[{"log":"missing timestamp"}]'),
            ("kube.var.log.containers.pod_default_container.log", '[{"__fluentbit_time":"2026-10-17"}]'),
        ):
            connect = MagicMock()
            with self.subTest(tag=tag, payload=payload), self.assertRaises(ValueError):
                writer.write_batch(connect, tag, payload)
            connect.assert_not_called()

    def test_sql_failures_propagate_for_http_retry_and_transaction_rollback(self):
        connect = MagicMock()
        conn = connect.return_value.__enter__.return_value
        cursor = conn.cursor.return_value.__enter__.return_value
        cursor.executemany.side_effect = RuntimeError("database unavailable")
        with self.assertRaises(RuntimeError):
            writer.write_batch(connect, "kube.var.log.containers.pod_default_container.log", self.batch())
        self.assertIs(connect.return_value.__exit__.call_args.args[0], RuntimeError)

    def test_http_acknowledges_success_and_retries_failed_storage(self):
        for failure, status in ((False, 204), (True, 503)):
            with self.subTest(failure=failure):
                handler = object.__new__(writer.Handler)
                payload = self.batch().encode()
                handler.path = "/logs"
                handler.headers = {"Content-Length": str(len(payload)),
                                   "X-Log-Tag": "kube.var.log.containers.pod_default_container.log"}
                handler.rfile = BytesIO(payload)
                connect = MagicMock()
                if failure:
                    connect.side_effect = RuntimeError("private SQL error")
                handler.server = SimpleNamespace(connect=connect, slots=threading.BoundedSemaphore(1))
                handler.send_response = MagicMock()
                handler.send_error = MagicMock()
                handler.end_headers = MagicMock()
                handler.do_POST()
                if failure:
                    handler.send_error.assert_called_once_with(status, "Log storage unavailable; retry batch")
                    handler.send_response.assert_not_called()
                else:
                    handler.send_response.assert_called_once_with(status)
                    handler.send_error.assert_not_called()


if __name__ == "__main__":
    unittest.main()

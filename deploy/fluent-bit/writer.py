"""Receive Fluent Bit batches and commit each batch to its namespace's table."""

from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import threading

PREFIX = "kube.var.log.containers."
MAX_BODY = 16 * 1024 * 1024


def namespace_for(tag):
    if not tag.startswith(PREFIX):
        raise ValueError("invalid source tag")
    parts = tag[len(PREFIX):].split("_")
    if (len(parts) != 3 or not parts[0] or not parts[2].endswith(".log")
            or not re.fullmatch(r"[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?", parts[1])):
        raise ValueError("invalid namespace in source tag")
    return parts[1]


def write_batch(connect, tag, payload):
    namespace = namespace_for(tag)
    records = json.loads(payload)
    if not isinstance(records, list) or not records:
        raise ValueError("expected a nonempty JSON batch")
    rows = []
    for record in records:
        if not isinstance(record, dict):
            raise ValueError("expected a JSON object")
        stamp = record.get("__fluentbit_time")
        if not isinstance(stamp, str):
            raise ValueError("missing event timestamp")
        timestamp = datetime.fromisoformat(stamp.replace("Z", "+00:00"))
        if timestamp.tzinfo is None:
            raise ValueError("event timestamp requires a timezone")
        rows.append((tag, timestamp, json.dumps(record)))
    # Kubernetes namespace names are validated DNS labels; quote them exactly
    # so hyphens work and similarly named namespaces never share a table.
    table = f'public."{namespace}"'
    with connect() as conn:
        with conn.cursor() as cursor:
            cursor.execute(f"CREATE TABLE IF NOT EXISTS {table} "
                           "(tag TEXT NOT NULL, time TIMESTAMPTZ NOT NULL, data JSONB NOT NULL)")
            cursor.executemany(f"INSERT INTO {table} (tag, time, data) VALUES (%s, %s, %s::JSONB)", rows)
    # The connection context commits before HTTP success; failures roll back
    # the whole batch and Fluent Bit retries. A lost response can duplicate rows.


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if self.path == "/healthz" else 404)
        self.end_headers()

    def do_POST(self):
        if self.path != "/logs":
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= MAX_BODY:
                raise ValueError("invalid body length")
            payload = self.rfile.read(length)
            if len(payload) != length:
                raise ValueError("incomplete body")
            # Limit concurrent SQL connections and in-flight memory.
            with self.server.slots:
                write_batch(self.server.connect, self.headers.get("X-Log-Tag", ""), payload)
        except (ValueError, TypeError, UnicodeError):
            self.send_error(400, "Invalid log batch")
            return
        except Exception:
            # SQL errors can include credentials or log payloads. Never expose them.
            self.send_error(503, "Log storage unavailable; retry batch")
            return
        self.send_response(204)
        self.end_headers()

    def log_message(self, *args):
        pass

    def setup(self):
        super().setup()
        self.connection.settimeout(30)


if __name__ == "__main__":
    import psycopg

    def connect():
        # Read the projected Secret again for every batch, including updates.
        uri = Path("/credentials/DATABASE_URL").read_text().strip()
        return psycopg.connect(uri, connect_timeout=10,
                               options="-c statement_timeout=20000")

    server = ThreadingHTTPServer(("0.0.0.0", 8080), Handler)
    server.connect = connect
    server.slots = threading.BoundedSemaphore(4)
    server.serve_forever()

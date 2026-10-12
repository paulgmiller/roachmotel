"""Run secure SQL acceptance checks in an isolated temporary local cluster.

This validates CockroachDB SQL/certificate behavior, not Kubernetes controllers.
Set COCKROACH to the pinned CockroachDB 26.2.3 binary. All keys and state are
temporary, with localhost listeners and process cleanup in a finally block.
"""
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
from urllib.parse import urlencode

from roachmotel.render import bootstrap_sql, identity

COCKROACH = os.environ.get("COCKROACH", "cockroach")


def run(*args):
    result = subprocess.run([COCKROACH, *args], text=True, capture_output=True, timeout=30)
    if result.returncode:
        raise RuntimeError(result.stderr)
    return result.stdout


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    with tempfile.TemporaryDirectory(prefix="roachmotel-sql-") as tmp:
        tmp = Path(tmp)
        certs = tmp / "certs"
        certs.mkdir()
        key = tmp / "ca.key"
        run("cert", "create-ca", f"--certs-dir={certs}", f"--ca-key={key}")
        run("cert", "create-node", "localhost", "127.0.0.1", f"--certs-dir={certs}", f"--ca-key={key}")
        users = ["root", identity("tenant-a"), identity("tenant-b")]
        for user in users:
            run("cert", "create-client", user, f"--certs-dir={certs}", f"--ca-key={key}")
        sql_port, http_port = port(), port()
        def url(user, database, certificate_user=None):
            certificate_user = certificate_user or user
            query = urlencode({"sslmode": "verify-full", "sslrootcert": str(certs / "ca.crt"),
                               "sslcert": str(certs / f"client.{certificate_user}.crt"),
                               "sslkey": str(certs / f"client.{certificate_user}.key")})
            return f"postgresql://{user}@localhost:{sql_port}/{database}?{query}"
        def sql(user, database, statement, certificate_user=None, deny=False, deny_reason=None):
            result = subprocess.run([COCKROACH, "sql", "--url=" + url(user, database, certificate_user),
                                     "--format=csv", "--execute=" + statement], text=True, capture_output=True, timeout=30)
            if deny:
                if not result.returncode:
                    raise AssertionError("Expected permission/authentication rejection: " + statement)
                reasons = [deny_reason] if deny_reason else ["privilege", "permission", "authenticate", "authentication", "certificate"]
                if not any(term in result.stderr.lower() for term in reasons):
                    raise AssertionError("Failed for an unexpected reason: " + result.stderr)
            elif result.returncode:
                raise RuntimeError(result.stderr)
            return result.stdout
        with (tmp / "server.log").open("w") as log:
            server = subprocess.Popen([COCKROACH, "start-single-node", f"--certs-dir={certs}",
                f"--store={tmp / 'store'}", f"--listen-addr=127.0.0.1:{sql_port}",
                f"--http-addr=127.0.0.1:{http_port}", "--cache=64MiB", "--max-sql-memory=64MiB"], stdout=log, stderr=log)
            try:
                deadline = time.monotonic() + 45
                while True:
                    try:
                        sql("root", "defaultdb", "SELECT 1")
                        break
                    except (RuntimeError, subprocess.TimeoutExpired):
                        if server.poll() is not None or time.monotonic() > deadline:
                            raise RuntimeError((tmp / "server.log").read_text())
                        time.sleep(0.25)
                for user in users[1:]:
                    sql("root", "defaultdb", f'CREATE DATABASE "{user}"')
                    sql("root", user, bootstrap_sql(user))
                    sql("root", user, bootstrap_sql(user))  # Required idempotency.
                    sql(user, user, "CREATE TABLE items (id INT PRIMARY KEY, value STRING)")
                    sql(user, user, "INSERT INTO items VALUES (1, 'original'); UPDATE items SET value = 'changed' WHERE id = 1")
                    assert "changed" in sql(user, user, "SELECT value FROM items")
                    sql(user, user, "ALTER TABLE items ADD COLUMN extra INT")
                    sql(user, user, "CREATE SCHEMA extra; CREATE TABLE extra.more (id INT)")
                a, b = users[1:]
                for target in [b + ".public.items", b + ".extra.more"]:
                    sql(a, a, "SELECT * FROM " + target, deny=True)
                    sql(a, a, "INSERT INTO " + target + " (id) VALUES (2)", deny=True)
                    sql(a, a, "ALTER TABLE " + target + " ADD COLUMN stolen INT", deny=True)
                sql(a, a, "CREATE DATABASE unauthorized", deny=True)
                sql(a, a, "CREATE USER unauthorized", deny=True)
                sql(a, a, f'DROP DATABASE "{a}"', deny=True)
                sql("root", "defaultdb", "SELECT 1", certificate_user=a, deny=True)
                sql(a, a, "DELETE FROM items WHERE id = 1")
                sql("root", "defaultdb", 'CREATE DATABASE foreign_database')
                sql("root", "defaultdb", f'ALTER DATABASE foreign_database OWNER TO "{b}"')
                sql("root", "foreign_database", bootstrap_sql("reserved_foreign"), deny=True, deny_reason="division by zero")
                print("Secure CockroachDB SQL passed: idempotent bootstrap, CRUD/DDL, new schemas, isolation, and certificate identity enforcement.")
            finally:
                server.terminate()
                try:
                    server.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait()


if __name__ == "__main__":
    main()

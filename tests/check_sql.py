"""Run secure SQL acceptance checks in an isolated temporary local cluster.

This validates CockroachDB password authentication and SQL isolation, not Kubernetes controllers.
Set COCKROACH to the pinned CockroachDB 26.2.3 binary. All keys and state are
temporary, with localhost listeners and process cleanup in a finally block.
"""
import argparse
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import secrets
import string
from urllib.parse import quote, urlencode

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
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plaintext", action="store_true", help="test explicitly enabled plaintext password authentication")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="roachmotel-sql-") as tmp:
        tmp = Path(tmp)
        certs = tmp / "certs"
        certs.mkdir()
        key = tmp / "ca.key"
        run("cert", "create-ca", f"--certs-dir={certs}", f"--ca-key={key}")
        run("cert", "create-node", "localhost", "127.0.0.1", f"--certs-dir={certs}", f"--ca-key={key}")
        users = ["root", identity("tenant-a"), identity("tenant-b")]
        run("cert", "create-client", "root", f"--certs-dir={certs}", f"--ca-key={key}")
        passwords = {u: ''.join(secrets.choice(string.ascii_letters + string.digits) for _ in range(48)) for u in users[1:]}
        sql_port, http_port = port(), port()
        def url(user, database, password=None):
            if user == "root" and password is None:
                params = {"sslmode": "verify-full", "sslrootcert": str(certs / "ca.crt"),
                          "sslcert": str(certs / "client.root.crt"), "sslkey": str(certs / "client.root.key")}
                credentials = user
            else:
                params = {"sslmode": "disable"} if args.plaintext else {
                    "sslmode": "verify-full", "sslrootcert": str(certs / "ca.crt")}
                credentials = f"{user}:{quote(password if password is not None else passwords[user], safe='')}"
            return f"postgresql://{credentials}@localhost:{sql_port}/{database}?{urlencode(params)}"
        def sql(user, database, statement, password=None, deny=False, deny_reason=None):
            result = subprocess.run([COCKROACH, "sql", "--url=" + url(user, database, password),
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
            command = [COCKROACH, "start-single-node", f"--certs-dir={certs}",
                f"--store={tmp / 'store'}", f"--listen-addr=127.0.0.1:{sql_port}",
                f"--http-addr=127.0.0.1:{http_port}", "--cache=64MiB", "--max-sql-memory=64MiB"]
            if args.plaintext:
                command.append("--accept-sql-without-tls")
            server = subprocess.Popen(command, stdout=log, stderr=log)
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
                    sql("root", "defaultdb", f'CREATE USER "{user}" WITH PASSWORD \'{passwords[user]}\'')
                    sql("root", "defaultdb", f'CREATE DATABASE "{user}"')
                    ready = "SELECT 1 / CASE WHEN has_schema_privilege('public', 'CREATE') AND has_database_privilege(current_database(), 'CREATE') THEN 1 ELSE 0 END"
                    sql(user, user, ready, deny=True, deny_reason="division by zero")
                    sql("root", user, bootstrap_sql(user))
                    sql("root", user, bootstrap_sql(user))  # Required idempotency.
                    sql(user, user, ready)
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
                sql(a, a, "SELECT 1", password="incorrect", deny=True)
                sql(b, b, "SELECT 1", password=passwords[a], deny=True)
                sql("root", "defaultdb", "SELECT 1", password=passwords[a], deny=True)
                sql(a, a, "SELECT 1", password="", deny=True)
                sql(a, a, "DELETE FROM items WHERE id = 1")
                sql("root", "defaultdb", 'CREATE DATABASE foreign_database')
                sql("root", "defaultdb", f'ALTER DATABASE foreign_database OWNER TO "{b}"')
                sql("root", "foreign_database", bootstrap_sql("reserved_foreign"), deny=True, deny_reason="division by zero")
                mode = "explicit plaintext" if args.plaintext else "verified TLS"
                print(f"CockroachDB SQL passed ({mode}): password authentication, readiness gate, idempotent bootstrap, CRUD/DDL, and namespace isolation.")
            finally:
                server.terminate()
                try:
                    server.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait()


if __name__ == "__main__":
    main()

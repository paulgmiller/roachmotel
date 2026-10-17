"""Adapt Roachmotel's connection-string Secret without logging credentials."""

import os
from pathlib import Path
import re
import sys
from urllib.parse import parse_qs, unquote, urlsplit


def prepare(secret, template, output):
    url = urlsplit(Path(secret).read_text().strip())
    options = parse_qs(url.query, strict_parsing=True)
    values = {
        "host": url.hostname or "",
        "port": str(url.port or 5432),
        "dbname": unquote(url.path.removeprefix("/")),
        "user": unquote(url.username or ""),
        "password": unquote(url.password or ""),
        "sslmode": options.get("sslmode", [""])[0],
        "connect_timeout": options.get("connect_timeout", ["10"])[0],
    }
    if (url.scheme not in ("postgres", "postgresql") or url.fragment
            or set(options) - {"sslmode", "connect_timeout"}
            or any(len(v) != 1 for v in options.values())
            or values["dbname"] != "logs" or values["user"] != "logs"
            or values["sslmode"] != "require"
            or not re.fullmatch(r"[a-zA-Z0-9.-]+", values["host"])
            or not re.fullmatch(r"[a-f0-9]{64}", values["password"])
            or not values["connect_timeout"].isdigit()):
        raise ValueError("unexpected collector connection settings")
    config = Path(template).read_text()
    for name, key in (("HOST", "host"), ("PORT", "port"),
                      ("NAME", "dbname"), ("USER", "user")):
        config = config.replace("__DB_" + name + "__", values[key])
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    # libpq reads the password here; it never appears in Fluent Bit's config,
    # environment, command arguments, or startup database-name log message.
    os.umask(0o077)
    (output / "pg_service.conf").write_text(
        "[roachmotel]\n" + "".join(f"{k}={v}\n" for k, v in values.items()))
    (output / "fluent-bit.conf").write_text(config)


if __name__ == "__main__":
    try:
        prepare(*sys.argv[1:])
    except Exception:
        sys.exit("Cannot prepare collector connection; check the generated logs Secret.")

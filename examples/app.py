"""Read the namespace DATABASE_URL Secret for each fresh connection."""
import os
from pathlib import Path
import time

import psycopg

while True:
    try:
        url = Path(os.environ["ROACHMOTEL_DATABASE_URL_FILE"]).read_text().strip()
        with psycopg.connect(url, connect_timeout=5) as conn:
            conn.execute("CREATE TABLE IF NOT EXISTS visits (pod STRING PRIMARY KEY, count INT NOT NULL)")
            conn.execute("INSERT INTO visits VALUES (%s, 1) ON CONFLICT (pod) DO UPDATE SET count = visits.count + 1",
                         (os.environ["HOSTNAME"],))
            rows = conn.execute("SELECT pod, count FROM visits ORDER BY pod").fetchall()
            print(rows, flush=True)
    except psycopg.Error:
        # Avoid logging driver errors which could contain the connection string.
        print("Database connection/query failed; retrying.", flush=True)
    time.sleep(5)

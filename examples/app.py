"""Fresh connections load renewed CSI certificates without restarting the pod."""
import os
import time

import psycopg

while True:
    try:
        with psycopg.connect(os.environ["ROACHMOTEL_DATABASE_URL"], connect_timeout=5) as conn:
            conn.execute("CREATE TABLE IF NOT EXISTS visits (pod STRING PRIMARY KEY, count INT NOT NULL)")
            conn.execute("INSERT INTO visits VALUES (%s, 1) ON CONFLICT (pod) DO UPDATE SET count = visits.count + 1",
                         (os.environ["HOSTNAME"],))
            rows = conn.execute("SELECT pod, count FROM visits ORDER BY pod").fetchall()
            print(rows, flush=True)
    except psycopg.Error as error:
        # A renewal/file replacement race or transient node failure is retried
        # with a new connection and newly loaded certificate files.
        print(f"Database connection/query failed: {error}", flush=True)
    time.sleep(5)

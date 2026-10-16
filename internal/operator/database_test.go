package operator

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestSQLIntegration(t *testing.T) {
	binary := os.Getenv("COCKROACH")
	if binary == "" {
		t.Skip("set COCKROACH to run real SQL integration")
	}
	dir := t.TempDir()
	certs := filepath.Join(dir, "certs")
	os.Mkdir(certs, 0700)
	for _, args := range [][]string{
		{"cert", "create-ca", "--certs-dir=" + certs, "--ca-key=" + filepath.Join(dir, "ca.key")},
		{"cert", "create-node", "localhost", "127.0.0.1", "--certs-dir=" + certs, "--ca-key=" + filepath.Join(dir, "ca.key")},
		{"cert", "create-client", "root", "--certs-dir=" + certs, "--ca-key=" + filepath.Join(dir, "ca.key")},
	} {
		if out, err := exec.Command(binary, args...).CombinedOutput(); err != nil {
			t.Fatalf("certificate setup: %s %v", out, err)
		}
	}
	port, httpPort := freePort(t), freePort(t)
	logfile, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	server := exec.Command(binary, "start-single-node", "--certs-dir="+certs, "--store="+filepath.Join(dir, "data"), fmt.Sprintf("--listen-addr=127.0.0.1:%d", port), fmt.Sprintf("--http-addr=127.0.0.1:%d", httpPort), "--cache=64MiB", "--max-sql-memory=64MiB")
	server.Stdout = logfile
	server.Stderr = logfile
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { server.Process.Kill(); server.Wait() }()
	q := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {filepath.Join(certs, "ca.crt")}, "sslcert": {filepath.Join(certs, "client.root.crt")}, "sslkey": {filepath.Join(certs, "client.root.key")}}
	admin, err := pgx.ParseConfig(fmt.Sprintf("postgresql://root@localhost:%d/defaultdb?%s", port, q.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		conn, err := pgx.ConnectConfig(ctx, admin.Copy())
		if err == nil {
			conn.Close(ctx)
			break
		}
		if ctx.Err() != nil {
			t.Fatal("local SQL server did not become ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
	db := SQLDatabase{Config: admin}
	t.Run("existing database", func(t *testing.T) {
		id := "existing-tenant"
		setup, err := pgx.ConnectConfig(ctx, admin.Copy())
		if err != nil {
			t.Fatal(err)
		}
		defer setup.Close(ctx)
		for _, statement := range []string{
			"CREATE USER previous_owner",
			"CREATE DATABASE " + quote(id) + " WITH OWNER previous_owner",
			"CREATE TABLE " + quote(id) + ".public.legacy (id INT PRIMARY KEY, value STRING)",
			"INSERT INTO " + quote(id) + ".public.legacy VALUES (1, 'preserved')",
			"CREATE SEQUENCE " + quote(id) + ".public.legacy_seq",
			"GRANT SELECT ON TABLE " + quote(id) + ".public.legacy TO public",
		} {
			if _, err := setup.Exec(ctx, statement); err != nil {
				t.Fatal("setup existing database", err)
			}
		}
		available, exists, err := db.Available(ctx, id)
		if err != nil || !available || !exists {
			t.Fatalf("existing database should allow a new user: %v %v %v", available, exists, err)
		}
		password, _ := Password()
		for _, bootstrap := range []bool{true, false} {
			if err := db.Ensure(ctx, id, password, bootstrap, true); err != nil {
				t.Fatal("reuse existing database", err)
			}
		}
		var owner string
		if err := setup.QueryRow(ctx, "SELECT owner FROM [SHOW DATABASES] WHERE database_name=$1", id).Scan(&owner); err != nil || owner != "previous_owner" {
			t.Fatal("database owner changed", owner, err)
		}
		var grants int
		if err := setup.QueryRow(ctx, "SELECT count(*) FROM [SHOW GRANTS ON TABLE "+quote(id)+".public.legacy] WHERE grantee='public' AND privilege_type='SELECT'").Scan(&grants); err != nil || grants != 1 {
			t.Fatal("existing grant changed", grants, err)
		}
		config := admin.Copy()
		config.Database = id
		inspect, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer inspect.Close(ctx)
		if err := inspect.QueryRow(ctx, "SELECT owner FROM [SHOW SCHEMAS] WHERE schema_name='public'").Scan(&owner); err != nil || owner != "previous_owner" {
			t.Fatal("schema owner changed", owner, err)
		}
		u := url.URL{Scheme: "postgresql", Host: fmt.Sprintf("localhost:%d", port), User: url.UserPassword(id, password), Path: "/" + id, RawQuery: "sslmode=require"}
		tenant, err := pgx.Connect(ctx, u.String())
		if err != nil {
			t.Fatal("new user authentication", err)
		}
		defer tenant.Close(ctx)
		var value string
		if err := tenant.QueryRow(ctx, "SELECT value FROM legacy WHERE id=1").Scan(&value); err != nil || value != "preserved" {
			t.Fatal("existing data access", value, err)
		}
		for _, statement := range []string{"INSERT INTO legacy VALUES (2, 'new')", "SELECT nextval('legacy_seq')", "CREATE TABLE new_items (id INT)"} {
			if _, err := tenant.Exec(ctx, statement); err != nil {
				t.Fatal("new user access", err)
			}
		}
	})
	a, b := "tenant-a", "tenant-b"
	password, _ := Password()
	for _, id := range []string{a, b} {
		available, _, err := db.Available(ctx, id)
		if err != nil || !available {
			t.Fatal("preflight", err)
		}
		if err := db.Ensure(ctx, id, password, true, false); err != nil {
			t.Fatal("provisioning", err)
		}
		if err := db.Ensure(ctx, id, password, false, false); err != nil {
			t.Fatal("idempotency", err)
		}
		available, _, err = db.Available(ctx, id)
		if err != nil || available {
			t.Fatal("collision detection", err)
		}
	}
	connect := func(id, pw string) (*pgx.Conn, error) {
		u := url.URL{Scheme: "postgresql", Host: fmt.Sprintf("localhost:%d", port), User: url.UserPassword(id, pw), Path: "/" + id, RawQuery: url.Values{"sslmode": {"require"}}.Encode()}
		return pgx.Connect(ctx, u.String())
	}
	for _, id := range []string{a, b} {
		conn, err := connect(id, password)
		if err != nil {
			t.Fatal("password authentication failed")
		}
		for _, sql := range []string{"CREATE TABLE items (id INT PRIMARY KEY, value STRING)", "INSERT INTO items VALUES (1, 'original')", "UPDATE items SET value='changed' WHERE id=1", "ALTER TABLE items ADD COLUMN extra INT", "CREATE SCHEMA extra"} {
			if _, err := conn.Exec(ctx, sql); err != nil {
				t.Fatal("tenant DDL/write", err)
			}
		}
		conn.Close(ctx)
	}
	conn, err := connect(a, password)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var value string
	if err := conn.QueryRow(ctx, "SELECT value FROM items WHERE id=1").Scan(&value); err != nil || value != "changed" {
		t.Fatal("read own data", err)
	}
	for _, sql := range []string{"SELECT * FROM " + quote(b) + ".public.items", "INSERT INTO " + quote(b) + ".public.items (id) VALUES (2)", "CREATE DATABASE stolen", "CREATE USER stolen", "DROP DATABASE " + quote(a)} {
		if _, err := conn.Exec(ctx, sql); err == nil {
			t.Fatal("expected tenant permission rejection:", sql)
		}
	}
	if wrong, err := connect(a, "wrong"); err == nil {
		wrong.Close(ctx)
		t.Fatal("wrong password accepted")
	}
	// Opt-out must reject both retained credentials and an already-open session.
	if err := db.Revoke(ctx, a); err != nil {
		t.Fatal("revoke", err)
	}
	if err := db.Revoke(ctx, a); err != nil {
		t.Fatal("idempotent revoke", err)
	}
	if blocked, err := connect(a, password); err == nil {
		blocked.Close(ctx)
		t.Fatal("opted-out user can still log in")
	}
	if _, err := conn.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("opted-out session remained active")
	}
	if err := db.Ensure(ctx, a, password, false, false); err != nil {
		t.Fatal("re-enable", err)
	}
	restored, err := connect(a, password)
	if err != nil {
		t.Fatal("restored login", err)
	}
	defer restored.Close(ctx)
	if err := restored.QueryRow(ctx, "SELECT value FROM items WHERE id=1").Scan(&value); err != nil || value != "changed" {
		t.Fatal("opt-out lost data", err)
	}
	if _, err := restored.Exec(ctx, "CREATE TABLE after_opt_out (id INT PRIMARY KEY)"); err != nil {
		t.Fatal("restored DDL", err)
	}

}

func TestNetworkService(t *testing.T) {
	for _, host := range []string{"db.database", "db.database.svc", "db.database.svc.cluster.local", "db.database.svc.cluster.local."} {
		u, err := url.Parse("postgresql://" + host + ":26257/?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		ns, service, err := NetworkService(u)
		if err != nil || ns != "database" || service != "db" {
			t.Fatalf("%s: %s %s %v", host, ns, service, err)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1", "db.example.com"} {
		u, _ := url.Parse("postgresql://" + host + "/?sslmode=disable")
		if _, _, err := NetworkService(u); err == nil {
			t.Fatalf("accepted non-Service host %s", host)
		}
	}
}

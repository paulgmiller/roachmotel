package operator

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
)

type SQLDatabase struct{ Config *pgx.ConnConfig }

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func (d *SQLDatabase) Available(ctx context.Context, id string) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, d.Config.Copy())
	if err != nil {
		return false, err
	}
	defer conn.Close(ctx)
	var exists bool
	err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM [SHOW DATABASES] WHERE database_name=$1) OR EXISTS(SELECT 1 FROM [SHOW USERS] WHERE username=$1)", id).Scan(&exists)
	return !exists, err
}

func (d *SQLDatabase) Ensure(ctx context.Context, id, password string, bootstrap bool) error {
	conn, err := pgx.ConnectConfig(ctx, d.Config.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	name := quote(id)
	var userExists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM [SHOW USERS] WHERE username=$1)", id).Scan(&userExists); err != nil {
		return err
	}
	// Persisted credentials precede SQL: each step can be retried after a crash.
	for _, statement := range []string{
		"CREATE USER IF NOT EXISTS " + name,
		"CREATE DATABASE IF NOT EXISTS " + name,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return err
		}
	}
	if bootstrap || !userExists {
		if _, err := conn.Exec(ctx, "ALTER USER "+name+" WITH PASSWORD '"+strings.ReplaceAll(password, "'", "''")+"'"); err != nil {
			return err
		}
	}
	config := d.Config.Copy()
	config.Database = id
	db, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	defer db.Close(ctx)
	var owner string
	if err := db.QueryRow(ctx, "SELECT owner FROM [SHOW DATABASES] WHERE database_name=$1", id).Scan(&owner); err != nil {
		return err
	}
	if owner != config.User {
		return errors.New("managed database ownership changed; refusing reconciliation")
	}
	for _, statement := range []string{
		"REVOKE ALL ON DATABASE " + name + " FROM public",
		"REVOKE ALL ON SCHEMA public FROM public",
		"ALTER DEFAULT PRIVILEGES FOR ALL ROLES REVOKE ALL ON TABLES FROM public",
		"GRANT CONNECT, CREATE ON DATABASE " + name + " TO " + name,
		"ALTER SCHEMA public OWNER TO " + name,
		"ALTER USER " + name + " WITH LOGIN",
	} {
		if _, err := db.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// Revoke retains the user, password, object ownership, and database for re-enabling.
func (d *SQLDatabase) Revoke(ctx context.Context, id string) error {
	conn, err := pgx.ConnectConfig(ctx, d.Config.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var userExists, databaseExists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM [SHOW USERS] WHERE username=$1), EXISTS(SELECT 1 FROM [SHOW DATABASES] WHERE database_name=$1)", id).Scan(&userExists, &databaseExists); err != nil {
		return err
	}
	if !userExists {
		return nil
	}
	name := quote(id)
	if _, err := conn.Exec(ctx, "ALTER USER "+name+" WITH NOLOGIN"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "CANCEL SESSIONS IF EXISTS SELECT session_id FROM [SHOW CLUSTER SESSIONS] WHERE user_name=$1", id); err != nil {
		return err
	}
	if databaseExists {
		_, err = conn.Exec(ctx, "REVOKE ALL ON DATABASE "+name+" FROM "+name)
	}
	return err
}

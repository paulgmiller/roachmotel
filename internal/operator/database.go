package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

type SQLDatabase struct{ Config *pgx.ConnConfig }

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Available reports whether the username is available and whether the database exists.
func (d *SQLDatabase) Available(ctx context.Context, id string) (bool, bool, error) {
	conn, err := pgx.ConnectConfig(ctx, d.Config.Copy())
	if err != nil {
		return false, false, fmt.Errorf("connect to admin database: %w", err)
	}
	defer conn.Close(ctx)
	var userExists, databaseExists bool
	err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM [SHOW USERS] WHERE username=$1), EXISTS(SELECT 1 FROM [SHOW DATABASES] WHERE database_name=$1)", id).Scan(&userExists, &databaseExists)
	if err != nil {
		return false, false, fmt.Errorf("query database and user existence: %w", err)
	}
	return !userExists, databaseExists, nil
}

func (d *SQLDatabase) Ensure(ctx context.Context, id, password string, bootstrap, preserveExisting bool) error {
	conn, err := pgx.ConnectConfig(ctx, d.Config.Copy())
	if err != nil {
		return fmt.Errorf("connect to admin database: %w", err)
	}
	defer conn.Close(ctx)
	name := quote(id)
	var userExists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM [SHOW USERS] WHERE username=$1)", id).Scan(&userExists); err != nil {
		return fmt.Errorf("query user existence: %w", err)
	}
	// Persisted credentials precede SQL: each step can be retried after a crash.
	for _, statement := range []string{
		"CREATE USER IF NOT EXISTS " + name,
		"CREATE DATABASE IF NOT EXISTS " + name,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create user and database: %w", err)
		}
	}
	if bootstrap || !userExists {
		if _, err := conn.Exec(ctx, "ALTER USER "+name+" WITH PASSWORD '"+strings.ReplaceAll(password, "'", "''")+"'"); err != nil {
			return fmt.Errorf("set user password: %w", err)
		}
	}
	config := d.Config.Copy()
	config.Database = id
	db, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("connect to tenant database: %w", err)
	}
	defer db.Close(ctx)
	if preserveExisting {
		// Add access without changing ownership, PUBLIC grants, or default privileges.
		for _, statement := range []string{
			"GRANT CONNECT, CREATE ON DATABASE " + name + " TO " + name,
			"GRANT USAGE, CREATE ON SCHEMA public TO " + name,
			"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + name,
			"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO " + name,
			"ALTER USER " + name + " WITH LOGIN",
		} {
			if _, err := db.Exec(ctx, statement); err != nil {
				return fmt.Errorf("grant database access: %w", err)
			}
		}
		return nil
	}
	var owner string
	if err := db.QueryRow(ctx, "SELECT owner FROM [SHOW DATABASES] WHERE database_name=$1", id).Scan(&owner); err != nil {
		return fmt.Errorf("check database ownership: %w", err)
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
			return fmt.Errorf("reconcile database privileges: %w", err)
		}
	}
	return nil
}

// Revoke retains the user, password, object ownership, and database for re-enabling.
func (d *SQLDatabase) Revoke(ctx context.Context, id string) error {
	conn, err := pgx.ConnectConfig(ctx, d.Config.Copy())
	if err != nil {
		return fmt.Errorf("connect to admin database: %w", err)
	}
	defer conn.Close(ctx)
	var userExists, databaseExists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM [SHOW USERS] WHERE username=$1), EXISTS(SELECT 1 FROM [SHOW DATABASES] WHERE database_name=$1)", id).Scan(&userExists, &databaseExists); err != nil {
		return fmt.Errorf("query user existence: %w", err)
	}
	if !userExists {
		return nil
	}
	name := quote(id)
	if _, err := conn.Exec(ctx, "ALTER USER "+name+" WITH NOLOGIN"); err != nil {
		return fmt.Errorf("disable user login: %w", err)
	}
	if _, err := conn.Exec(ctx, "CANCEL SESSIONS IF EXISTS SELECT session_id FROM [SHOW CLUSTER SESSIONS] WHERE user_name=$1", id); err != nil {
		return fmt.Errorf("cancel user sessions: %w", err)
	}
	if databaseExists {
		if _, err := conn.Exec(ctx, "REVOKE ALL ON DATABASE "+name+" FROM "+name); err != nil {
			return fmt.Errorf("revoke database privileges: %w", err)
		}
	}
	return nil
}

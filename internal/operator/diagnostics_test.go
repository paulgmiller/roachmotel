package operator

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestErrorSummary(t *testing.T) {
	const secret = "sensitive-password"
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"timeout", atStage("ensure database access", fmt.Errorf("%s: %w", secret, context.DeadlineExceeded)), "ensure database access: context deadline exceeded"},
		{"canceled", context.Canceled, "context canceled"},
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connection refused"},
		{"network timeout", &net.DNSError{Err: secret, Name: secret, IsTimeout: true}, "DNS lookup failed"},
		{"certificate", x509.HostnameError{Host: secret}, "TLS certificate verification failed"},
		{"SQL password", &pgconn.PgError{Code: "42601", Message: "syntax error in ALTER USER WITH PASSWORD '" + secret + "'", Detail: secret, Hint: secret, InternalQuery: secret}, "SQL error (SQLSTATE 42601)"},
		{"SQL authentication", fmt.Errorf("%s: %w", secret, &pgconn.PgError{Code: "28P01", Message: secret}), "SQL error (SQLSTATE 28P01)"},
		{"invalid SQLSTATE", &pgconn.PgError{Code: secret, Message: secret}, "SQL error"},
		{"API forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, secret, errors.New(secret)), "Kubernetes API Forbidden (HTTP 403)"},
		{"API secret validation", apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, secret, field.ErrorList{field.Invalid(field.NewPath("data"), secret, secret)}), "Kubernetes API Invalid (HTTP 422)"},
		{"API unknown reason", &apierrors.StatusError{ErrStatus: meta.Status{Reason: meta.StatusReason(secret), Message: secret, Code: 500}}, "Kubernetes API unknown reason (HTTP 500)"},
		{"malformed connection", pgconn.NewParseConfigError("postgres://root:"+secret+"@db", secret, nil), "unclassified error (*pgconn.ParseConfigError)"},
		{"URL", &url.Error{Op: secret, URL: "postgres://root:" + secret + "@db", Err: errors.New(secret)}, "unclassified error (*errors.errorString)"},
		{"operator refusal", safeFailure("invalid retained credential record"), "invalid retained credential record"},
		{"joined cleanup", errors.Join(atStage("revoke SQL access", context.DeadlineExceeded), atStage("remove application credential", apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, secret, errors.New(secret)))), "revoke SQL access: context deadline exceeded; remove application credential: Kubernetes API Forbidden (HTTP 403)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ErrorSummary(tt.err)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if strings.Contains(got, secret) {
				t.Fatal("summary leaked credentials")
			}
		})
	}
}

func TestReconciliationErrorStages(t *testing.T) {
	t.Run("network API", func(t *testing.T) {
		c, _, ns := fixture(t)
		c.Client.(*fake.Clientset).PrependReactor("get", "services", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "db", errors.New("sensitive-password"))
		})
		err := c.reconcileNamespace(context.Background(), ns)
		want := "ensure network access: Kubernetes API Forbidden (HTTP 403)"
		if got := ErrorSummary(err); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("database timeout", func(t *testing.T) {
		c, _, ns := fixture(t)
		c.DB = timeoutDB{}
		err := c.reconcileNamespace(context.Background(), ns)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost original timeout: %v", err)
		}
		want := "check database availability: context deadline exceeded"
		if got := ErrorSummary(err); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

type timeoutDB struct{}

func (timeoutDB) Available(context.Context, string) (bool, bool, error) {
	return false, false, context.DeadlineExceeded
}
func (timeoutDB) Ensure(context.Context, string, string, bool, bool) error {
	return context.DeadlineExceeded
}
func (timeoutDB) Revoke(context.Context, string) error { return context.DeadlineExceeded }

func TestSQLConnectionErrorStage(t *testing.T) {
	config, err := pgx.ParseConfig("postgresql://root:sensitive-password@localhost:1/system?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db := &SQLDatabase{Config: config}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func() error{
		"available": func() error { _, _, err := db.Available(ctx, "tenant"); return err },
		"ensure":    func() error { return db.Ensure(ctx, "tenant", "sensitive-password", true, false) },
		"revoke":    func() error { return db.Revoke(ctx, "tenant") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost original cancellation: %v", err)
			}
			if got, want := ErrorSummary(err), "connect to admin database: context canceled"; got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestReconciliationErrorStages(t *testing.T) {
	t.Run("network API", func(t *testing.T) {
		c, _, ns := fixture(t)
		c.Client.(*fake.Clientset).PrependReactor("get", "services", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "db", errors.New("blocked by policy"))
		})
		err := c.reconcileNamespace(context.Background(), ns)
		want := `ensure network access: services "db" is forbidden: blocked by policy`
		if got := err.Error(); got != want {
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
		if got := err.Error(); got != want {
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
			if !strings.HasPrefix(err.Error(), "connect to admin database:") {
				t.Fatalf("missing connection context: %v", err)
			}
		})
	}
}

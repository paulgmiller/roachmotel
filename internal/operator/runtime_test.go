package operator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	c, db, ns := fixture(t)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ns.Name}}
	check := func() {
		t.Helper()
		result, err := c.Reconcile(ctx, req)
		if err != nil || !result.IsZero() {
			t.Fatalf("expected no error or timed requeue, got %+v, %v", result, err)
		}
	}
	check() // A deleted namespace must not be retried or recreated.
	if len(db.checked) != 0 {
		t.Fatal("provisioned a missing namespace")
	}
	if _, err := c.Client.CoreV1().Namespaces().Create(ctx, ns, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{}); err != nil {
		t.Fatal("did not publish credentials", err)
	}
	check() // Existing namespaces remain idempotent.
	if len(db.ensured) != 2 {
		t.Fatal("did not reconcile the namespace")
	}
	ns.Annotations = map[string]string{EnabledAnnotation: "false"}
	if _, err := c.Client.CoreV1().Namespaces().Update(ctx, ns, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	check()
	if len(db.revoked) != 1 {
		t.Fatal("namespace opt-out did not revoke SQL access")
	}
	if _, err := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("namespace opt-out retained application credentials", err)
	}
}

func TestReconcileSafeErrors(t *testing.T) {
	ctx := context.Background()
	c, _, ns := fixture(t)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ns.Name}}
	c.Client.(*fake.Clientset).PrependReactor("get", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "sensitive-password", errors.New("sensitive-password"))
	})
	result, err := c.Reconcile(ctx, req)
	if err == nil || err.Error() != "read namespace: Kubernetes API Forbidden (HTTP 403)" || !result.IsZero() {
		t.Fatalf("unexpected error or requeue: %+v, %v", result, err)
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("manager can unwrap an unsafe error")
	}

	c, _, ns = fixture(t)
	if _, err := c.Client.CoreV1().Namespaces().Create(ctx, ns, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Secret API responses can contain credential values. The returned error
	// itself must be safe because controller-runtime logs it automatically.
	c.Client.(*fake.Clientset).PrependReactor("create", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewBadRequest("sensitive-password")
	})
	result, err = c.Reconcile(ctx, req)
	if err == nil || err.Error() != "persist retained credential: Kubernetes API BadRequest (HTTP 400)" || !result.IsZero() {
		t.Fatalf("unexpected error or requeue: %+v, %v", result, err)
	}
}

func TestRequestsForChange(t *testing.T) {
	ctx := context.Background()
	c, _, ns := fixture(t)
	other := &core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "other"}}
	for _, n := range []*core.Namespace{ns, other} {
		if _, err := c.Client.CoreV1().Namespaces().Create(ctx, n, meta.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	all := []string{other.Name, ns.Name} // Fake client lists names alphabetically.
	for _, tt := range []struct {
		name, kind, namespace, resource, tenant string
		managed                                 bool
		want                                    []string
	}{
		{"application secret", "Secret", ns.Name, SecretName, ns.Name, true, []string{ns.Name}},
		{"retained credential", "Secret", c.Namespace, "rm-tenant", ns.Name, true, []string{ns.Name}},
		{"tenant policy", "NetworkPolicy", "database", "rm-tenant", ns.Name, true, []string{ns.Name}},
		{"infrastructure policy", "NetworkPolicy", "database", "roachmotel-sql-infrastructure", c.Namespace, true, all},
		{"admin secret", "Secret", c.Namespace, "roachmotel-admin", "", false, all},
		{"database service", "Service", "database", "cockroachdb", "", false, all},
		{"unrelated secret", "Secret", ns.Name, "unrelated", ns.Name, false, nil},
		{"unrelated service", "Service", "other", "cockroachdb", "", false, nil},
		{"same name different kind", "Secret", "database", "cockroachdb", "", false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := &meta.PartialObjectMetadata{
				TypeMeta:   meta.TypeMeta{Kind: tt.kind, APIVersion: "v1"},
				ObjectMeta: meta.ObjectMeta{Name: tt.resource, Namespace: tt.namespace, Annotations: map[string]string{namespaceKey: tt.tenant}},
			}
			if tt.managed {
				obj.Labels = map[string]string{"app.kubernetes.io/managed-by": "roachmotel"}
			}
			var got []string
			for _, req := range c.requestsForChange(ctx, obj) {
				if req.Namespace != "" {
					t.Fatal("namespace reconciliation request must be cluster-scoped")
				}
				got = append(got, req.Name)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdminConfigurationReload(t *testing.T) {
	ctx := context.Background()
	c, _, ns := fixture(t)
	if _, err := c.Client.CoreV1().Namespaces().Create(ctx, ns, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.DB = nil // Production loads the current admin URL for each reconciliation.
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ns.Name}}
	if _, err := c.Reconcile(ctx, req); err == nil || err.Error() != "read admin credential: Kubernetes API NotFound (HTTP 404)" {
		t.Fatalf("expected missing admin Secret, got %v", err)
	}
	secret := &core.Secret{ObjectMeta: meta.ObjectMeta{Name: "roachmotel-admin", Namespace: c.Namespace}, Data: map[string][]byte{"connection-string": []byte("postgresql://root:sensitive-password@db:invalid/")}}
	secret, err := c.Client.CoreV1().Secrets(c.Namespace).Create(ctx, secret, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Reconcile(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "parse admin connection configuration") || strings.Contains(err.Error(), "sensitive-password") {
		t.Fatalf("unsafe or missing configuration diagnostic: %v", err)
	}
	secret.Data["connection-string"] = []byte("postgresql://root:sensitive-password@localhost:1/system?sslmode=disable")
	if _, err := c.Client.CoreV1().Secrets(c.Namespace).Update(ctx, secret, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel() // Verify the new URL parsed without making a network connection.
	_, err = c.Reconcile(ctx, req)
	if err == nil || err.Error() != "check database availability: connect to admin database: context canceled" {
		t.Fatalf("did not reload corrected admin configuration: %v", err)
	}
	if c.DB != nil {
		t.Fatal("retained a stale admin configuration between reconciliations")
	}
}

func TestReconcileSkipsInfrastructureWithoutAdmin(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"kube-system", "roachmotel-system", "database", "excluded"} {
		t.Run(name, func(t *testing.T) {
			c, _, ns := fixture(t)
			c.DB = nil
			c.Excluded = map[string]bool{"excluded": true}
			ns.Name = name
			if _, err := c.Client.CoreV1().Namespaces().Create(ctx, ns, meta.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			result, err := c.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
			if err != nil || !result.IsZero() {
				t.Fatalf("excluded namespace requires admin credentials: %+v, %v", result, err)
			}
		})
	}
}

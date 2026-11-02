package operator

import (
	"context"
	"errors"
	"testing"

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

func TestReconcileErrorDetails(t *testing.T) {
	ctx := context.Background()
	c, _, ns := fixture(t)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: ns.Name}}
	cause := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, ns.Name, errors.New("blocked by policy"))
	c.Client.(*fake.Clientset).PrependReactor("get", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, cause
	})
	result, err := c.Reconcile(ctx, req)
	if err == nil || err.Error() != "read namespace: "+cause.Error() || !errors.Is(err, cause) || !result.IsZero() {
		t.Fatalf("unexpected error or requeue: %+v, %v", result, err)
	}

	c, _, ns = fixture(t)
	if _, err := c.Client.CoreV1().Namespaces().Create(ctx, ns, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cause = apierrors.NewBadRequest("invalid Secret")
	c.Client.(*fake.Clientset).PrependReactor("create", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, cause
	})
	result, err = c.Reconcile(ctx, req)
	if err == nil || err.Error() != "persist retained credential: "+cause.Error() || !errors.Is(err, cause) || !result.IsZero() {
		t.Fatalf("unexpected error or requeue: %+v, %v", result, err)
	}
}

func TestReconcileSkipsInfrastructure(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"kube-system", "roachmotel-system", "database", "excluded"} {
		t.Run(name, func(t *testing.T) {
			c, _, ns := fixture(t)
			c.Excluded = map[string]bool{"excluded": true}
			ns.Name = name
			if _, err := c.Client.CoreV1().Namespaces().Create(ctx, ns, meta.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			result, err := c.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
			if err != nil || !result.IsZero() {
				t.Fatalf("excluded namespace returned an error or requeue: %+v, %v", result, err)
			}
		})
	}
}

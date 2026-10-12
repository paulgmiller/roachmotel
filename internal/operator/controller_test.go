package operator

import (
	"context"
	"errors"
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeDB struct {
	fail      bool
	available bool
	passwords []string
	bootstrap []bool
}

func (d *fakeDB) Available(context.Context, string) (bool, error) { return d.available, nil }
func (d *fakeDB) Ensure(_ context.Context, _ string, password string, bootstrap bool) error {
	d.passwords = append(d.passwords, password)
	d.bootstrap = append(d.bootstrap, bootstrap)
	if d.fail {
		return errors.New("transient SQL failure")
	}
	return nil
}

func fixture(t *testing.T) (*Controller, *fakeDB, *core.Namespace) {
	t.Helper()
	template, err := ValidateTemplate("postgresql://localhost:26257/?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db := &fakeDB{available: true}
	return &Controller{Client: fake.NewClientset(), DB: db, Namespace: "roachmotel-system", Template: template}, db, &core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "tenant-a"}}
}

func TestRetryAndNamespaceRecreation(t *testing.T) {
	c, db, ns := fixture(t)
	ctx := context.Background()
	db.fail = true
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("expected SQL failure")
	}
	master, err := c.Client.CoreV1().Secrets(c.Namespace).Get(ctx, Identity(ns.Name), meta.GetOptions{})
	if err != nil {
		t.Fatal("password must be persisted before SQL", err)
	}
	if _, err := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{}); err == nil {
		t.Fatal("published credentials before SQL success")
	}
	db.fail = false
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	first, _ := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{})
	if len(first.OwnerReferences) != 0 || len(master.OwnerReferences) != 0 {
		t.Fatal("retained credentials must not have owner references")
	}
	if err := c.Client.CoreV1().Secrets(ns.Name).Delete(ctx, SecretName, meta.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	recreated, _ := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{})
	if string(first.Data["connection-string"]) != string(recreated.Data["connection-string"]) {
		t.Fatal("credentials changed after namespace recreation")
	}
	for _, p := range db.passwords {
		if p != string(master.Data["password"]) {
			t.Fatal("retry changed password")
		}
	}
	if !db.bootstrap[0] || !db.bootstrap[1] || db.bootstrap[2] {
		t.Fatal("password bootstrap state was not retained")
	}
}

func TestConflictsAndExclusions(t *testing.T) {
	c, db, ns := fixture(t)
	ctx := context.Background()
	db.available = false
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("adopted an existing database")
	}
	db.available = true
	_, err := c.Client.CoreV1().Secrets(ns.Name).Create(ctx, &core.Secret{ObjectMeta: meta.ObjectMeta{Name: SecretName}}, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("overwrote an unrelated Secret")
	}
	c, db, ns = fixture(t)
	for _, name := range []string{"kube-system", c.Namespace} {
		ns.Name = name
		if err := c.Reconcile(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	ns.Name = "opt-out"
	ns.Labels = map[string]string{"roachmotel.dev/enabled": "false"}
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if len(db.passwords) != 0 {
		t.Fatal("provisioned excluded namespace")
	}
}

func TestTemplateCannotLeakAdminCredentials(t *testing.T) {
	for _, raw := range []string{"postgresql://root:secret@db/?sslmode=disable", "postgresql://db/?sslmode=verify-full&sslkey=/admin/key", "postgresql://db/?sslmode=require", "postgresql://db/?sslmode=disable&user=root"} {
		if _, err := ValidateTemplate(raw); err == nil {
			t.Fatal("accepted unsafe template")
		}
	}
}

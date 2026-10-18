package operator

import (
	"context"
	"errors"
	"net/url"
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeDB struct {
	revoked          []string
	fail             bool
	available        bool
	passwords        []string
	bootstrap        []bool
	checked          []string
	ensured          []string
	existingDatabase bool
	preserved        []bool
}

func (d *fakeDB) Available(_ context.Context, id string) (bool, bool, error) {
	d.checked = append(d.checked, id)
	return d.available, d.existingDatabase, nil
}
func (d *fakeDB) Ensure(_ context.Context, id string, password string, bootstrap, preserveExisting bool) error {
	d.preserved = append(d.preserved, preserveExisting)
	d.ensured = append(d.ensured, id)
	d.passwords = append(d.passwords, password)
	d.bootstrap = append(d.bootstrap, bootstrap)
	if d.fail {
		return errors.New("transient SQL failure")
	}
	return nil
}

func (d *fakeDB) Revoke(_ context.Context, id string) error {
	d.revoked = append(d.revoked, id)
	if d.fail {
		return errors.New("transient SQL failure")
	}
	return nil
}

func fixture(t *testing.T) (*Controller, *fakeDB, *core.Namespace) {
	t.Helper()
	template, err := ValidateTemplate("postgresql://cockroachdb.database.svc.cluster.local:26257/?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	db := &fakeDB{available: true}
	return &Controller{Client: fake.NewClientset(&core.Service{ObjectMeta: meta.ObjectMeta{Name: "cockroachdb", Namespace: "database"}, Spec: core.ServiceSpec{Selector: map[string]string{"app": "cockroachdb"}, Ports: []core.ServicePort{{Port: 26257, TargetPort: intstr.FromInt32(26257)}}}}), DB: db, Namespace: "roachmotel-system", Template: template}, db, &core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "tenant-a"}}
}

func TestRetryAndNamespaceRecreation(t *testing.T) {
	c, db, ns := fixture(t)
	ctx := context.Background()
	db.fail = true
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("expected SQL failure")
	}
	masters, err := c.Client.CoreV1().Secrets(c.Namespace).List(ctx, meta.ListOptions{})
	if err != nil {
		t.Fatal("password must be persisted before SQL", err)
	}
	if len(masters.Items) != 1 {
		t.Fatal("expected one retained credential before SQL")
	}
	master := &masters.Items[0]
	if errs := validation.IsDNS1123Subdomain(master.Name); len(errs) != 0 {
		t.Fatalf("Kubernetes would reject retained Secret name %q: %v", master.Name, errs)
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
	if string(first.Data["DATABASE_URL"]) != string(recreated.Data["DATABASE_URL"]) {
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
		t.Fatal("adopted an existing user")
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

func TestExistingDatabasePreservedAcrossRetries(t *testing.T) {
	c, db, ns := fixture(t)
	db.existingDatabase = true
	db.fail = true
	ctx := context.Background()
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("expected SQL failure")
	}
	db.existingDatabase = false
	db.fail = false
	for i := 0; i < 2; i++ {
		if err := c.Reconcile(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	if len(db.preserved) != 3 {
		t.Fatalf("expected three SQL attempts: %v", db.preserved)
	}
	for _, preserve := range db.preserved {
		if !preserve {
			t.Fatal("retry would change existing database ownership or grants")
		}
	}
}

func TestLegacyCredentialPreservesExistingDatabase(t *testing.T) {
	c, db, ns := fixture(t)
	ctx := context.Background()
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	secrets := c.Client.CoreV1().Secrets(c.Namespace)
	list, err := secrets.List(ctx, meta.ListOptions{})
	if err != nil || len(list.Items) != 1 {
		t.Fatal("expected retained credential", err)
	}
	master := &list.Items[0]
	delete(master.Annotations, existingDatabaseKey)
	if _, err := secrets.Update(ctx, master, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	db.existingDatabase = true
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if !db.preserved[len(db.preserved)-1] {
		t.Fatal("upgrade would change existing database ownership or grants")
	}
	master, err = secrets.Get(ctx, master.Name, meta.GetOptions{})
	if err != nil || master.Annotations[existingDatabaseKey] != "true" {
		t.Fatal("upgrade did not retain existing database state", err)
	}
}

func TestControllerIgnoresOtherCredentialSecret(t *testing.T) {
	c, _, ns := fixture(t)
	ctx := context.Background()
	secrets := c.Client.CoreV1().Secrets(ns.Name)
	ignored := &core.Secret{ObjectMeta: meta.ObjectMeta{Name: "roachmotel-credentials", Namespace: ns.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "roachmotel"}, Annotations: map[string]string{namespaceKey: ns.Name}}, Data: map[string][]byte{"connection-string": []byte("leave alone")}}
	if _, err := secrets.Create(ctx, ignored, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.Client.(*fake.Clientset).ClearActions()
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	ns.Annotations = map[string]string{EnabledAnnotation: "false"}
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	for _, action := range c.Client.(*fake.Clientset).Actions() {
		if get, ok := action.(interface{ GetName() string }); ok && get.GetName() == ignored.Name {
			t.Fatalf("controller accessed ignored Secret: %s", action.GetVerb())
		}
	}
	retained, err := secrets.Get(ctx, ignored.Name, meta.GetOptions{})
	if err != nil || string(retained.Data["connection-string"]) != "leave alone" {
		t.Fatal("controller changed ignored Secret", err)
	}
}

func TestTemplateCannotLeakAdminCredentials(t *testing.T) {
	for _, raw := range []string{"postgresql://root:secret@db/?sslmode=require", "postgresql://db/?sslmode=verify-full&sslkey=/admin/key", "postgresql://db/?sslmode=disable", "postgresql://db/?sslmode=verify-full", "postgresql://db/?sslmode=require&sslrootcert=/ca.crt", "postgresql://db/?sslmode=require&user=root"} {
		if _, err := ValidateTemplate(raw); err == nil {
			t.Fatal("accepted unsafe template")
		}
	}
}

func TestNetworkPolicyAndAnnotationOptOut(t *testing.T) {
	c, db, ns := fixture(t)
	ctx := context.Background()
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	policies := c.Client.NetworkingV1().NetworkPolicies("database")
	list, err := policies.List(ctx, meta.ListOptions{})
	if err != nil || len(list.Items) != 2 {
		t.Fatalf("expected tenant and infrastructure policies: %v %v", list, err)
	}
	name := "rm-" + Identity(ns.Name)[3:]
	policy, err := policies.Get(ctx, name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rule := policy.Spec.Ingress[0]
	if policy.Spec.PodSelector.MatchLabels["app"] != "cockroachdb" || len(rule.From) != 1 || rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != ns.Name || rule.From[0].PodSelector != nil || rule.Ports[0].Port.IntVal != 26257 {
		t.Fatalf("wrong SQL access scope: %+v", policy.Spec)
	}
	first, _ := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{})
	ns.Annotations = map[string]string{EnabledAnnotation: "false"}
	for i := 0; i < 2; i++ {
		if err := c.Reconcile(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	if len(db.revoked) != 2 || db.revoked[0] != ns.Name || db.revoked[1] != ns.Name {
		t.Fatal("did not revoke SQL access")
	}
	if _, err := policies.Get(ctx, name, meta.GetOptions{}); err == nil {
		t.Fatal("tenant policy retained")
	}
	if _, err := policies.Get(ctx, "roachmotel-sql-infrastructure", meta.GetOptions{}); err != nil {
		t.Fatal("infrastructure policy removed", err)
	}
	if _, err := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{}); err == nil {
		t.Fatal("tenant credentials retained")
	}
	if _, err := c.Client.CoreV1().Secrets(c.Namespace).Get(ctx, name, meta.GetOptions{}); err != nil {
		t.Fatal("master credentials removed", err)
	}
	delete(ns.Annotations, EnabledAnnotation)
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	restored, _ := c.Client.CoreV1().Secrets(ns.Name).Get(ctx, SecretName, meta.GetOptions{})
	if string(first.Data["DATABASE_URL"]) != string(restored.Data["DATABASE_URL"]) {
		t.Fatal("re-enable changed credentials")
	}
	if _, err := policies.Get(ctx, name, meta.GetOptions{}); err != nil {
		t.Fatal("policy not restored", err)
	}
}

func TestOptOutRetriesAndDoesNotProvision(t *testing.T) {
	c, db, ns := fixture(t)
	ctx := context.Background()
	ns.Annotations = map[string]string{EnabledAnnotation: "false"}
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if len(db.passwords) != 0 || len(db.revoked) != 0 {
		t.Fatal("touched unprovisioned SQL identity")
	}
	masters, _ := c.Client.CoreV1().Secrets(c.Namespace).List(ctx, meta.ListOptions{})
	if len(masters.Items) != 0 {
		t.Fatal("provisioned opted-out namespace")
	}
	delete(ns.Annotations, EnabledAnnotation)
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	ns.Annotations[EnabledAnnotation] = "false"
	db.fail = true
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("SQL failure not retried")
	}
	policies, _ := c.Client.NetworkingV1().NetworkPolicies("database").List(ctx, meta.ListOptions{})
	if len(policies.Items) != 1 {
		t.Fatal("SQL failure prevented network cleanup")
	}
	db.fail = false
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkPolicyRepairAndConflicts(t *testing.T) {
	c, _, ns := fixture(t)
	ctx := context.Background()
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	policies := c.Client.NetworkingV1().NetworkPolicies("database")
	name := "rm-" + Identity(ns.Name)[3:]
	policy, _ := policies.Get(ctx, name, meta.GetOptions{})
	policy.Spec.Ingress = nil
	if _, err := policies.Update(ctx, policy, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	policy, _ = policies.Get(ctx, name, meta.GetOptions{})
	if len(policy.Spec.Ingress) != 1 {
		t.Fatal("policy drift not repaired")
	}
	policy.Labels = nil
	if _, err := policies.Update(ctx, policy, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("adopted unrelated policy")
	}
	ns.Annotations = map[string]string{EnabledAnnotation: "false"}
	if c.Reconcile(ctx, ns) == nil {
		t.Fatal("deleted unrelated policy")
	}
	if _, err := policies.Get(ctx, name, meta.GetOptions{}); err != nil {
		t.Fatal("unrelated policy removed")
	}
}

func TestNetworkUsesServiceSQLTargetPort(t *testing.T) {
	for _, target := range []intstr.IntOrString{intstr.FromInt32(26258), intstr.FromString("sql")} {
		c, _, ns := fixture(t)
		ctx := context.Background()
		services := c.Client.CoreV1().Services("database")
		svc, _ := services.Get(ctx, "cockroachdb", meta.GetOptions{})
		svc.Spec.Ports[0].TargetPort = target
		svc.Spec.Ports = append(svc.Spec.Ports, core.ServicePort{Port: 8080, TargetPort: intstr.FromInt32(8080)})
		if _, err := services.Update(ctx, svc, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := c.Reconcile(ctx, ns); err != nil {
			t.Fatal(err)
		}
		policy, err := c.Client.NetworkingV1().NetworkPolicies("database").Get(ctx, "rm-"+Identity(ns.Name)[3:], meta.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ports := policy.Spec.Ingress[0].Ports
		if len(ports) != 1 || *ports[0].Port != target {
			t.Fatalf("wrong SQL target port: %+v", ports)
		}
		svc.Spec.Selector = nil
		if _, err := services.Update(ctx, svc, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if c.Reconcile(ctx, ns) == nil {
			t.Fatal("accepted selectorless Service")
		}
	}
}

func TestCredentialsContainOnlyTLSRequiredConnectionString(t *testing.T) {
	c, db, ns := fixture(t)
	ctx := context.Background()
	_, err := c.Client.CoreV1().Secrets(ns.Name).Create(ctx, &core.Secret{
		ObjectMeta: meta.ObjectMeta{Name: "roachkey", Namespace: ns.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "roachmotel"}, Annotations: map[string]string{namespaceKey: ns.Name}},
		Data:       map[string][]byte{"DATABASE_URL": []byte("postgresql://rm_old:old@cockroachdb.database:26257/rm_old?sslmode=require")},
	}, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	secrets := c.Client.CoreV1().Secrets(ns.Name)
	secret, err := secrets.Get(ctx, SecretName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := ValidateTemplate("postgresql://cockroachdb.database.svc.cluster.local:26257/?sslmode=require&connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	c.Template = u
	// Reconcile an existing Secret from a deployment that published certificates.
	secret.Data["ca.crt"] = []byte("old certificate")
	secret.Data["client.crt"] = []byte("old client certificate")
	if _, err := secrets.Update(ctx, secret, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	secret, err = secrets.Get(ctx, SecretName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 1 {
		t.Fatalf("unexpected credential keys: %v", secret.Data)
	}
	connection, err := url.Parse(string(secret.Data["DATABASE_URL"]))
	if err != nil {
		t.Fatal(err)
	}
	if connection.Query().Get("sslmode") != "require" || connection.Query().Get("connect_timeout") != "5" || connection.Query().Has("sslrootcert") {
		t.Fatalf("unexpected application transport: %v", connection.Query())
	}
	if connection.User.Username() != ns.Name || connection.Path != "/"+ns.Name {
		t.Fatalf("username and database must match namespace %q: %s", ns.Name, connection.Redacted())
	}
	if len(db.checked) != 1 || db.checked[0] != ns.Name {
		t.Fatalf("checked wrong SQL identity: %v", db.checked)
	}
	for _, id := range db.ensured {
		if id != ns.Name {
			t.Fatalf("provisioned SQL identity %q instead of namespace %q", id, ns.Name)
		}
	}
}

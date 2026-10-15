package operator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const SecretName = "roachmotel-credentials"
const EnabledAnnotation = "roachmotel.dev/enabled"
const namespaceKey = "roachmotel.dev/namespace"

type Database interface {
	Available(context.Context, string) (bool, error)
	Ensure(context.Context, string, string, bool) error
	Revoke(context.Context, string) error
}

type Controller struct {
	Client    kubernetes.Interface
	DB        Database
	Namespace string
	Template  *url.URL
	Excluded  map[string]bool
}

func Identity(namespace string) string {
	hash := sha256.Sum256([]byte(namespace))
	return "rm_" + hex.EncodeToString(hash[:16])
}

func Password() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Only connection transport settings belong in the tenant template, never admin credentials.
func ValidateTemplate(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgresql" && u.Scheme != "postgres") || u.Host == "" || u.User != nil {
		return nil, errors.New("application URL template must be a PostgreSQL URL without user credentials")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid application URL options")
	}
	if q.Get("sslmode") != "require" {
		return nil, errors.New("application template requires explicit sslmode=require")
	}
	for k, values := range q {
		if len(values) != 1 {
			return nil, errors.New("duplicate application URL option")
		}
		switch k {
		case "sslmode", "connect_timeout":
		default:
			return nil, fmt.Errorf("unsupported application URL option %s", k)
		}
	}
	if u.Fragment != "" {
		return nil, errors.New("application URL cannot contain a fragment")
	}
	return u, nil
}

func (c *Controller) Reconcile(ctx context.Context, ns *core.Namespace) error {
	if ns.DeletionTimestamp != nil || ns.Name == c.Namespace || c.Excluded[ns.Name] || strings.HasPrefix(ns.Name, "kube-") {
		return nil
	}
	dbNamespace, _, err := NetworkService(c.Template)
	if err != nil {
		return err
	}
	if ns.Name == dbNamespace {
		return nil
	}
	if ns.Annotations[EnabledAnnotation] == "false" || ns.Labels[EnabledAnnotation] == "false" {
		return c.revoke(ctx, ns)
	}
	if err := c.ensureNetwork(ctx, ns.Name, true); err != nil {
		return err
	}
	id := Identity(ns.Name)
	// SQL identifiers use underscores; Kubernetes Secret names require DNS names.
	masterName := strings.ReplaceAll(id, "_", "-")
	store := c.Client.CoreV1().Secrets(c.Namespace)
	master, err := store.Get(ctx, masterName, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		available, checkErr := c.DB.Available(ctx, id)
		if checkErr != nil {
			return checkErr
		}
		if !available {
			return errors.New("database or username already exists without a retained credential; refusing adoption")
		}
		password, passwordErr := Password()
		if passwordErr != nil {
			return passwordErr
		}
		master, err = store.Create(ctx, &core.Secret{ObjectMeta: meta.ObjectMeta{Name: masterName, Namespace: c.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "roachmotel"}, Annotations: map[string]string{namespaceKey: ns.Name}}, Type: core.SecretTypeOpaque, Data: map[string][]byte{"password": []byte(password)}}, meta.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			master, err = store.Get(ctx, masterName, meta.GetOptions{})
		}
	}
	if err != nil {
		return err
	}
	_, passwordErr := hex.DecodeString(string(master.Data["password"]))
	if master.Labels["app.kubernetes.io/managed-by"] != "roachmotel" || master.Annotations[namespaceKey] != ns.Name || len(master.Data["password"]) != 64 || passwordErr != nil {
		return errors.New("invalid retained credential record")
	}
	password := string(master.Data["password"])
	if err := c.DB.Ensure(ctx, id, password, master.Annotations["roachmotel.dev/provisioned"] != "true"); err != nil {
		return err
	}
	if master.Annotations["roachmotel.dev/provisioned"] != "true" {
		master.Annotations["roachmotel.dev/provisioned"] = "true"
		if _, err := store.Update(ctx, master, meta.UpdateOptions{}); err != nil {
			return err
		}
	}
	u := *c.Template
	u.User = url.UserPassword(id, password)
	u.Path = "/" + id
	u.RawPath = ""
	data := map[string][]byte{"connection-string": []byte(u.String())}
	target := c.Client.CoreV1().Secrets(ns.Name)
	secret, err := target.Get(ctx, SecretName, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = target.Create(ctx, &core.Secret{ObjectMeta: meta.ObjectMeta{Name: SecretName, Namespace: ns.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "roachmotel"}, Annotations: map[string]string{namespaceKey: ns.Name}}, Type: core.SecretTypeOpaque, Data: data}, meta.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "roachmotel" || secret.Annotations[namespaceKey] != ns.Name {
		return errors.New("namespace Secret already exists and is not managed by roachmotel")
	}
	if secret.Immutable != nil && *secret.Immutable {
		return errors.New("namespace credential Secret cannot be immutable")
	}
	if reflect.DeepEqual(secret.Data, data) {
		return nil
	}
	secret.Data = data
	_, err = target.Update(ctx, secret, meta.UpdateOptions{})
	return err
}

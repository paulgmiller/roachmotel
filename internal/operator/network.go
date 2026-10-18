package operator

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NetworkService resolves the namespace-qualified Kubernetes Service in the URL.
func NetworkService(u *url.URL) (string, string, error) {
	parts := strings.Split(strings.TrimSuffix(u.Hostname(), "."), ".")
	if len(parts) < 2 || (len(parts) > 2 && parts[2] != "svc") || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("application endpoint must be a namespace-qualified Kubernetes Service DNS name")
	}
	return parts[1], parts[0], nil
}

func managed(m meta.Object, namespace string) bool {
	return m.GetLabels()["app.kubernetes.io/managed-by"] == "roachmotel" && m.GetAnnotations()[namespaceKey] == namespace
}

func (c *Controller) ensureNetwork(ctx context.Context, tenant string, allow bool) error {
	namespace, name, err := NetworkService(c.Template)
	if err != nil {
		return err
	}
	service, err := c.Client.CoreV1().Services(namespace).Get(ctx, name, meta.GetOptions{})
	if err != nil {
		return err
	}
	if len(service.Spec.Selector) == 0 {
		return errors.New("CockroachDB Service must have a pod selector")
	}
	port := 5432
	if c.Template.Port() != "" {
		port, err = strconv.Atoi(c.Template.Port())
		if err != nil {
			return err
		}
	}
	var target intstr.IntOrString
	for _, p := range service.Spec.Ports {
		if p.Port == int32(port) && (p.Protocol == core.ProtocolTCP || p.Protocol == "") {
			target = p.TargetPort
			if target == (intstr.IntOrString{}) {
				target = intstr.FromInt32(p.Port)
			}
			break
		}
	}
	if target == (intstr.IntOrString{}) {
		return errors.New("application SQL port not found on CockroachDB Service")
	}
	tcp := core.ProtocolTCP
	sqlPorts := []networking.NetworkPolicyPort{{Protocol: &tcp, Port: &target}}
	peer := func(ns string) networking.NetworkPolicyPeer {
		return networking.NetworkPolicyPeer{NamespaceSelector: &meta.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}}}
	}
	// Keep a policy after the last tenant opts out, so deletion cannot open ingress.
	admin := peer(c.Namespace)
	admin.PodSelector = &meta.LabelSelector{MatchLabels: map[string]string{"app": "roachmotel"}}
	baseline := &networking.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: "roachmotel-sql-infrastructure", Namespace: namespace}, Spec: networking.NetworkPolicySpec{
		PodSelector: meta.LabelSelector{MatchLabels: service.Spec.Selector}, PolicyTypes: []networking.PolicyType{networking.PolicyTypeIngress},
		Ingress: []networking.NetworkPolicyIngressRule{{From: []networking.NetworkPolicyPeer{peer(namespace)}}, {From: []networking.NetworkPolicyPeer{admin}, Ports: sqlPorts}},
	}}
	if err := c.applyPolicy(ctx, baseline, c.Namespace); err != nil {
		return err
	}
	if !allow {
		return nil
	}
	policy := &networking.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: strings.ReplaceAll(Identity(tenant), "_", "-"), Namespace: namespace}, Spec: networking.NetworkPolicySpec{
		PodSelector: meta.LabelSelector{MatchLabels: service.Spec.Selector}, PolicyTypes: []networking.PolicyType{networking.PolicyTypeIngress},
		Ingress: []networking.NetworkPolicyIngressRule{{From: []networking.NetworkPolicyPeer{peer(tenant)}, Ports: sqlPorts}},
	}}
	return c.applyPolicy(ctx, policy, tenant)
}

func (c *Controller) applyPolicy(ctx context.Context, desired *networking.NetworkPolicy, tenant string) error {
	policies := c.Client.NetworkingV1().NetworkPolicies(desired.Namespace)
	existing, err := policies.Get(ctx, desired.Name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		desired.Labels = map[string]string{"app.kubernetes.io/managed-by": "roachmotel"}
		desired.Annotations = map[string]string{namespaceKey: tenant}
		_, err = policies.Create(ctx, desired, meta.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if !managed(existing, tenant) {
		return fmt.Errorf("NetworkPolicy %s is not managed by roachmotel", desired.Name)
	}
	if reflect.DeepEqual(existing.Spec, desired.Spec) {
		return nil
	}
	existing.Spec = desired.Spec
	_, err = policies.Update(ctx, existing, meta.UpdateOptions{})
	return err
}

func (c *Controller) revoke(ctx context.Context, ns *core.Namespace) error {
	id := ns.Name
	masterName := strings.ReplaceAll(Identity(ns.Name), "_", "-")
	master, err := c.Client.CoreV1().Secrets(c.Namespace).Get(ctx, masterName, meta.GetOptions{})
	var sqlErr error
	if err == nil {
		if !managed(master, ns.Name) {
			return errors.New("invalid retained credential record")
		}
		sqlErr = c.DB.Revoke(ctx, id)
	} else if !apierrors.IsNotFound(err) {
		sqlErr = err
	}
	// Attempt all cleanup even when SQL or an API operation temporarily fails.
	networkErr := c.ensureNetwork(ctx, ns.Name, false)
	namespace, _, _ := NetworkService(c.Template)
	policies := c.Client.NetworkingV1().NetworkPolicies(namespace)
	policy, policyErr := policies.Get(ctx, masterName, meta.GetOptions{})
	if policyErr == nil && networkErr == nil {
		if !managed(policy, ns.Name) {
			policyErr = errors.New("refusing to delete unrelated NetworkPolicy")
		} else {
			uid := policy.UID
			policyErr = policies.Delete(ctx, masterName, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &uid}})
		}
	}
	if apierrors.IsNotFound(policyErr) {
		policyErr = nil
	}
	secrets := c.Client.CoreV1().Secrets(ns.Name)
	secret, secretErr := secrets.Get(ctx, SecretName, meta.GetOptions{})
	if secretErr == nil {
		if !managed(secret, ns.Name) {
			secretErr = errors.New("refusing to delete unrelated Secret")
		} else {
			uid := secret.UID
			secretErr = secrets.Delete(ctx, SecretName, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &uid}})
		}
	}
	if apierrors.IsNotFound(secretErr) {
		secretErr = nil
	}
	return errors.Join(sqlErr, networkErr, policyErr, secretErr)
}

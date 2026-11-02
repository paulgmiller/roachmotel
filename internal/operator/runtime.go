package operator

import (
	"context"
	"time"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	changes := handler.EnqueueRequestsFromMapFunc(c.requestsForChange)
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Namespace{}).
		// Only metadata is needed to route changes; do not cache Secret values.
		WatchesMetadata(&core.Secret{}, changes).
		WatchesMetadata(&networking.NetworkPolicy{}, changes).
		WatchesMetadata(&core.Service{}, changes).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1, ReconciliationTimeout: 15 * time.Second}).
		Complete(c)
}

// Reconcile handles a namespace event from the manager's workqueue.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (result reconcile.Result, err error) {
	ns, err := c.Client.CoreV1().Namespaces().Get(ctx, req.Name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return result, nil
	}
	if err != nil {
		return result, atStage("read namespace", err)
	}
	// Keep admin configuration scoped to this request.
	current := *c
	return result, current.reconcileNamespace(ctx, ns)
}

func (c *Controller) requestsForChange(ctx context.Context, obj client.Object) []reconcile.Request {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	dbNamespace, service, _ := NetworkService(c.Template)
	adminChanged := kind == "Secret" && obj.GetNamespace() == c.Namespace && obj.GetName() == "roachmotel-admin"
	serviceChanged := kind == "Service" && obj.GetNamespace() == dbNamespace && obj.GetName() == service
	infrastructureChanged := kind == "NetworkPolicy" && obj.GetNamespace() == dbNamespace && obj.GetName() == "roachmotel-sql-infrastructure"
	if adminChanged || serviceChanged || infrastructureChanged {
		namespaces, err := c.Client.CoreV1().Namespaces().List(ctx, meta.ListOptions{})
		if err != nil {
			log.FromContext(ctx).Error(err, "cannot enqueue namespaces after configuration change")
			return nil
		}
		requests := make([]reconcile.Request, 0, len(namespaces.Items))
		for _, ns := range namespaces.Items {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: ns.Name}})
		}
		return requests
	}
	if tenant := obj.GetAnnotations()[namespaceKey]; tenant != "" && managed(obj, tenant) {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: tenant}}}
	}
	return nil
}

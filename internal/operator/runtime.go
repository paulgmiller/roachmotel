package operator

import (
	"context"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Namespace{}).
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
	return result, c.reconcileNamespace(ctx, ns)
}

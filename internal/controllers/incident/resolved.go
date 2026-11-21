package incident

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// SetupResolved adds the retention controller of Resolved incidents: it deletes each one once
// Resolved for longer than the retention, at resolution.at plus the retention. Resolved incidents
// are in the manager's cache (CacheByObject excludes only Closed), so it watches them there and
// holds nothing of its own. Closed incidents are retained by the Closed controller (SetupClosed);
// neither the Incident controller nor this one deletes the other's.
func SetupResolved(mgr ctrl.Manager, o Options) error {
	r := &resolvedReconciler{
		cache:  mgr.GetClient(),
		kube:   mgr.GetClient(),
		reader: mgr.GetAPIReader(),
		cfg:    o.Checks,
		log:    o.Controller.Logger.WithValues("controller", "resolved-incident"),
		now:    time.Now,
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("resolved-incident").
		WithOptions(o.Controller.ForControllerRuntime()).
		For(&v1alpha1.Incident{}, builder.WithPredicates(predicate.NewPredicateFuncs(isResolved))).
		Complete(r)
}

func isResolved(o client.Object) bool {
	inc, ok := o.(*v1alpha1.Incident)
	return ok && inc.Status.State == v1alpha1.StateResolved
}

type resolvedReconciler struct {
	// cache is the manager's cache of incidents that are not Closed.
	cache  client.Reader
	kube   client.Client
	reader client.Reader
	cfg    Config
	log    logging.Logger
	now    func() time.Time
}

func (r *resolvedReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if r.cfg.Retention <= 0 {
		return reconcile.Result{}, nil
	}
	cached := &v1alpha1.Incident{}
	if err := r.cache.Get(ctx, req.NamespacedName, cached); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !isResolved(cached) || !cached.DeletionTimestamp.IsZero() {
		// Not Resolved (any more), or already going: the Incident controller owns it.
		return reconcile.Result{}, nil
	}
	inc := &v1alpha1.Incident{}
	if err := r.reader.Get(ctx, req.NamespacedName, inc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if inc.UID != cached.UID || !isResolved(inc) || !inc.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	return retain(ctx, r.kube, inc, r.cfg, r.now(), r.log)
}

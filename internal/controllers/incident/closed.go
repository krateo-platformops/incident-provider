package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// FieldState is the Incident CRD's selectable field for status.state.
const FieldState = "status.state"

// CacheByObject is how the manager caches Incidents: every one that is not Closed. Closed is the
// one state nothing leaves (spec.closed cannot be unset), and on a long-lived cluster Closed
// incidents are almost all of them. Caching them made the controller's memory grow with history:
// it was OOMKilled at 128Mi holding 2,548 Closed incidents (2026-11-17). An incident that turns
// Closed leaves this cache, and the Closed controller (SetupClosed) takes it over.
func CacheByObject() map[client.Object]cache.ByObject {
	return map[client.Object]cache.ByObject{
		&v1alpha1.Incident{}: {Field: fields.OneTermNotEqualSelector(FieldState, string(v1alpha1.StateClosed))},
	}
}

// maxClosedRequeue bounds how long a Closed incident waits for its retention before it is read again.
const maxClosedRequeue = 6 * time.Hour

// SetupClosed adds the controller of Closed incidents. It watches their METADATA only, through a
// cache of its own (the manager's cache holds one configuration per kind, and the Incident entry
// excludes Closed). It does the two things a Closed incident still needs:
//   - deletion: it removes the check pods and ConfigMaps left for the incident, then provider-
//     runtime's finalizer, so a Closed incident deleted by a person or by retention goes away;
//   - retention: it reads the one incident past any cache (status.resolution.at is not metadata)
//     and deletes it once Closed for longer than the retention, pinned to its UID.
//
// Memory: metadata of Closed incidents, managedFields stripped (nothing here reads them; the
// Incident controller's closedBy does, and its cache keeps them), and one incident at a time.
func SetupClosed(mgr ctrl.Manager, o Options) error {
	gvk := v1alpha1.IncidentGroupVersionKind
	c, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme:               mgr.GetScheme(),
		Mapper:               mgr.GetRESTMapper(),
		HTTPClient:           mgr.GetHTTPClient(),
		DefaultFieldSelector: fields.OneTermEqualSelector(FieldState, string(v1alpha1.StateClosed)),
		DefaultTransform:     cache.TransformStripManagedFields(),
	})
	if err != nil {
		return fmt.Errorf("cannot create the Closed incidents cache: %w", err)
	}
	if err := mgr.Add(c); err != nil {
		return err
	}
	cfg := o.Checks
	r := &closedReconciler{
		cache:  c,
		kube:   mgr.GetClient(),
		reader: mgr.GetAPIReader(),
		pods:   o.Pods.Namespace,
		cfg:    cfg,
		log:    o.Controller.Logger.WithValues("controller", "closed-incident"),
		now:    time.Now,
	}
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(gvk)
	return ctrl.NewControllerManagedBy(mgr).
		Named("closed-incident").
		WithOptions(o.Controller.ForControllerRuntime()).
		WatchesRawSource(source.Kind(c, m, &handler.TypedEnqueueRequestForObject[*metav1.PartialObjectMetadata]{})).
		Complete(r)
}

type closedReconciler struct {
	// cache holds the metadata of Closed incidents.
	cache  client.Reader
	kube   client.Client
	reader client.Reader
	pods   string
	cfg    Config
	log    logging.Logger
	now    func() time.Time
}

func (r *closedReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(v1alpha1.IncidentGroupVersionKind)
	if err := r.cache.Get(ctx, req.NamespacedName, m); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !m.GetDeletionTimestamp().IsZero() {
		return reconcile.Result{}, r.finalize(ctx, m)
	}
	if r.cfg.Retention <= 0 {
		return reconcile.Result{}, nil
	}
	inc := &v1alpha1.Incident{}
	if err := r.reader.Get(ctx, req.NamespacedName, inc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if inc.UID != m.UID || inc.Status.State != v1alpha1.StateClosed {
		// Recreated, or not Closed after all: the Incident controller owns it.
		return reconcile.Result{}, nil
	}
	now := r.now()
	if !expired(inc, r.cfg, now) {
		if inc.Status.Resolution == nil || inc.Status.Resolution.At.IsZero() {
			return reconcile.Result{}, nil
		}
		wait := inc.Status.Resolution.At.Add(r.cfg.Retention).Sub(now) + time.Second
		return reconcile.Result{RequeueAfter: min(wait, maxClosedRequeue)}, nil
	}
	uid := inc.UID
	if err := r.kube.Delete(ctx, inc, client.Preconditions{UID: &uid}); resource.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, fmt.Errorf("cannot delete incident past its retention: %w", err)
	}
	r.log.Info("deleted after its retention", "incident", req.String(), "state", string(inc.Status.State),
		"resolvedBy", string(inc.Status.Resolution.By), "resolvedAt", inc.Status.Resolution.At.UTC().Format(time.RFC3339),
		"retention", r.cfg.Retention.String())
	return reconcile.Result{}, nil
}

// finalize deletes the incident's check pods and ConfigMaps, then removes provider-runtime's
// finalizer with a merge patch that carries the resourceVersion read.
func (r *closedReconciler) finalize(ctx context.Context, m *metav1.PartialObjectMetadata) error {
	if !controllerutil.ContainsFinalizer(m, reconciler.FinalizerName) {
		return nil
	}
	sel := client.MatchingLabels{labelIncidentUID: string(m.UID)}
	var pods corev1.PodList
	if err := r.kube.List(ctx, &pods, client.InNamespace(r.pods), sel); err != nil {
		return fmt.Errorf("cannot list check pods: %w", err)
	}
	for i := range pods.Items {
		if err := r.kube.Delete(ctx, &pods.Items[i], client.PropagationPolicy("Background")); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete check pod %s: %w", pods.Items[i].Name, err)
		}
	}
	var cms corev1.ConfigMapList
	if err := r.kube.List(ctx, &cms, client.InNamespace(r.pods), sel); err != nil {
		return fmt.Errorf("cannot list check ConfigMaps: %w", err)
	}
	for i := range cms.Items {
		if err := r.kube.Delete(ctx, &cms.Items[i]); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete check ConfigMap %s: %w", cms.Items[i].Name, err)
		}
	}
	var keep []string
	for _, f := range m.GetFinalizers() {
		if f != reconciler.FinalizerName {
			keep = append(keep, f)
		}
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": m.GetResourceVersion(), "finalizers": keep}})
	if err != nil {
		return err
	}
	if err := r.kube.Patch(ctx, m, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("cannot remove the finalizer: %w", err)
	}
	r.log.Info("finalized a deleted Closed incident", "incident", m.GetNamespace()+"/"+m.GetName(),
		"checkPods", len(pods.Items), "checkConfigMaps", len(cms.Items))
	return nil
}

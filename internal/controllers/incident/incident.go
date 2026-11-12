// Package incident is the controller of Incident managed resources. An Incident has no external
// resource: Observe always finds it, and decides from the Incident whether its current step, the
// precondition while Open or verify while Verifying, has a result to record or a run to start.
// Check pods only carry the runs. Observe also reads, once a poll interval, whether the Alert that
// opened an Open or Verifying incident still exists.
package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
	"github.com/krateo-platformops/incident-provider/internal/controllers/common/option"
)

var errNotIncident = errors.New("managed resource is not an Incident")

const actionReconcile event.Action = "Reconcile"

// AlertGroupVersionKind is the Alert an Incident's spec.alertRef names. alert-provider serves it.
var AlertGroupVersionKind = schema.GroupVersionKind{Group: "observability.krateo.io", Version: "v1alpha1", Kind: "Alert"}

// Options configure the Incident controller.
type Options struct {
	Controller option.ControllerOptions
	// Checks is the check cadence; its PollInterval is taken from Controller.
	Checks Config
	Pods   CheckPods
	// Discovery reads whether the cluster serves the target of an applyAction. Nil: a discovery
	// client of the manager's config. It is never cached: the manager's RESTMapper only learns
	// new versions, and keeps answering for a version the cluster stopped serving.
	Discovery discovery.DiscoveryInterface
}

// Setup adds the Incident controller to the manager.
func Setup(mgr ctrl.Manager, o Options) error {
	name := reconciler.ControllerName(v1alpha1.IncidentGroupKind)
	log := o.Controller.Logger.WithValues("controller", name)

	recorder, err := eventrecorder.Create(context.Background(), mgr.GetConfig(), name, nil)
	if err != nil {
		return fmt.Errorf("failed to create event recorder: %w", err)
	}
	rec := event.NewAPIRecorder(recorder)

	cfg := o.Checks
	cfg.PollInterval = o.Controller.PollInterval
	runs := newLastRuns()

	disc := o.Discovery
	if disc == nil {
		d, err := discovery.NewDiscoveryClientForConfigAndClient(mgr.GetConfig(), mgr.GetHTTPClient())
		if err != nil {
			return fmt.Errorf("failed to create discovery client: %w", err)
		}
		disc = d
	}

	r := reconciler.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.IncidentGroupVersionKind),
		// Alerts are read past the cache, one at a time: no informer on every Alert, and a
		// missing Alert CRD is one failed read, not a cache that never syncs.
		reconciler.WithExternalConnecter(&connector{kube: mgr.GetClient(), alerts: mgr.GetAPIReader(), cfg: cfg, pods: o.Pods,
			runs: runs, alertReads: newLastRuns(), discovery: disc, targetReads: newTargetReads(), reader: mgr.GetAPIReader(),
			log: log, rec: rec}),
		reconciler.WithPollInterval(cfg.PollInterval),
		reconciler.WithPollIntervalHook(runs.pollIntervalHook),
		reconciler.WithLogger(log),
		reconciler.WithRecorder(rec),
		reconciler.WithTimeout(o.Controller.Timeout),
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.Controller.ForControllerRuntime()).
		For(&v1alpha1.Incident{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(incidentOfPod)).
		Complete(ratelimiter.New(name, r, o.Controller.GlobalRateLimiter))
}

// incidentOfPod maps a check pod to its Incident, so a finished check is read at once.
func incidentOfPod(_ context.Context, o client.Object) []reconcile.Request {
	a := o.GetAnnotations()
	ns, name := a[annotationIncidentNamespace], a[annotationIncidentName]
	if ns == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// lastRuns is when each incident's last check ended. It is held in memory only: a check that
// gives the same result writes nothing, so the status does not say when the last one ran. After a
// restart every incident's check is due at once.
type lastRuns struct {
	mu sync.Mutex
	at map[types.UID]time.Time
}

func newLastRuns() *lastRuns {
	return &lastRuns{at: map[types.UID]time.Time{}}
}

func (l *lastRuns) get(uid types.UID) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.at[uid]
}

// seen records a run of uid that ended at at, unless a later one is known.
func (l *lastRuns) seen(uid types.UID, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at.After(l.at[uid]) {
		l.at[uid] = at
	}
}

func (l *lastRuns) forget(uid types.UID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.at, uid)
}

// pollIntervalHook requeues an incident when its next check falls due, within the poll interval.
func (l *lastRuns) pollIntervalHook(mg resource.Managed, poll time.Duration) time.Duration {
	inc, ok := mg.(*v1alpha1.Incident)
	if !ok || currentScript(inc) == "" {
		return poll
	}
	d := time.Until(nextCheckAt(inc, l.get(inc.UID), poll))
	switch {
	case d <= 0:
		// A check is running or starting; its pod's events requeue the incident.
		return poll
	case d < time.Second:
		return time.Second
	case d > poll:
		return poll
	}
	return d
}

type connector struct {
	kube   client.Client
	alerts client.Reader
	cfg    Config
	pods   CheckPods
	runs   *lastRuns
	// alertReads is when each incident's Alert was last read.
	alertReads *lastRuns
	// discovery and targetReads read whether the cluster serves each applyAction's target.
	discovery   discovery.DiscoveryInterface
	targetReads *targetReads
	// reader reads an incident past the cache, to recheck a reanalysis request after a conflict.
	reader client.Reader
	log    logging.Logger
	rec    event.Recorder
}

func (c *connector) Connect(_ context.Context, mg resource.Managed) (reconciler.ExternalClient, error) {
	inc, ok := mg.(*v1alpha1.Incident)
	if !ok {
		return nil, errNotIncident
	}
	return &external{
		kube:        c.kube,
		alerts:      c.alerts,
		cfg:         c.cfg,
		pods:        c.pods,
		runs:        c.runs,
		alertReads:  c.alertReads,
		discovery:   c.discovery,
		targetReads: c.targetReads,
		reader:      c.reader,
		log:         c.log.WithValues("incident", inc.Namespace+"/"+inc.Name),
		rec:         c.rec,
		now:         time.Now,
	}, nil
}

// plan is what Observe found for Update to act on. A new external serves each reconcile.
type plan struct {
	// changes are the state changes to announce once the status is persisted.
	changes []change
	// dirty means the status changed beyond conditions.
	dirty bool
	// due means the current script has no running check and its next run is due.
	due bool
	// from is when the wait for the due run started; it names the run's pod.
	from time.Time
	// consumeApplied means spec.applied was acted on and goes back to false.
	consumeApplied bool
	// requestReanalysis means ApplyActionStale is True and its episode has no reanalysis yet: the
	// controller sets spec.reanalysis.
	requestReanalysis bool
	// stale are the check pods to delete: finished ones, and those of a script no longer current.
	stale []corev1.Pod
	// live is the running check pod of the current script.
	live *corev1.Pod
	// expired means the incident is Closed or Resolved past its retention: it is deleted.
	expired bool
}

func (p plan) String() string {
	var parts []string
	for _, c := range p.changes {
		parts = append(parts, c.message)
	}
	if p.consumeApplied {
		parts = append(parts, "spec.applied consumed")
	}
	if p.requestReanalysis {
		parts = append(parts, "reanalysis to request")
	}
	if n := len(p.stale); n > 0 {
		parts = append(parts, fmt.Sprintf("%d check pod(s) to delete", n))
	}
	if p.due {
		parts = append(parts, "check due")
	}
	if p.expired {
		parts = append(parts, "retention passed")
	}
	return strings.Join(parts, "; ")
}

type external struct {
	kube        client.Client
	alerts      client.Reader
	cfg         Config
	pods        CheckPods
	runs        *lastRuns
	alertReads  *lastRuns
	discovery   discovery.DiscoveryInterface
	targetReads *targetReads
	reader      client.Reader
	log         logging.Logger
	rec         event.Recorder
	now         func() time.Time
	plan        plan
}

// Observe reads the incident's check pods, records the finished ones and applies the resulting
// transitions and the human decisions in spec to the status. It changes nothing outside the
// Incident object held in memory; Update persists and acts. The Incident always exists: a status
// to persist, a check pod to delete or a check that is due make it out of date.
func (e *external) Observe(ctx context.Context, mg resource.Managed) (reconciler.ExternalObservation, error) {
	inc, ok := mg.(*v1alpha1.Incident)
	if !ok {
		return reconciler.ExternalObservation{}, errNotIncident
	}

	pods, cms, err := e.list(ctx, inc)
	if err != nil {
		return reconciler.ExternalObservation{}, err
	}
	if meta.WasDeleted(inc) {
		e.runs.forget(inc.UID)
		e.alertReads.forget(inc.UID)
		if e.targetReads != nil {
			e.targetReads.forget(inc.UID)
		}
		// A deleted Incident is Deleting. provider-runtime sets that itself while it deletes, after
		// a successful Observe and after a failed one (v1.8.2), so this write is not required;
		// it keeps every successful Observe answering Ready.
		inc.SetConditions(prv1.Deleting())
		return reconciler.ExternalObservation{ResourceExists: len(pods)+len(cms) > 0}, nil
	}

	now := e.now()
	before := inc.Status.DeepCopy()
	changes, consumed := applySpec(inc, now)
	e.plan = plan{changes: changes, consumeApplied: consumed}
	e.plan.changes = append(e.plan.changes, observeAlert(inc, e.cfg, e.readAlert(ctx, inc, now), now)...)
	e.plan.changes = append(e.plan.changes, observeApplyAction(inc, e.readApplyTarget(inc, now), now)...)

	sort.Slice(pods, func(i, j int) bool { return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp) })
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		r, done := resultOf(pod, e.cfg.CheckTimeout, now)
		switch {
		case !done && r.script == currentScript(inc) && e.plan.live == nil:
			e.plan.live = pod
		case !done:
			e.plan.stale = append(e.plan.stale, *pod)
		default:
			// The result of a script that is no longer current is dropped: the state it tested
			// has moved on. A result recorded before, whose pod outlived a failed delete, records
			// the same result again and changes nothing.
			if r.script == currentScript(inc) {
				e.plan.changes = append(e.plan.changes, record(inc, e.cfg, r, now)...)
				e.runs.seen(inc.UID, r.at)
			}
			e.plan.stale = append(e.plan.stale, *pod)
		}
	}
	syncChecksUnknown(inc, now)
	e.plan.dirty = consumed || !equality.Semantic.DeepEqual(before, &inc.Status)
	lastRun := e.runs.get(inc.UID)
	e.plan.due = e.plan.live == nil && checkDue(inc, e.cfg, lastRun, now)
	e.plan.requestReanalysis = staleReanalysisDue(inc)
	e.plan.from = checkFrom(inc, lastRun)
	// After this pass's own changes: an incident this pass closed has a fresh resolution.at.
	e.plan.expired = expired(inc, e.cfg, now)

	inc.SetConditions(prv1.Available())

	if e.plan.dirty || len(e.plan.stale) > 0 || e.plan.due || e.plan.requestReanalysis || e.plan.expired {
		return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false, Diff: e.plan.String()}, nil
	}
	return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}

// Create is never called: Observe always finds the Incident.
func (e *external) Create(context.Context, resource.Managed) error {
	return nil
}

// Update persists the status Observe computed, then consumes spec.applied, deletes the stale check
// pods, and starts the next check when it is due. The status goes first, so a check result or a
// consumed spec.applied is never lost with its pod or its flag.
func (e *external) Update(ctx context.Context, mg resource.Managed) error {
	inc, ok := mg.(*v1alpha1.Incident)
	if !ok {
		return errNotIncident
	}

	if e.plan.dirty {
		if err := e.kube.Status().Update(ctx, inc); err != nil {
			return fmt.Errorf("cannot persist incident status: %w", err)
		}
		for _, c := range e.plan.changes {
			e.rec.Event(inc, event.Normal(event.Reason(c.reason), actionReconcile, c.message))
			e.log.Info(c.message, append([]any{"reason", c.reason}, c.kv...)...)
		}
	}
	if e.plan.consumeApplied {
		inc.Spec.Applied = false
		if err := e.kube.Update(ctx, inc); err != nil {
			return fmt.Errorf("cannot reset spec.applied: %w", err)
		}
	}
	for i := range e.plan.stale {
		if err := e.kube.Delete(ctx, &e.plan.stale[i], client.PropagationPolicy("Background")); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete check pod %s: %w", e.plan.stale[i].Name, err)
		}
	}
	if e.plan.requestReanalysis {
		if err := e.requestReanalysis(ctx, inc); err != nil {
			return err
		}
	}
	if e.plan.due {
		return e.startCheck(ctx, inc, currentScript(inc))
	}
	if e.plan.expired {
		// Last, and pinned to this incident's UID: a recreated incident of the same name is
		// never deleted in its place. Its check pods and ConfigMaps go in Delete, as for any
		// deletion.
		uid := inc.UID
		if err := e.kube.Delete(ctx, inc, client.Preconditions{UID: &uid}); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete incident past its retention: %w", err)
		}
		e.log.Info("deleted after its retention", "state", string(inc.Status.State),
			"resolvedBy", string(inc.Status.Resolution.By), "resolvedAt", inc.Status.Resolution.At.UTC().Format(time.RFC3339),
			"retention", e.cfg.Retention.String())
	}
	return nil
}

// Delete removes every check pod and ConfigMap of the incident.
func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	inc, ok := mg.(*v1alpha1.Incident)
	if !ok {
		return errNotIncident
	}
	pods, cms, err := e.list(ctx, inc)
	if err != nil {
		return err
	}
	for i := range pods {
		if err := e.kube.Delete(ctx, &pods[i], client.PropagationPolicy("Background")); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete check pod %s: %w", pods[i].Name, err)
		}
	}
	for i := range cms {
		if err := e.kube.Delete(ctx, &cms[i]); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete check ConfigMap %s: %w", cms[i].Name, err)
		}
	}
	return nil
}

func (e *external) list(ctx context.Context, inc *v1alpha1.Incident) ([]corev1.Pod, []corev1.ConfigMap, error) {
	opts := []client.ListOption{client.InNamespace(e.pods.Namespace), client.MatchingLabels(selector(inc))}
	var pods corev1.PodList
	if err := e.kube.List(ctx, &pods, opts...); err != nil {
		return nil, nil, fmt.Errorf("cannot list check pods: %w", err)
	}
	var cms corev1.ConfigMapList
	if err := e.kube.List(ctx, &cms, opts...); err != nil {
		return nil, nil, fmt.Errorf("cannot list check ConfigMaps: %w", err)
	}
	return pods.Items, cms.Items, nil
}

// readAlert reads whether the Alert in spec.alertRef of an Open or Verifying incident exists, at
// most once a poll interval. Only a NotFound that names the Alert counts as gone: a missing Alert
// CRD, a denied read or an unreachable API server leave it unread.
func (e *external) readAlert(ctx context.Context, inc *v1alpha1.Incident, now time.Time) alertState {
	if e.alerts == nil {
		return alertUnread
	}
	if s := inc.Status.State; s != v1alpha1.StateOpen && s != v1alpha1.StateVerifying {
		return alertUnread
	}
	if last := e.alertReads.get(inc.UID); !last.IsZero() && now.Sub(last) < e.cfg.PollInterval {
		return alertUnread
	}
	e.alertReads.seen(inc.UID, now)
	ref := inc.Spec.AlertRef
	alert := &metav1.PartialObjectMetadata{}
	alert.SetGroupVersionKind(AlertGroupVersionKind)
	err := e.alerts.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, alert)
	switch {
	case err == nil:
		return alertFound
	case alertNotFoundErr(err, ref.Name):
		return alertNotFound
	}
	e.log.Debug("cannot read the incident's Alert", "alert", ref.Namespace+"/"+ref.Name, "error", err.Error())
	return alertUnread
}

// alertNotFoundErr reports whether err is the API server's NotFound for the Alert named name. A
// NotFound for the resource type itself, from a REST mapping that outlived the Alert CRD, names
// no object.
func alertNotFoundErr(err error, name string) bool {
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return false
	}
	s := st.Status()
	return s.Reason == metav1.StatusReasonNotFound && s.Details != nil && s.Details.Name == name
}

// readApplyTarget reads from discovery whether the cluster serves the target of an Open or
// Verifying incident's applyAction, the states in which the portal can send it: at most once a
// poll interval for the same target, and at once for a new one. nil: it was not read. A failed
// read is logged and not counted, so the next reconcile reads again; it changes no state, like a
// check that gives no verdict.
func (e *external) readApplyTarget(inc *v1alpha1.Incident, now time.Time) *applyTarget {
	a := applyActionOf(inc)
	if a == nil || e.discovery == nil || e.targetReads == nil {
		return nil
	}
	if s := inc.Status.State; s != v1alpha1.StateOpen && s != v1alpha1.StateVerifying {
		return nil
	}
	target := a.APIVersion + " " + a.Resource
	if !e.targetReads.due(inc.UID, target, now, e.cfg.PollInterval) {
		return nil
	}
	t, err := lookupApplyTarget(e.discovery, a.APIVersion, a.Resource)
	if err != nil {
		e.log.Info("cannot read whether the cluster serves the applyAction target; ApplyActionStale unchanged",
			"apiVersion", a.APIVersion, "resource", a.Resource, "error", err.Error())
		return nil
	}
	e.targetReads.seen(inc.UID, target, now)
	return &t
}

// maxRequestAttempts bounds the conflicts requestReanalysis retries.
const maxRequestAttempts = 3

// requestReanalysis sets spec.reanalysis to the controller's request, cause ApplyActionStale. It
// runs after the status write that made ApplyActionStale True, so the episode the request answers
// is persisted first. It patches spec.reanalysis alone, with a JSON merge patch that carries the
// resourceVersion read: a write in between, such as a person's request, makes it conflict, and the
// incident is read again and the guards checked again before another attempt. The same holds for a
// reconcile that runs on a cached incident from before the controller's own request: its patch
// conflicts, and the fresh read finds the request pending.
func (e *external) requestReanalysis(ctx context.Context, inc *v1alpha1.Incident) error {
	cur := inc
	for range maxRequestAttempts {
		if !staleReanalysisDue(cur) {
			return nil
		}
		episode := cur.GetCondition(v1alpha1.TypeApplyActionStale)
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": cur.ResourceVersion},
			"spec": map[string]any{"reanalysis": v1alpha1.ReanalysisRequest{
				Cause:    v1alpha1.ReanalysisCauseApplyActionStale,
				Feedback: v1alpha1.ReanalysisFeedbackApplyActionStale,
			}},
		})
		if err != nil {
			return fmt.Errorf("cannot encode the reanalysis request: %w", err)
		}
		// The patch's response refreshes cur. When cur is inc, the reconciler's next status write
		// carries the new resourceVersion; after a conflict, inc keeps its old one and that write
		// conflicts and is retried, never overwriting what was read.
		err = e.kube.Patch(ctx, cur, client.RawPatch(types.MergePatchType, patch))
		if err == nil {
			msg := "requested a reanalysis, cause ApplyActionStale: " + episode.Message
			e.rec.Event(cur, event.Normal(event.Reason(reasonReanalysisRequested), actionReconcile, msg))
			e.log.Info(msg, "reason", reasonReanalysisRequested, "cause", string(v1alpha1.ReanalysisCauseApplyActionStale),
				"state", string(cur.Status.State))
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("cannot request a reanalysis: %w", err)
		}
		fresh := &v1alpha1.Incident{}
		if err := e.reader.Get(ctx, client.ObjectKeyFromObject(cur), fresh); err != nil {
			return fmt.Errorf("cannot read the incident again to request a reanalysis: %w", err)
		}
		cur = fresh
	}
	return fmt.Errorf("cannot request a reanalysis: %d conflicting writes", maxRequestAttempts)
}

// reasonReanalysisRequested is the Event and log reason of the controller's reanalysis request.
const reasonReanalysisRequested = "ReanalysisRequested"

// startCheck creates the check pod, then the ConfigMap with its script, owned by the pod. The
// kubelet waits for the ConfigMap before it starts the container.
func (e *external) startCheck(ctx context.Context, inc *v1alpha1.Incident, s v1alpha1.Script) error {
	pod := e.pods.Pod(inc, s, e.plan.from)
	if err := e.kube.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("cannot create check pod: %w", err)
		}
		if err := e.kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return fmt.Errorf("cannot get check pod: %w", err)
		}
	}
	if err := e.kube.Create(ctx, e.pods.ConfigMap(inc, pod, s)); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("cannot create check ConfigMap: %w", err)
	}
	e.log.Debug("started check", "script", s, "pod", pod.Name)
	return nil
}

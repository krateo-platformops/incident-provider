package incident

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/plumbing/kubeutil"
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

var errNotIncidentApply = errors.New("managed resource is not an IncidentApply")

const actionApply event.Action = "Apply"

// maxApplyRequeue bounds the wait of a finished IncidentApply for its TTL.
const maxApplyRequeue = time.Hour

// The secrets authn writes at login, <username>-clientconfig, and their keys.
const (
	credentialsSuffix = "-clientconfig"
	keyClientCert     = "client-certificate-data"
	keyClientKey      = "client-key-data"
)

// The admission policy and binding that write spec.requestedBy.
var (
	requesterPolicyGVK        = schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingAdmissionPolicy"}
	requesterPolicyBindingGVK = schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingAdmissionPolicyBinding"}
)

// ApplyOptions configure the IncidentApply controller.
type ApplyOptions struct {
	Controller option.ControllerOptions
	Pods       CheckPods
	Applies    ApplyConfig
}

// ApplyConfig is how apply runs are made and kept.
type ApplyConfig struct {
	// Timeout bounds one run. It is the apply pod's activeDeadlineSeconds.
	Timeout time.Duration

	// TTL is how long a finished IncidentApply is kept before the controller deletes it.
	TTL time.Duration

	// CredentialsNamespace holds the <username>-clientconfig Secrets authn writes at login.
	CredentialsNamespace string
}

// apiServer is where apply pods send kubectl: the apiserver by IP, since check pods resolve no
// names, and the CA that signs its serving certificate.
type apiServer struct {
	host string
	ca   []byte
}

func apiServerOf(rc *rest.Config) (apiServer, error) {
	if rc.Host == "" {
		return apiServer{}, errors.New("the rest config names no apiserver")
	}
	ca := rc.CAData
	if len(ca) == 0 && rc.CAFile != "" {
		b, err := os.ReadFile(rc.CAFile)
		if err != nil {
			return apiServer{}, fmt.Errorf("cannot read the apiserver CA: %w", err)
		}
		ca = b
	}
	return apiServer{host: rc.Host, ca: ca}, nil
}

// logReader returns the end of an apply pod's output, at most MaxApplyOutput bytes.
type logReader func(ctx context.Context, pod *corev1.Pod) (string, error)

func podLogs(cs kubernetes.Interface) logReader {
	return func(ctx context.Context, pod *corev1.Pod) (string, error) {
		rc, err := cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: containerName,
			TailLines: ptr.To(int64(400)),
		}).Stream(ctx)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		t := &tailBuffer{max: v1alpha1.MaxApplyOutput}
		if _, err := io.Copy(t, rc); err != nil {
			return "", err
		}
		return strings.ToValidUTF8(string(t.b), ""), nil
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if n := len(t.b); n > t.max {
		t.b = append(t.b[:0], t.b[n-t.max:]...)
	}
	return len(p), nil
}

// SetupApply adds the IncidentApply controller to the manager.
func SetupApply(mgr ctrl.Manager, o ApplyOptions) error {
	name := reconciler.ControllerName(v1alpha1.IncidentApplyGroupKind)
	log := o.Controller.Logger.WithValues("controller", name)

	recorder, err := eventrecorder.Create(context.Background(), mgr.GetConfig(), name, nil)
	if err != nil {
		return fmt.Errorf("failed to create event recorder: %w", err)
	}
	rec := event.NewAPIRecorder(recorder)

	api, err := apiServerOf(mgr.GetConfig())
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("cannot create clientset: %w", err)
	}

	r := reconciler.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.IncidentApplyGroupVersionKind),
		reconciler.WithExternalConnecter(&applyConnector{
			kube:   mgr.GetClient(),
			reader: mgr.GetAPIReader(),
			cfg:    o.Applies,
			pods:   o.Pods,
			api:    api,
			logs:   podLogs(cs),
			log:    log,
			rec:    rec,
		}),
		reconciler.WithPollInterval(o.Controller.PollInterval),
		reconciler.WithPollIntervalHook(applyPollIntervalHook(o.Applies.TTL)),
		reconciler.WithLogger(log),
		reconciler.WithRecorder(rec),
		reconciler.WithTimeout(o.Controller.Timeout),
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.Controller.ForControllerRuntime()).
		For(&v1alpha1.IncidentApply{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(applyOfPod)).
		Complete(ratelimiter.New(name, r, o.Controller.GlobalRateLimiter))
}

// applyOfPod maps an apply pod to its IncidentApply, so a finished run is read at once.
func applyOfPod(_ context.Context, o client.Object) []reconcile.Request {
	a := o.GetAnnotations()
	ns, name := a[annotationApplyNamespace], a[annotationApplyName]
	if ns == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// applyPollIntervalHook requeues a finished IncidentApply when its TTL runs out.
func applyPollIntervalHook(ttl time.Duration) reconciler.PollIntervalHook {
	return func(mg resource.Managed, poll time.Duration) time.Duration {
		ia, ok := mg.(*v1alpha1.IncidentApply)
		if !ok || !ia.Status.Phase.Terminal() || ia.Status.FinishedAt == nil {
			return poll
		}
		d := time.Until(ia.Status.FinishedAt.Add(ttl))
		switch {
		case d < time.Second:
			return time.Second
		case d > maxApplyRequeue:
			return maxApplyRequeue
		}
		return d
	}
}

type applyConnector struct {
	kube   client.Client
	reader client.Reader
	cfg    ApplyConfig
	pods   CheckPods
	api    apiServer
	logs   logReader
	log    logging.Logger
	rec    event.Recorder
}

func (c *applyConnector) Connect(_ context.Context, mg resource.Managed) (reconciler.ExternalClient, error) {
	ia, ok := mg.(*v1alpha1.IncidentApply)
	if !ok {
		return nil, errNotIncidentApply
	}
	return &applyExternal{
		kube:   c.kube,
		reader: c.reader,
		cfg:    c.cfg,
		pods:   c.pods,
		api:    c.api,
		logs:   c.logs,
		log:    c.log.WithValues("incidentapply", ia.Namespace+"/"+ia.Name),
		rec:    c.rec,
		now:    time.Now,
	}, nil
}

// applyPlan is what Observe found for Create and Update to act on.
type applyPlan struct {
	// base is the IncidentApply as read, before Observe changed it: the status is written as a
	// merge patch from it.
	base *v1alpha1.IncidentApply
	// start is the admitted run, for Create.
	start *applyStart
	// dirty means the status changed beyond conditions.
	dirty bool
	// finished means the request reached a terminal phase in this reconcile: it gets its Event.
	finished bool
	// run is the run that ended, to record on the incident before the status is persisted.
	run *result
	// pod is the apply pod, to delete once the request is terminal.
	pod *corev1.Pod
	// expired means the TTL has passed: the IncidentApply goes.
	expired bool
}

// applyStart is what an admitted run needs.
type applyStart struct {
	script     string
	kubeconfig []byte
}

type applyExternal struct {
	kube   client.Client
	reader client.Reader
	cfg    ApplyConfig
	pods   CheckPods
	api    apiServer
	logs   logReader
	log    logging.Logger
	rec    event.Recorder
	now    func() time.Time
	plan   applyPlan
}

// Observe admits a new request or rejects it, reads the apply pod of a running one, and finds a
// finished one's leftovers and TTL. It changes nothing but the IncidentApply held in memory.
// The apply pod is read past the cache: a run must never start twice.
func (e *applyExternal) Observe(ctx context.Context, mg resource.Managed) (reconciler.ExternalObservation, error) {
	ia, ok := mg.(*v1alpha1.IncidentApply)
	if !ok {
		return reconciler.ExternalObservation{}, errNotIncidentApply
	}

	pod, err := e.getPod(ctx, ia)
	if err != nil {
		return reconciler.ExternalObservation{}, err
	}
	if meta.WasDeleted(ia) {
		// A deleted IncidentApply is Deleting. provider-runtime sets that itself while it deletes, after
		// a successful Observe and after a failed one (v1.8.2), so this write is not required;
		// it keeps every successful Observe answering Ready.
		ia.SetConditions(prv1.Deleting())
		return reconciler.ExternalObservation{ResourceExists: pod != nil}, nil
	}
	e.plan = applyPlan{base: ia.DeepCopy()}
	// The IncidentApply is a request with no external resource: once its pod is read, it is
	// Available whatever the steps below find. A read that fails below fails admitting or
	// recording the run, not this assessment, so its error is wrapped with ReadyAssessed and
	// the runtime keeps Available instead of replacing it with NotObserved.
	ia.SetConditions(prv1.Available())
	now := e.now()

	if ia.Status.Phase == "" {
		switch {
		case pod != nil:
			if err := e.started(ctx, ia, pod); err != nil {
				return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(err)
			}
		case meta.ExternalCreateIncomplete(ia):
			// The pod is read past the cache, so it was never created.
			e.reject(ia, "the run was interrupted before its pod was created; create a new IncidentApply", now)
		default:
			why, start, err := e.admit(ctx, ia, now)
			if err != nil {
				return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(err)
			}
			if why == "" {
				e.plan.start = start
				return reconciler.ExternalObservation{ResourceExists: false}, nil
			}
			e.reject(ia, why, now)
		}
	}

	if ia.Status.Phase == v1alpha1.ApplyRunning {
		if pod == nil {
			e.end(ia, v1alpha1.ApplyFailed, "the apply pod is gone before the run's result was read", nil, "", now)
		} else if r, done := resultOf(pod, e.cfg.Timeout, now); done {
			e.finishRun(ctx, ia, pod, r)
		}
	}

	if ia.Status.Phase.Terminal() {
		if pod != nil && pod.DeletionTimestamp == nil {
			e.plan.pod = pod
		}
		e.plan.expired = ia.Status.FinishedAt != nil && !now.Before(ia.Status.FinishedAt.Add(e.cfg.TTL))
	}

	if e.plan.dirty || e.plan.pod != nil || e.plan.expired {
		return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false, Diff: e.plan.String()}, nil
	}
	return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}

func (p applyPlan) String() string {
	var parts []string
	if p.finished {
		parts = append(parts, "finished")
	}
	if p.dirty && !p.finished {
		parts = append(parts, "status changed")
	}
	if p.pod != nil {
		parts = append(parts, "apply pod to delete")
	}
	if p.expired {
		parts = append(parts, "TTL passed")
	}
	return strings.Join(parts, "; ")
}

// Create starts the admitted run: the apply pod, then the kubeconfig Secret and the script
// ConfigMap, both owned by the pod. The kubelet waits for both before it starts the container.
func (e *applyExternal) Create(ctx context.Context, mg resource.Managed) error {
	ia, ok := mg.(*v1alpha1.IncidentApply)
	if !ok {
		return errNotIncidentApply
	}
	if e.plan.start == nil {
		return nil
	}
	pod := e.pods.ApplyPod(ia, e.cfg.Timeout)
	if err := e.kube.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("cannot create apply pod: %w", err)
		}
		if err := e.reader.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return fmt.Errorf("cannot get apply pod: %w", err)
		}
	}
	if err := e.kube.Create(ctx, e.pods.ApplySecret(ia, pod, e.plan.start.kubeconfig)); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("cannot create apply kubeconfig Secret: %w", err)
	}
	if err := e.kube.Create(ctx, e.pods.ApplyConfigMap(ia, pod, e.plan.start.script)); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("cannot create apply ConfigMap: %w", err)
	}
	e.log.Info("started apply", "pod", pod.Name, "user", ia.Spec.RequestedBy.Username, "incident", ia.Spec.IncidentRef.Name)
	return nil
}

// Update records a finished run on its incident, persists the status, announces the end,
// deletes the apply pod of a terminal request, and deletes a request past its TTL. The incident
// goes first: a run is recorded there once, whatever fails after.
func (e *applyExternal) Update(ctx context.Context, mg resource.Managed) error {
	ia, ok := mg.(*v1alpha1.IncidentApply)
	if !ok {
		return errNotIncidentApply
	}

	if e.plan.run != nil {
		if err := e.recordOnIncident(ctx, ia, *e.plan.run); err != nil {
			return err
		}
	}
	if e.plan.dirty {
		// A patch, not an update: the reconcile that created the pod has just written the
		// object's annotations, and only this controller writes its status.
		if err := e.kube.Status().Patch(ctx, ia, client.MergeFrom(e.plan.base)); err != nil {
			return fmt.Errorf("cannot persist incidentapply status: %w", err)
		}
		if e.plan.finished {
			e.announce(ia)
		}
	}
	if e.plan.pod != nil {
		if err := e.kube.Delete(ctx, e.plan.pod, client.PropagationPolicy("Background")); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete apply pod %s: %w", e.plan.pod.Name, err)
		}
	}
	if e.plan.expired {
		if err := e.kube.Delete(ctx, ia); resource.IgnoreNotFound(err) != nil {
			return fmt.Errorf("cannot delete expired incidentapply: %w", err)
		}
		e.log.Info("deleted after its TTL", "finishedAt", ia.Status.FinishedAt)
	}
	return nil
}

// Delete removes the apply pod; its kubeconfig Secret and ConfigMap go with it.
func (e *applyExternal) Delete(ctx context.Context, mg resource.Managed) error {
	ia, ok := mg.(*v1alpha1.IncidentApply)
	if !ok {
		return errNotIncidentApply
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: applyName(ia), Namespace: e.pods.Namespace}}
	if err := e.kube.Delete(ctx, pod, client.PropagationPolicy("Background")); resource.IgnoreNotFound(err) != nil {
		return fmt.Errorf("cannot delete apply pod %s: %w", pod.Name, err)
	}
	return nil
}

func (e *applyExternal) getPod(ctx context.Context, ia *v1alpha1.IncidentApply) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := e.reader.Get(ctx, types.NamespacedName{Namespace: e.pods.Namespace, Name: applyName(ia)}, pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get apply pod: %w", err)
	}
	return pod, nil
}

// started records that the run's pod exists, with the script it runs.
func (e *applyExternal) started(ctx context.Context, ia *v1alpha1.IncidentApply, pod *corev1.Pod) error {
	ia.Status.Phase = v1alpha1.ApplyRunning
	ia.Status.StartedAt = ptr.To(pod.CreationTimestamp)
	var cm corev1.ConfigMap
	err := e.reader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &cm)
	switch {
	case err == nil:
		ia.Status.Script = cm.Data[scriptKey]
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("cannot get apply ConfigMap: %w", err)
	}
	e.plan.dirty = true
	return nil
}

// finishRun reads a finished run's exit code and output.
func (e *applyExternal) finishRun(ctx context.Context, ia *v1alpha1.IncidentApply, pod *corev1.Pod, r result) {
	output, err := e.logs(ctx, pod)
	var msg string
	switch {
	case r.exit != nil:
		msg = fmt.Sprintf("the apply script exited %d", *r.exit)
	default:
		msg = fmt.Sprintf("the apply script did not finish within %s", e.cfg.Timeout)
	}
	if err != nil {
		msg += fmt.Sprintf("; its output could not be read: %v", err)
	}
	phase := v1alpha1.ApplyFailed
	if r.exit != nil && *r.exit == 0 {
		phase = v1alpha1.ApplySucceeded
	}
	e.end(ia, phase, msg, r.exit, output, r.at)
	e.plan.run = &r
}

func (e *applyExternal) reject(ia *v1alpha1.IncidentApply, why string, now time.Time) {
	e.end(ia, v1alpha1.ApplyRejected, why, nil, "", now)
}

func (e *applyExternal) end(ia *v1alpha1.IncidentApply, phase v1alpha1.ApplyPhase, msg string, exit *int32, output string, at time.Time) {
	ia.Status.Phase = phase
	ia.Status.Message = msg
	ia.Status.ExitCode = exit
	ia.Status.Output = output
	ia.Status.FinishedAt = ptr.To(metav1.NewTime(at).Rfc3339Copy())
	e.plan.dirty = true
	e.plan.finished = true
}

// announce emits the request's one ApplyFinished Event; the portal waits for it.
func (e *applyExternal) announce(ia *v1alpha1.IncidentApply) {
	msg := string(ia.Status.Phase) + ": " + ia.Status.Message
	if ia.Status.Phase == v1alpha1.ApplySucceeded {
		e.rec.Event(ia, event.Normal(v1alpha1.ReasonApplyFinished, actionApply, msg))
	} else {
		e.rec.Event(ia, event.Warning(v1alpha1.ReasonApplyFinished, actionApply, errors.New(msg)))
	}
	e.log.Info(msg, "user", ia.Spec.RequestedBy.Username, "incident", ia.Spec.IncidentRef.Name)
}

// recordOnIncident adds the run to its incident's checks. An incident that is gone, or no longer
// Open or Verifying, records nothing.
func (e *applyExternal) recordOnIncident(ctx context.Context, ia *v1alpha1.IncidentApply, r result) error {
	var inc v1alpha1.Incident
	err := e.kube.Get(ctx, types.NamespacedName{Namespace: ia.Namespace, Name: ia.Spec.IncidentRef.Name}, &inc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot get incident: %w", err)
	}
	changes, changed := recordApply(&inc, r, ia.Spec.RequestedBy.Username)
	if !changed {
		return nil
	}
	if err := e.kube.Status().Update(ctx, &inc); err != nil {
		return fmt.Errorf("cannot record the apply run on incident %s: %w", inc.Name, err)
	}
	for _, c := range changes {
		e.rec.Event(&inc, event.Normal(event.Reason(c.reason), actionReconcile, c.message))
		e.log.Info(c.message, "reason", c.reason, "incident", inc.Name)
	}
	return nil
}

// admit decides whether a new request runs. A non-empty why rejects it; an error retries.
func (e *applyExternal) admit(ctx context.Context, ia *v1alpha1.IncidentApply, now time.Time) (why string, start *applyStart, err error) {
	u := ia.Spec.RequestedBy
	if u.Username == "" {
		return "no requester is recorded", nil, nil
	}
	if why, err := e.vouched(ctx, ia); why != "" || err != nil {
		return why, nil, err
	}

	// Past the cache: the manager does not cache Closed incidents (CacheByObject), and a request
	// for one must be refused because it is Closed, not because it "does not exist".
	var inc v1alpha1.Incident
	name := ia.Spec.IncidentRef.Name
	err = e.reader.Get(ctx, types.NamespacedName{Namespace: ia.Namespace, Name: name}, &inc)
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("incident %s does not exist", name), nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("cannot get incident: %w", err)
	}
	if inc.Status.State != v1alpha1.StateOpen {
		state := inc.Status.State
		if state == "" {
			state = v1alpha1.StateAnalyzing
		}
		return fmt.Sprintf("incident %s is %s; only an Open incident's apply script runs", name, state), nil, nil
	}
	if inc.Status.HowToFix == nil || inc.Status.HowToFix.Apply == "" {
		return fmt.Sprintf("incident %s has no apply script", name), nil, nil
	}

	if other, err := e.busy(ctx, ia); other != "" || err != nil {
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("IncidentApply %s for this incident is already pending or running", other), nil, nil
	}

	allowed, err := e.mayPatch(ctx, u, &inc)
	if err != nil {
		return "", nil, err
	}
	if !allowed {
		return fmt.Sprintf("%s may not patch incident %s; running its apply script takes the same rights as marking it applied", u.Username, name), nil, nil
	}

	kubeconfig, why, err := e.kubeconfigOf(ctx, u.Username, now)
	if why != "" || err != nil {
		return why, nil, err
	}
	return "", &applyStart{script: inc.Status.HowToFix.Apply, kubeconfig: kubeconfig}, nil
}

// vouched checks that the admission policy writing spec.requestedBy, and its binding, existed
// before the request was created. Without them the client wrote requestedBy itself.
func (e *applyExternal) vouched(ctx context.Context, ia *v1alpha1.IncidentApply) (why string, err error) {
	for _, gvk := range []schema.GroupVersionKind{requesterPolicyGVK, requesterPolicyBindingGVK} {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(gvk)
		err := e.reader.Get(ctx, types.NamespacedName{Name: v1alpha1.RequesterPolicy}, o)
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return fmt.Sprintf("the %s %s is not installed (it needs Kubernetes 1.36 or later), so nothing vouches for spec.requestedBy", gvk.Kind, v1alpha1.RequesterPolicy), nil
		}
		if err != nil {
			return "", fmt.Errorf("cannot get %s %s: %w", gvk.Kind, v1alpha1.RequesterPolicy, err)
		}
		if created := o.GetCreationTimestamp(); !created.Before(&ia.CreationTimestamp) {
			return fmt.Sprintf("the %s %s was created after this request, so nothing vouched for its spec.requestedBy", gvk.Kind, v1alpha1.RequesterPolicy), nil
		}
	}
	return "", nil
}

// busy returns the name of another unfinished request for the same incident that goes first: a
// running one, or an older pending one.
func (e *applyExternal) busy(ctx context.Context, ia *v1alpha1.IncidentApply) (string, error) {
	var l v1alpha1.IncidentApplyList
	if err := e.kube.List(ctx, &l, client.InNamespace(ia.Namespace)); err != nil {
		return "", fmt.Errorf("cannot list incidentapplies: %w", err)
	}
	for i := range l.Items {
		o := &l.Items[i]
		if o.UID == ia.UID || o.Spec.IncidentRef.Name != ia.Spec.IncidentRef.Name || o.Status.Phase.Terminal() || meta.WasDeleted(o) {
			continue
		}
		if o.Status.Phase == v1alpha1.ApplyRunning || older(o, ia) {
			return o.Name, nil
		}
	}
	return "", nil
}

func older(a, b *v1alpha1.IncidentApply) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// mayPatch asks the apiserver whether the requester may patch the incident: running its apply
// script takes the rights that marking it applied takes.
func (e *applyExternal) mayPatch(ctx context.Context, u v1alpha1.Requester, inc *v1alpha1.Incident) (bool, error) {
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   u.Username,
		Groups: u.Groups,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: inc.Namespace,
			Verb:      "patch",
			Group:     v1alpha1.Group,
			Resource:  "incidents",
			Name:      inc.Name,
		},
	}}
	if err := e.kube.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("cannot review %s's access: %w", u.Username, err)
	}
	return sar.Status.Allowed, nil
}

// kubeconfigOf builds a kubeconfig from the client certificate authn issued the user at login.
// The certificate must outlast the run.
func (e *applyExternal) kubeconfigOf(ctx context.Context, username string, now time.Time) (kubeconfig []byte, why string, err error) {
	name := kubeutil.MakeDNS1123Compatible(username) + credentialsSuffix
	var sec corev1.Secret
	err = e.reader.Get(ctx, types.NamespacedName{Namespace: e.cfg.CredentialsNamespace, Name: name}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Sprintf("%s has no Krateo login credentials: log in again", username), nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("cannot get the credentials of %s: %w", username, err)
	}

	certPEM, keyPEM := pemOf(sec.Data[keyClientCert]), pemOf(sec.Data[keyClientKey])
	block, _ := pem.Decode(certPEM)
	if block == nil || len(keyPEM) == 0 {
		return nil, fmt.Sprintf("the credentials of %s hold no client certificate", username), nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Sprintf("cannot read the client certificate of %s: %v", username, err), nil
	}
	if cert.Subject.CommonName != username {
		return nil, fmt.Sprintf("the credentials of %s are for %s", username, cert.Subject.CommonName), nil
	}
	if deadline := now.Add(e.cfg.Timeout + pendingGrace); cert.NotAfter.Before(deadline) {
		return nil, fmt.Sprintf("the login of %s expires at %s, before the run could finish: log in again", username, cert.NotAfter.UTC().Format(time.RFC3339)), nil
	}

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["apiserver"] = &clientcmdapi.Cluster{Server: e.api.host, CertificateAuthorityData: e.api.ca}
	cfg.AuthInfos["requester"] = &clientcmdapi.AuthInfo{ClientCertificateData: certPEM, ClientKeyData: keyPEM}
	cfg.Contexts["requester"] = &clientcmdapi.Context{Cluster: "apiserver", AuthInfo: "requester"}
	cfg.CurrentContext = "requester"
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, "", fmt.Errorf("cannot write the kubeconfig of %s: %w", username, err)
	}
	return b, "", nil
}

// pemOf returns PEM data that authn stores either as is or base64-encoded once more.
func pemOf(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "-----BEGIN") {
		return []byte(s)
	}
	d, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return d
}

package incident

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	psaapi "k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/policy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/plumbing/kubeutil/event"
	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler/reconcilertest"
	"github.com/krateo-platformops/provider-runtime/pkg/test"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

const checksNamespace = "krateo-incident-checks"

var template = corev1.PodSpec{
	ServiceAccountName: "incident-check",
	Containers:         []corev1.Container{{Name: "check", Image: "ghcr.io/krateo-platformops/incident-provider-check:0.1.0"}},
}

var pods = CheckPods{Namespace: checksNamespace, Template: template, Timeout: time.Minute}

// kube is a provider-runtime MockClient over an in-memory set of check pods and ConfigMaps. It
// records every write in order.
type kube struct {
	*test.MockClient
	pods      []corev1.Pod
	cms       []corev1.ConfigMap
	created   []client.Object
	calls     []string
	statusErr error
}

func newKube(p ...corev1.Pod) *kube {
	k := &kube{pods: p}
	m := test.NewMockClient()
	m.MockList = func(_ context.Context, l client.ObjectList, _ ...client.ListOption) error {
		switch l := l.(type) {
		case *corev1.PodList:
			l.Items = append([]corev1.Pod(nil), k.pods...)
		case *corev1.ConfigMapList:
			l.Items = append([]corev1.ConfigMap(nil), k.cms...)
		}
		return nil
	}
	m.MockStatusUpdate = func(_ context.Context, _ client.Object, _ ...client.SubResourceUpdateOption) error {
		k.calls = append(k.calls, "update status")
		return k.statusErr
	}
	m.MockUpdate = func(_ context.Context, o client.Object, _ ...client.UpdateOption) error {
		k.calls = append(k.calls, "update spec applied="+boolString(o.(*v1alpha1.Incident).Spec.Applied))
		return nil
	}
	m.MockDelete = func(_ context.Context, o client.Object, _ ...client.DeleteOption) error {
		k.calls = append(k.calls, "delete "+kindOf(o)+" "+o.GetName())
		return nil
	}
	m.MockCreate = func(_ context.Context, o client.Object, _ ...client.CreateOption) error {
		k.calls = append(k.calls, "create "+kindOf(o)+" "+o.GetName())
		for _, p := range k.pods {
			if p.Name == o.GetName() && kindOf(o) == "pod" {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, p.Name)
			}
		}
		if p, ok := o.(*corev1.Pod); ok {
			p.UID = types.UID("pod-uid-" + p.Name)
		}
		k.created = append(k.created, o)
		return nil
	}
	m.MockGet = func(_ context.Context, key client.ObjectKey, o client.Object) error {
		k.calls = append(k.calls, "get "+kindOf(o)+" "+key.Name)
		for _, p := range k.pods {
			if p.Name == key.Name {
				p.DeepCopyInto(o.(*corev1.Pod))
				return nil
			}
		}
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
	}
	k.MockClient = m
	return k
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func kindOf(o client.Object) string {
	switch o.(type) {
	case *corev1.Pod:
		return "pod"
	case *corev1.ConfigMap:
		return "configmap"
	case *v1alpha1.Incident:
		return "incident"
	}
	return "object"
}

func newExternal(k *kube, now time.Time) *external {
	return &external{
		kube: k, cfg: cfg, pods: pods, runs: newLastRuns(), alertReads: newLastRuns(),
		log: logging.NewNopLogger(), rec: event.NewNopRecorder(),
		now: func() time.Time { return now },
	}
}

// checkPod is a check pod of inc for s, created at created. A nil code keeps it running.
func checkPod(inc *v1alpha1.Incident, s v1alpha1.Script, created time.Time, code *int32, finished time.Time) corev1.Pod {
	p := pods.Pod(inc, s, time.Time{})
	p.CreationTimestamp = metav1.NewTime(created)
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = ptr.To(metav1.NewTime(created))
	if code != nil {
		p.Status.Phase = corev1.PodSucceeded
		if *code != 0 {
			p.Status.Phase = corev1.PodFailed
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: *code, FinishedAt: metav1.NewTime(finished)},
		}}}
	}
	return *p
}

func TestObserve(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	open := func(opts ...incOpt) *v1alpha1.Incident {
		return newIncident(v1alpha1.StateOpen, append([]incOpt{withReproduced(metav1.ConditionTrue)}, opts...)...)
	}
	lastRun := t0.Add(9*time.Minute + 30*time.Second)

	cases := []struct {
		name      string
		inc       *v1alpha1.Incident
		lastRun   time.Time
		pods      func(inc *v1alpha1.Incident) []corev1.Pod
		want      reconciler.ExternalObservation
		wantState v1alpha1.State
		wantStale int
		wantCalls []string // of the Update that follows, when the observation asks for one
	}{
		{
			name:      "a due check with no pod is started by Update, with no status write",
			inc:       open(),
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateOpen,
			wantCalls: []string{
				"create pod check-0b7c3a3e1d2f-precondition-0",
				"create configmap check-0b7c3a3e1d2f-precondition-0",
			},
		},
		{
			name:      "a check that is not due waits",
			inc:       open(),
			lastRun:   lastRun,
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantState: v1alpha1.StateOpen,
		},
		{
			name: "a running check is up to date",
			inc:  open(),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				return []corev1.Pod{checkPod(inc, v1alpha1.ScriptPrecondition, now.Add(-10*time.Second), nil, time.Time{})}
			},
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantState: v1alpha1.StateOpen,
		},
		{
			name: "a finished precondition is recorded before its pod is deleted",
			inc:  open(),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				return []corev1.Pod{checkPod(inc, v1alpha1.ScriptPrecondition, now.Add(-30*time.Second), exit(0), now.Add(-20*time.Second))}
			},
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateVerifying,
			wantStale: 1,
			wantCalls: []string{"update status", "delete pod check-0b7c3a3e1d2f-precondition-0"},
		},
		{
			name: "the same result again writes nothing",
			inc:  open(withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 5*time.Minute)})),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				p := checkPod(newIncident(v1alpha1.StateOpen), v1alpha1.ScriptPrecondition, now.Add(-30*time.Second), exit(1), now.Add(-20*time.Second))
				return []corev1.Pod{p}
			},
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateOpen,
			wantStale: 1,
			wantCalls: []string{"delete pod check-0b7c3a3e1d2f-precondition-0"},
		},
		{
			name: "the result of a script no longer current is dropped",
			inc:  newIncident(v1alpha1.StateVerifying, withVerifyingSince(9*time.Minute+50*time.Second)),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				return []corev1.Pod{checkPod(newIncident(v1alpha1.StateOpen), v1alpha1.ScriptPrecondition, now.Add(-30*time.Second), exit(0), now.Add(-5*time.Second))}
			},
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateVerifying,
			wantStale: 1,
			wantCalls: []string{"delete pod check-0b7c3a3e1d2f-precondition-0"},
		},
		{
			name: "a pod that never finished is a timeout",
			inc:  open(),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				p := checkPod(inc, v1alpha1.ScriptPrecondition, now.Add(-3*time.Minute), nil, time.Time{})
				p.Status.Phase = corev1.PodPending
				return []corev1.Pod{p}
			},
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateOpen,
			wantStale: 1,
			// The timeout is recorded at its deadline, a poll interval ago, so the next run is due.
			wantCalls: []string{
				"update status",
				"delete pod check-0b7c3a3e1d2f-precondition-0",
				"create pod check-0b7c3a3e1d2f-precondition-1790345340",
				"create configmap check-0b7c3a3e1d2f-precondition-1790345340",
			},
		},
		{
			name:      "spec.applied is persisted in the status before it is reset",
			inc:       open(withSpec(true, false)),
			lastRun:   lastRun,
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateVerifying,
			wantCalls: []string{"update status", "update spec applied=false"},
		},
		{
			name: "closing deletes the running check",
			inc:  open(withSpec(false, true)),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				return []corev1.Pod{checkPod(inc, v1alpha1.ScriptPrecondition, now.Add(-10*time.Second), nil, time.Time{})}
			},
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateClosed,
			wantStale: 1,
			wantCalls: []string{"update status", "delete pod check-0b7c3a3e1d2f-precondition-0"},
		},
		{
			name:      "an incident flagged by an earlier version runs its precondition again",
			inc:       newIncident(v1alpha1.StateOpen, withReproduced(metav1.ConditionFalse)),
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateOpen,
			wantCalls: []string{
				"create pod check-0b7c3a3e1d2f-precondition-0",
				"create configmap check-0b7c3a3e1d2f-precondition-0",
			},
		},
		{
			name: "a stopped precondition runs no check",
			inc:  open(stoppedOn(v1alpha1.ScriptPrecondition)),
			want: reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			// Nothing to create: the precondition hit the no-verdict bound.
			wantState: v1alpha1.StateOpen,
		},
		{
			name: "the run that hits the no-verdict bound is persisted, and no next run starts",
			inc:  open(withLastChecks(v1alpha1.LastChecks{Precondition: streakOf(lc(exit(2), 4*time.Minute), 5, 4*time.Minute, 7*time.Minute)})),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				return []corev1.Pod{checkPod(inc, v1alpha1.ScriptPrecondition, now.Add(-3*time.Minute), exit(2), now.Add(-2*time.Minute))}
			},
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantState: v1alpha1.StateOpen,
			wantStale: 1,
			wantCalls: []string{"update status", "delete pod check-0b7c3a3e1d2f-precondition-0"},
		},
		{
			name: "a ChecksUnknown left from before a reanalysis is cleared and the new precondition runs",
			inc:  open(withChecksUnknown(v1alpha1.ReasonPreconditionNoVerdict)),
			want: reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			// The condition is a status change: written before the check starts.
			wantState: v1alpha1.StateOpen,
			wantCalls: []string{
				"update status",
				"create pod check-0b7c3a3e1d2f-precondition-0",
				"create configmap check-0b7c3a3e1d2f-precondition-0",
			},
		},
		{
			name:      "an incident without scripts runs no check",
			inc:       newIncident(v1alpha1.StateOpen, withoutHowToFix),
			want:      reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantState: v1alpha1.StateOpen,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p []corev1.Pod
			if tc.pods != nil {
				p = tc.pods(tc.inc)
			}
			k := newKube(p...)
			e := newExternal(k, now)
			e.cfg.NoVerdictRuns = 6
			if !tc.lastRun.IsZero() {
				e.runs.seen(tc.inc.UID, tc.lastRun)
			}
			obs, err := e.Observe(context.Background(), tc.inc)
			if err != nil {
				t.Fatal(err)
			}
			obs.Diff = ""
			if d := cmp.Diff(tc.want, obs); d != "" {
				t.Errorf("observation (-want +got):\n%s", d)
			}
			if tc.inc.Status.State != tc.wantState {
				t.Errorf("state = %s, want %s", tc.inc.Status.State, tc.wantState)
			}
			if n := len(e.plan.stale); n != tc.wantStale {
				t.Errorf("stale pods = %d, want %d", n, tc.wantStale)
			}
			if !obs.ResourceExists {
				t.Fatal("an Incident always exists")
			}
			if !obs.ResourceUpToDate {
				if err := e.Update(context.Background(), tc.inc); err != nil {
					t.Fatal(err)
				}
				if d := cmp.Diff(tc.wantCalls, k.calls); d != "" {
					t.Errorf("Update calls (-want +got):\n%s", d)
				}
			}
		})
	}
}

func TestObserveDeleting(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen, func(i *v1alpha1.Incident) { i.DeletionTimestamp = ptr.To(metav1.NewTime(t0)) })
	for _, tc := range []struct {
		name string
		k    *kube
		want bool
	}{
		{name: "check pods left", k: newKube(checkPod(inc, v1alpha1.ScriptPrecondition, t0, nil, time.Time{})), want: true},
		{name: "a ConfigMap left", k: func() *kube { k := newKube(); k.cms = []corev1.ConfigMap{{}}; return k }(), want: true},
		{name: "nothing left", k: newKube(), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs, err := newExternal(tc.k, t0).Observe(context.Background(), inc)
			if err != nil {
				t.Fatal(err)
			}
			if obs.ResourceExists != tc.want {
				t.Errorf("ResourceExists = %v, want %v", obs.ResourceExists, tc.want)
			}
		})
	}
}

// Every success path of Observe must set Ready (provider-runtime's Ready contract): after a failed
// Observe the runtime leaves Ready at NotObserved, and only an Observe replaces it. One case per
// return of Observe, so a new path without a case stands out in review.
func TestObserveSetsReady(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	deleted := func(i *v1alpha1.Incident) { i.DeletionTimestamp = ptr.To(metav1.NewTime(t0)) }
	cases := []struct {
		name    string
		inc     *v1alpha1.Incident
		pods    func(inc *v1alpha1.Incident) []corev1.Pod
		lastRun time.Time
		obs     reconciler.ExternalObservation // the return reached, Diff aside
		want    prv1.ConditionReason
	}{
		{
			name: "deleted, check pods left",
			inc:  newIncident(v1alpha1.StateOpen, deleted),
			pods: func(inc *v1alpha1.Incident) []corev1.Pod {
				return []corev1.Pod{checkPod(inc, v1alpha1.ScriptPrecondition, t0, nil, time.Time{})}
			},
			obs:  reconciler.ExternalObservation{ResourceExists: true},
			want: prv1.ReasonDeleting,
		},
		{
			name: "deleted, nothing left",
			inc:  newIncident(v1alpha1.StateOpen, deleted),
			obs:  reconciler.ExternalObservation{ResourceExists: false},
			want: prv1.ReasonDeleting,
		},
		{
			name: "out of date: a check is due",
			inc:  newIncident(v1alpha1.StateOpen),
			obs:  reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			want: prv1.ReasonAvailable,
		},
		{
			name:    "up to date",
			inc:     newIncident(v1alpha1.StateOpen),
			lastRun: now.Add(-30 * time.Second),
			obs:     reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			want:    prv1.ReasonAvailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p []corev1.Pod
			if tc.pods != nil {
				p = tc.pods(tc.inc)
			}
			e := newExternal(newKube(p...), now)
			if !tc.lastRun.IsZero() {
				e.runs.seen(tc.inc.UID, tc.lastRun)
			}
			obs, err := reconcilertest.AssertObserveSetsReady(t, context.Background(), e, tc.inc)
			if err != nil {
				t.Fatal(err)
			}
			obs.Diff = ""
			if d := cmp.Diff(tc.obs, obs); d != "" {
				t.Errorf("observation (-want +got):\n%s", d)
			}
			if got := tc.inc.GetCondition(prv1.TypeReady).Reason; got != tc.want {
				t.Errorf("Ready reason = %s, want %s", got, tc.want)
			}
		})
	}
}

// A failed read of the check pods means Observe could not tell: the error is not ReadyAssessed, so
// the runtime sets Ready to NotObserved.
func TestObserveCannotTellReadyWhenTheChecksCannotBeRead(t *testing.T) {
	k := newKube()
	k.MockList = func(context.Context, client.ObjectList, ...client.ListOption) error {
		return errors.New("apiserver unavailable")
	}
	_, err := newExternal(k, t0).Observe(context.Background(), newIncident(v1alpha1.StateOpen))
	if err == nil {
		t.Fatal("want an error")
	}
	if reconciler.IsReadyAssessed(err) {
		t.Errorf("a failed read must leave Ready to the runtime: %v", err)
	}
}

func TestUpdateStopsWhenTheStatusCannotBePersisted(t *testing.T) {
	now := t0.Add(time.Minute)
	inc := newIncident(v1alpha1.StateOpen, withSpec(true, false))
	k := newKube(checkPod(inc, v1alpha1.ScriptPrecondition, t0, exit(1), t0.Add(10*time.Second)))
	k.statusErr = errors.New("conflict")
	e := newExternal(k, now)
	if _, err := e.Observe(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if err := e.Update(context.Background(), inc); err == nil {
		t.Fatal("want an error")
	}
	if d := cmp.Diff([]string{"update status"}, k.calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
}

func TestUpdateStartsTheNextDueCheck(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	// A result recorded long ago whose pod is still there: delete it and start the next run.
	inc := newIncident(v1alpha1.StateOpen, withReproduced(metav1.ConditionTrue),
		withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)}))
	old := checkPod(newIncident(v1alpha1.StateOpen), v1alpha1.ScriptPrecondition, t0, exit(1), t0.Add(time.Minute))
	k := newKube(old)
	e := newExternal(k, now)
	if _, err := e.Observe(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if err := e.Update(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"delete pod check-0b7c3a3e1d2f-precondition-0",
		"create pod check-0b7c3a3e1d2f-precondition-1790344860",
		"create configmap check-0b7c3a3e1d2f-precondition-1790344860",
	}
	if d := cmp.Diff(want, k.calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
}

func TestStartCheck(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen)
	k := newKube()
	if err := newExternal(k, t0).startCheck(context.Background(), inc, v1alpha1.ScriptPrecondition); err != nil {
		t.Fatal(err)
	}
	if len(k.created) != 2 {
		t.Fatalf("created %d objects, want a pod and a ConfigMap", len(k.created))
	}
	pod, cm := k.created[0].(*corev1.Pod), k.created[1].(*corev1.ConfigMap)

	if pod.Namespace != checksNamespace || cm.Namespace != checksNamespace {
		t.Errorf("namespaces = %s, %s, want %s", pod.Namespace, cm.Namespace, checksNamespace)
	}
	if got := pod.Spec.ServiceAccountName; got != "incident-check" {
		t.Errorf("serviceAccountName = %s", got)
	}
	if got := *pod.Spec.ActiveDeadlineSeconds; got != 60 {
		t.Errorf("activeDeadlineSeconds = %d, want 60", got)
	}
	if got := pod.Labels[LabelComponent]; got != ComponentCheck {
		t.Errorf("component label = %q", got)
	}
	if got := pod.Annotations[annotationIncidentName]; got != inc.Name {
		t.Errorf("incident annotation = %q", got)
	}
	if d := cmp.Diff([]string{"bash", "/check/check.sh"}, pod.Spec.Containers[0].Command); d != "" {
		t.Errorf("command (-want +got):\n%s", d)
	}
	if got := cm.Data[scriptKey]; got != inc.Status.HowToFix.Precondition {
		t.Errorf("script = %q, want the precondition", got)
	}
	if o := cm.OwnerReferences; len(o) != 1 || o[0].UID != pod.UID || o[0].Kind != "Pod" {
		t.Errorf("ConfigMap owner = %+v, want the pod", o)
	}
}

func TestStartCheckAfterAPartialStart(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen)
	k := newKube(*pods.Pod(inc, v1alpha1.ScriptPrecondition, time.Time{}))
	if err := newExternal(k, t0).startCheck(context.Background(), inc, v1alpha1.ScriptPrecondition); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create pod check-0b7c3a3e1d2f-precondition-0",
		"get pod check-0b7c3a3e1d2f-precondition-0",
		"create configmap check-0b7c3a3e1d2f-precondition-0",
	}
	if d := cmp.Diff(want, k.calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
}

func TestDelete(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen)
	k := newKube(checkPod(inc, v1alpha1.ScriptPrecondition, t0, nil, time.Time{}))
	k.cms = []corev1.ConfigMap{*pods.ConfigMap(inc, &k.pods[0], v1alpha1.ScriptPrecondition)}
	if err := newExternal(k, t0).Delete(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete pod check-0b7c3a3e1d2f-precondition-0", "delete configmap check-0b7c3a3e1d2f-precondition-0"}
	if d := cmp.Diff(want, k.calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
}

func TestPodEnforcesTheSandbox(t *testing.T) {
	loose := CheckPods{Namespace: checksNamespace, Timeout: time.Minute, Template: corev1.PodSpec{
		ServiceAccountName: "incident-check",
		HostNetwork:        true,
		DNSPolicy:          corev1.DNSClusterFirst,
		DNSConfig:          &corev1.PodDNSConfig{Nameservers: []string{"10.96.0.10"}, Searches: []string{"svc.cluster.local"}},
		Containers: []corev1.Container{{
			Name: "anything", Image: "img",
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true), AllowPrivilegeEscalation: ptr.To(true)},
		}},
	}}
	p := loose.Pod(newIncident(v1alpha1.StateOpen), v1alpha1.ScriptPrecondition, time.Time{})
	sc := p.Spec.Containers[0].SecurityContext
	switch {
	case p.Spec.HostNetwork:
		t.Error("hostNetwork is not disabled")
	case *sc.Privileged || *sc.AllowPrivilegeEscalation:
		t.Error("privileges are not dropped")
	case !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || !*p.Spec.SecurityContext.RunAsNonRoot:
		t.Error("the root filesystem is writable or root is allowed")
	case len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL":
		t.Error("capabilities are not all dropped")
	case p.Spec.RestartPolicy != corev1.RestartPolicyNever:
		t.Error("the pod restarts")
	case p.Spec.Containers[0].Name != containerName:
		t.Error("the container is not named check")
	case p.Spec.DNSPolicy != corev1.DNSNone:
		t.Errorf("dnsPolicy = %s, want None", p.Spec.DNSPolicy)
	}
	if d := cmp.Diff(&corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}}, p.Spec.DNSConfig); d != "" {
		t.Errorf("dnsConfig (-want +got):\n%s", d)
	}
	if loose.Template.Containers[0].SecurityContext.Privileged == nil || !*loose.Template.Containers[0].SecurityContext.Privileged {
		t.Error("the template was modified")
	}
}

func TestResultOf(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen)
	now := t0.Add(time.Minute)
	deadline := func(p corev1.Pod) corev1.Pod {
		p.Status.Phase, p.Status.Reason = corev1.PodFailed, "DeadlineExceeded"
		return p
	}
	withMessage := func(p corev1.Pod, msg string) corev1.Pod {
		p.Status.ContainerStatuses[0].State.Terminated.Message = msg
		return p
	}
	cases := []struct {
		name       string
		pod        corev1.Pod
		now        time.Time
		wantDone   bool
		wantExit   *int32
		wantAt     time.Time
		wantOutput string
	}{
		{name: "running", pod: checkPod(inc, v1alpha1.ScriptPrecondition, t0, nil, time.Time{}), now: now},
		{name: "exit 0", pod: checkPod(inc, v1alpha1.ScriptPrecondition, t0, exit(0), t0.Add(5*time.Second)), now: now,
			wantDone: true, wantExit: exit(0), wantAt: t0.Add(5 * time.Second)},
		{name: "exit 1", pod: checkPod(inc, v1alpha1.ScriptPrecondition, t0, exit(1), t0.Add(5*time.Second)), now: now,
			wantDone: true, wantExit: exit(1), wantAt: t0.Add(5 * time.Second)},
		{name: "another exit", pod: checkPod(inc, v1alpha1.ScriptPrecondition, t0, exit(2), t0.Add(5*time.Second)), now: now,
			wantDone: true, wantExit: exit(2), wantAt: t0.Add(5 * time.Second)},
		{name: "another exit with a message", pod: withMessage(checkPod(inc, v1alpha1.ScriptPrecondition, t0, exit(2), t0.Add(5*time.Second)), "jq: error: Invalid escape"), now: now,
			wantDone: true, wantExit: exit(2), wantAt: t0.Add(5 * time.Second), wantOutput: "jq: error: Invalid escape"},
		{name: "killed at the deadline", pod: deadline(checkPod(inc, v1alpha1.ScriptPrecondition, t0, exit(137), t0.Add(time.Minute))), now: now,
			wantDone: true, wantAt: t0.Add(time.Minute), wantOutput: "the check was stopped at its 1m0s deadline"},
		{name: "failed before running", pod: func() corev1.Pod {
			p := checkPod(inc, v1alpha1.ScriptPrecondition, t0, nil, time.Time{})
			p.Status.Phase = corev1.PodFailed
			return p
		}(), now: now, wantDone: true, wantAt: t0.Add(time.Minute)},
		{name: "pending inside its deadline", pod: func() corev1.Pod {
			p := checkPod(inc, v1alpha1.ScriptPrecondition, t0, nil, time.Time{})
			p.Status.Phase, p.Status.StartTime = corev1.PodPending, nil
			return p
		}(), now: t0.Add(time.Minute + 59*time.Second)},
		{name: "pending past its deadline", pod: func() corev1.Pod {
			p := checkPod(inc, v1alpha1.ScriptPrecondition, t0, nil, time.Time{})
			p.Status.Phase, p.Status.StartTime = corev1.PodPending, nil
			return p
		}(), now: t0.Add(2 * time.Minute), wantDone: true, wantAt: t0.Add(2 * time.Minute),
			wantOutput: "the check did not finish within 2m0s of its pod's creation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, done := resultOf(&tc.pod, time.Minute, tc.now)
			if done != tc.wantDone {
				t.Fatalf("done = %v, want %v", done, tc.wantDone)
			}
			if !done {
				return
			}
			if d := cmp.Diff(tc.wantExit, r.exit); d != "" {
				t.Errorf("exit (-want +got):\n%s", d)
			}
			if !r.at.Equal(tc.wantAt) {
				t.Errorf("at = %v, want %v", r.at, tc.wantAt)
			}
			if r.script != v1alpha1.ScriptPrecondition {
				t.Errorf("script = %s", r.script)
			}
			if r.output != tc.wantOutput {
				t.Errorf("output = %q, want %q", r.output, tc.wantOutput)
			}
		})
	}
}

func TestLoadPodTemplate(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "valid", body: "serviceAccountName: incident-check\ncontainers:\n  - name: check\n    image: img:1\n"},
		{name: "no service account", body: "containers:\n  - name: check\n    image: img:1\n", wantErr: true},
		{name: "two containers", body: "serviceAccountName: a\ncontainers:\n  - name: a\n  - name: b\n", wantErr: true},
		{name: "a reserved volume", body: "serviceAccountName: a\ncontainers:\n  - name: a\nvolumes:\n  - name: script\n", wantErr: true},
		{name: "an unknown field", body: "serviceAccountName: a\ncontainers:\n  - name: a\nimage: x\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPodTemplate(write(tc.name, tc.body))
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPollIntervalHook(t *testing.T) {
	ranAgo := func(d time.Duration) time.Duration {
		runs := newLastRuns()
		inc := newIncident(v1alpha1.StateOpen)
		runs.seen(inc.UID, time.Now().Add(-d))
		return runs.pollIntervalHook(inc, time.Minute)
	}
	if got := newLastRuns().pollIntervalHook(newIncident(v1alpha1.StateAnalyzing), time.Minute); got != time.Minute {
		t.Errorf("no script: %v, want the poll interval", got)
	}
	if got := newLastRuns().pollIntervalHook(newIncident(v1alpha1.StateOpen), time.Minute); got != time.Minute {
		t.Errorf("no run known: %v, want the poll interval", got)
	}
	if got := ranAgo(time.Hour); got != time.Minute {
		t.Errorf("a check due: %v, want the poll interval", got)
	}
	if got := ranAgo(40 * time.Second); got < 15*time.Second || got > 21*time.Second {
		t.Errorf("next check in ~20s: %v", got)
	}
}

func TestLastRunsForgetsADeletedIncident(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen, func(i *v1alpha1.Incident) { i.DeletionTimestamp = ptr.To(metav1.NewTime(t0)) })
	e := newExternal(newKube(), t0)
	e.runs.seen(inc.UID, t0)
	if _, err := e.Observe(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if got := e.runs.get(inc.UID); !got.IsZero() {
		t.Errorf("last run = %v, want none", got)
	}
}

// TestPodPassesRestrictedPodSecurity checks the pod the controller builds from the smallest
// template against the restricted Pod Security Standard the checks namespace enforces.
func TestPodPassesRestrictedPodSecurity(t *testing.T) {
	evaluator, err := policy.NewEvaluator(policy.DefaultChecks(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := pods.Pod(newIncident(v1alpha1.StateOpen), v1alpha1.ScriptPrecondition, time.Time{})
	lv := psaapi.LevelVersion{Level: psaapi.LevelRestricted, Version: psaapi.LatestVersion()}
	if r := policy.AggregateCheckResults(evaluator.EvaluatePod(lv, &p.ObjectMeta, &p.Spec)); !r.Allowed {
		t.Errorf("the check pod violates restricted: %s", r.ForbiddenDetail())
	}
}

// alertReader answers every Alert read with err.
type alertReader struct {
	client.Reader
	err   error
	reads int
}

func (a *alertReader) Get(_ context.Context, key client.ObjectKey, o client.Object, _ ...client.GetOption) error {
	a.reads++
	if gvk := o.GetObjectKind().GroupVersionKind(); gvk != AlertGroupVersionKind || key.Name != "cd-not-ready" || key.Namespace != "krateo-system" {
		return fmt.Errorf("unexpected read of %s %s", gvk, key)
	}
	return a.err
}

func TestObserveReadsAlert(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "observability.krateo.io", Resource: "alerts"}, "cd-not-ready")
	// The apiserver's answer for a resource type it does not serve names no object.
	noResource := apierrors.NewNotFound(schema.GroupResource{}, "")
	noKind := &apimeta.NoKindMatchError{GroupKind: AlertGroupVersionKind.GroupKind(), SearchedVersions: []string{"v1alpha1"}}

	cases := []struct {
		name          string
		inc           *v1alpha1.Incident
		err           error
		lastRead      time.Time
		wantReads     int
		wantAlertGone metav1.ConditionStatus
		wantState     v1alpha1.State
		wantWrite     bool
	}{
		{name: "found", inc: newIncident(v1alpha1.StateOpen), wantReads: 1,
			wantAlertGone: metav1.ConditionUnknown, wantState: v1alpha1.StateOpen},
		{name: "NotFound naming the Alert: AlertGone, written", inc: newIncident(v1alpha1.StateOpen), err: notFound, wantReads: 1,
			wantAlertGone: metav1.ConditionTrue, wantState: v1alpha1.StateOpen, wantWrite: true},
		{name: "NotFound for the grace: Closed", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(0), withAlertGone(0)), err: notFound, wantReads: 1,
			wantAlertGone: metav1.ConditionTrue, wantState: v1alpha1.StateClosed, wantWrite: true},
		{name: "NotFound for the resource type: unread", inc: newIncident(v1alpha1.StateOpen), err: noResource, wantReads: 1,
			wantAlertGone: metav1.ConditionUnknown, wantState: v1alpha1.StateOpen},
		{name: "no Alert CRD: unread", inc: newIncident(v1alpha1.StateOpen), err: noKind, wantReads: 1,
			wantAlertGone: metav1.ConditionUnknown, wantState: v1alpha1.StateOpen},
		{name: "forbidden: unread", inc: newIncident(v1alpha1.StateOpen), err: apierrors.NewForbidden(schema.GroupResource{Resource: "alerts"}, "cd-not-ready", errors.New("no")), wantReads: 1,
			wantAlertGone: metav1.ConditionUnknown, wantState: v1alpha1.StateOpen},
		{name: "read less than a poll interval ago: not read", inc: newIncident(v1alpha1.StateOpen), err: notFound, lastRead: now.Add(-30 * time.Second),
			wantAlertGone: metav1.ConditionUnknown, wantState: v1alpha1.StateOpen},
		{name: "Analyzing: not read", inc: newIncident(v1alpha1.StateAnalyzing), err: notFound,
			wantAlertGone: metav1.ConditionUnknown, wantState: v1alpha1.StateAnalyzing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := newKube()
			e := newExternal(k, now)
			e.cfg.AlertGoneGrace = 10 * time.Minute
			a := &alertReader{err: tc.err}
			e.alerts = a
			if !tc.lastRead.IsZero() {
				e.alertReads.seen(tc.inc.UID, tc.lastRead)
			}
			if _, err := e.Observe(context.Background(), tc.inc); err != nil {
				t.Fatal(err)
			}
			if a.reads != tc.wantReads {
				t.Errorf("Alert reads = %d, want %d", a.reads, tc.wantReads)
			}
			if s := tc.inc.GetCondition(v1alpha1.TypeAlertGone).Status; s != tc.wantAlertGone {
				t.Errorf("AlertGone = %s, want %s", s, tc.wantAlertGone)
			}
			if tc.inc.Status.State != tc.wantState {
				t.Errorf("state = %s, want %s", tc.inc.Status.State, tc.wantState)
			}
			if e.plan.dirty != tc.wantWrite {
				t.Errorf("status write = %v, want %v", e.plan.dirty, tc.wantWrite)
			}
		})
	}
}

func resolved(state v1alpha1.State, at time.Time) incOpt {
	return func(inc *v1alpha1.Incident) {
		inc.Status.State = state
		inc.Status.HowToFix = nil
		inc.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: metav1.NewTime(at)}
		if state == v1alpha1.StateResolved {
			inc.Status.Resolution.By = v1alpha1.ResolvedByVerify
		}
	}
}

// expired holds for a Closed or Resolved incident once resolution.at plus the retention has
// passed, and never for a live one, one with no resolution.at, or with retention off.
func TestExpired(t *testing.T) {
	at := t0
	ret := Config{Retention: 30 * 24 * time.Hour}
	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		cfg  Config
		now  time.Time
		want bool
	}{
		{"closed, retention passed", newIncident(v1alpha1.StateClosed, resolved(v1alpha1.StateClosed, at)), ret, at.Add(30 * 24 * time.Hour), true},
		{"resolved, retention passed", newIncident(v1alpha1.StateResolved, resolved(v1alpha1.StateResolved, at)), ret, at.Add(31 * 24 * time.Hour), true},
		{"closed, a second short", newIncident(v1alpha1.StateClosed, resolved(v1alpha1.StateClosed, at)), ret, at.Add(30*24*time.Hour - time.Second), false},
		{"retention off", newIncident(v1alpha1.StateClosed, resolved(v1alpha1.StateClosed, at)), Config{}, at.Add(365 * 24 * time.Hour), false},
		{"closed, no resolution.at", newIncident(v1alpha1.StateClosed), ret, at.Add(365 * 24 * time.Hour), false},
		{"closed, a zero resolution.at", newIncident(v1alpha1.StateClosed, func(i *v1alpha1.Incident) {
			i.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser}
		}), ret, at, false},
		{"open with an old resolution", newIncident(v1alpha1.StateOpen, func(i *v1alpha1.Incident) {
			i.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: metav1.NewTime(at)}
		}), ret, at.Add(365 * 24 * time.Hour), false},
		{"verifying", newIncident(v1alpha1.StateVerifying), ret, at.Add(365 * 24 * time.Hour), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expired(c.inc, c.cfg, c.now); got != c.want {
				t.Errorf("expired = %v, want %v", got, c.want)
			}
		})
	}
}

// A Closed incident past its retention is deleted by Update, last and pinned to its UID; one
// within it is left alone, and nothing is written for it.
func TestUpdateDeletesAnIncidentPastItsRetention(t *testing.T) {
	inc := newIncident(v1alpha1.StateClosed, resolved(v1alpha1.StateClosed, t0))
	k := newKube()
	var pinned *types.UID
	k.MockDelete = func(_ context.Context, o client.Object, opts ...client.DeleteOption) error {
		k.calls = append(k.calls, "delete "+kindOf(o)+" "+o.GetName())
		do := &client.DeleteOptions{}
		for _, op := range opts {
			op.ApplyToDelete(do)
		}
		if do.Preconditions != nil {
			pinned = do.Preconditions.UID
		}
		return nil
	}
	e := newExternal(k, t0.Add(31*24*time.Hour))
	e.cfg.Retention = 30 * 24 * time.Hour
	obs, err := e.Observe(context.Background(), inc)
	if err != nil {
		t.Fatal(err)
	}
	if obs.ResourceUpToDate {
		t.Fatal("an incident past its retention must reach Update")
	}
	if err := e.Update(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([]string{"delete incident " + inc.Name}, k.calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
	if pinned == nil || *pinned != inc.UID {
		t.Errorf("the delete is not pinned to the incident's UID: %v", pinned)
	}

	within := newIncident(v1alpha1.StateClosed, resolved(v1alpha1.StateClosed, t0))
	k2 := newKube()
	e2 := newExternal(k2, t0.Add(29*24*time.Hour))
	e2.cfg.Retention = 30 * 24 * time.Hour
	obs, err = e2.Observe(context.Background(), within)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.ResourceUpToDate {
		t.Errorf("within its retention, nothing to do: %+v", obs)
	}
}

// Closing an incident in this pass starts its retention now: it is not deleted in the same pass
// even if the retention is shorter than the time since it was created.
func TestClosingStartsTheRetention(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen, withSpec(false, false), func(i *v1alpha1.Incident) { i.Spec.Closed = true })
	k := newKube()
	e := newExternal(k, t0.Add(365*24*time.Hour))
	e.cfg.Retention = time.Hour
	if _, err := e.Observe(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if e.plan.expired {
		t.Fatal("an incident closed in this pass was marked expired")
	}
	if inc.Status.State != v1alpha1.StateClosed {
		t.Fatalf("state %s, want Closed", inc.Status.State)
	}
}

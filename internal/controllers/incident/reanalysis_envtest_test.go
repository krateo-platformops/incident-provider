//go:build integration

package incident

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// TestStaleReanalysisEnvtest runs the Incident reconciler against a real API server, with the
// writer (alert-provider) played by the test: when ApplyActionStale becomes True, the controller
// requests one reanalysis per episode.
func TestStaleReanalysisEnvtest(t *testing.T) {
	ctx, kube, _ := startEnvtest(t)

	autoReq := &v1alpha1.ReanalysisRequest{Cause: v1alpha1.ReanalysisCauseApplyActionStale, Feedback: v1alpha1.ReanalysisFeedbackApplyActionStale}
	personReq := &v1alpha1.ReanalysisRequest{Feedback: "the root cause is the chart, not the CRD"}

	auto := newEnvIncident(ctx, t, kube, "auto", nil)
	openWith(ctx, t, kube, auto, "v1")
	person := newEnvIncident(ctx, t, kube, "person", personReq)
	openWith(ctx, t, kube, person, "v1")

	// Incidents that already carry ApplyActionStale=True, in states that take no request.
	others := map[v1alpha1.State]*v1alpha1.Incident{}
	for _, state := range []v1alpha1.State{v1alpha1.StateAnalyzing, v1alpha1.StateResolved, v1alpha1.StateClosed} {
		inc := newEnvIncident(ctx, t, kube, "in-"+strings.ToLower(string(state)), nil)
		if state == v1alpha1.StateClosed {
			patchSpec(ctx, t, kube, inc, `{"spec":{"closed":true}}`)
		}
		writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) {
			st.State = state
			st.HowToFix = &v1alpha1.HowToFix{Precondition: "exit 1", ApplyAction: widgetAction("v0")}
			st.SetConditions(prv1.Condition{Type: v1alpha1.TypeApplyActionStale, Status: metav1.ConditionTrue,
				Reason: v1alpha1.ReasonVersionNotServed, Message: "applyAction targets " + widgetsGroup + "/v0 widgets, which this cluster does not serve; served: v1, v2",
				LastTransitionTime: metav1.NewTime(time.Now()).Rfc3339Copy()})
			switch state {
			case v1alpha1.StateResolved:
				st.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: metav1.NewTime(time.Now()).Rfc3339Copy()}
			case v1alpha1.StateClosed:
				st.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: metav1.NewTime(time.Now()).Rfc3339Copy()}
			}
		})
		others[state] = inc
	}

	isServed := func(c prv1.Condition) bool {
		return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonServed
	}
	isStale := func(c prv1.Condition) bool { return c.Status == metav1.ConditionTrue }
	waitFor(ctx, t, kube, auto, "False, Served", isServed)
	waitFor(ctx, t, kube, person, "False, Served", isServed)
	if r := get(ctx, t, kube, auto).Spec.Reanalysis; r != nil {
		t.Fatalf("a served target got a reanalysis request: %+v", r)
	}

	time.Sleep(1100 * time.Millisecond)
	setServed(ctx, t, kube, map[string]bool{"v1": false})
	episode := waitFor(ctx, t, kube, auto, "True", isStale)

	t.Run("the flip requests a reanalysis", func(t *testing.T) {
		eventuallyReanalysis(ctx, t, kube, auto, autoReq)
		eventually(t, "a ReanalysisRequested event", func() bool { return requestEvents(ctx, t, kube, auto) == 1 })
	})

	t.Run("a person's pending request wins", func(t *testing.T) {
		waitFor(ctx, t, kube, person, "True", isStale)
		holds(t, 3*poll, func() string {
			return cmp.Diff(&v1alpha1.ReanalysisRequest{Cause: v1alpha1.ReanalysisCausePerson, Feedback: personReq.Feedback}, get(ctx, t, kube, person).Spec.Reanalysis)
		})
		if n := requestEvents(ctx, t, kube, person); n != 0 {
			t.Errorf("%d ReanalysisRequested events, want none", n)
		}
	})

	t.Run("no request in Analyzing, Resolved or Closed", func(t *testing.T) {
		for state, inc := range others {
			if r := get(ctx, t, kube, inc).Spec.Reanalysis; r != nil {
				t.Errorf("%s: spec.reanalysis = %+v, want none", state, r)
			}
		}
	})

	t.Run("one request per episode: the writer answers, the new analysis is stale too", func(t *testing.T) {
		consume(ctx, t, kube, auto)
		// Analyzing: howToFix is gone, the condition stays, nothing is requested.
		holds(t, 3*poll, func() string {
			inc := get(ctx, t, kube, auto)
			if inc.Spec.Reanalysis != nil {
				return "spec.reanalysis set while Analyzing"
			}
			return cmp.Diff(episode, inc.GetCondition(v1alpha1.TypeApplyActionStale))
		})
		// The new analysis targets another version the cluster does not serve.
		openWith(ctx, t, kube, auto, "v0")
		c := waitFor(ctx, t, kube, auto, "True for v0", func(c prv1.Condition) bool {
			return c.Status == metav1.ConditionTrue && c.Message == "applyAction targets "+widgetsGroup+"/v0 widgets, which this cluster does not serve; served: v2"
		})
		if !c.LastTransitionTime.Equal(&episode.LastTransitionTime) {
			t.Errorf("lastTransitionTime = %s, want %s: the episode goes on", c.LastTransitionTime, episode.LastTransitionTime)
		}
		holds(t, 4*poll, func() string {
			if r := get(ctx, t, kube, auto).Spec.Reanalysis; r != nil {
				return "a second request in the same episode"
			}
			return ""
		})
		if n := requestEvents(ctx, t, kube, auto); n != 1 {
			t.Errorf("%d ReanalysisRequested events, want 1", n)
		}
	})

	t.Run("a new episode requests again", func(t *testing.T) {
		time.Sleep(1100 * time.Millisecond)
		addVersion(ctx, t, kube, "v0")
		waitFor(ctx, t, kube, auto, "False, Served", isServed)
		time.Sleep(1100 * time.Millisecond)
		setServed(ctx, t, kube, map[string]bool{"v0": false})
		c := waitFor(ctx, t, kube, auto, "True again", isStale)
		if !c.LastTransitionTime.After(episode.LastTransitionTime.Time) {
			t.Errorf("lastTransitionTime = %s, want after %s", c.LastTransitionTime, episode.LastTransitionTime)
		}
		eventuallyReanalysis(ctx, t, kube, auto, autoReq)
		eventually(t, "a second ReanalysisRequested event", func() bool { return requestEvents(ctx, t, kube, auto) == 2 })
	})
}

func widgetAction(version string) *v1alpha1.ApplyAction {
	return &v1alpha1.ApplyAction{Verb: "delete", APIVersion: widgetsGroup + "/" + version, Resource: "widgets", Namespace: "demo", Name: "w1"}
}

// newEnvIncident creates an incident as the writer does, with req as its spec.reanalysis.
func newEnvIncident(ctx context.Context, t *testing.T, kube client.Client, name string, req *v1alpha1.ReanalysisRequest) *v1alpha1.Incident {
	t.Helper()
	inc := &v1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		Spec: v1alpha1.IncidentSpec{
			AlertRef:    v1alpha1.AlertRef{Name: "widget-not-ready", Namespace: "demo"},
			Trigger:     v1alpha1.TriggerAlert,
			TriggeredAt: ptr.To(metav1.NewTime(time.Now()).Rfc3339Copy()),
			Reanalysis:  req.DeepCopy(),
		},
	}
	if err := kube.Create(ctx, inc); err != nil {
		t.Fatal(err)
	}
	return inc
}

// openWith writes an analysis whose applyAction targets widgets at version: the incident is Open.
func openWith(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident, version string) {
	t.Helper()
	writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) {
		st.State = v1alpha1.StateOpen
		st.HowToFix = &v1alpha1.HowToFix{Precondition: "exit 1", Apply: "true", Verify: "exit 0", ApplyAction: widgetAction(version)}
	})
}

// consume answers the incident's spec.reanalysis as alert-provider does: one status write (back to
// Analyzing, status.reanalysis recorded, the analysis cleared), then spec.reanalysis removed.
func consume(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident) {
	t.Helper()
	req := get(ctx, t, kube, inc).Spec.Reanalysis
	if req == nil {
		t.Fatal("no spec.reanalysis to consume")
	}
	writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) {
		count := int32(1)
		if st.Reanalysis != nil {
			count = st.Reanalysis.Count + 1
		}
		st.State = v1alpha1.StateAnalyzing
		st.Reanalysis = &v1alpha1.Reanalysis{At: metav1.NewTime(time.Now()), Cause: req.Cause, Feedback: req.Feedback, Count: count}
		st.HowToFix, st.LastChecks, st.VerifyingSince = nil, nil, nil
	})
	patchSpec(ctx, t, kube, inc, `{"spec":{"reanalysis":null}}`)
}

func patchSpec(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident, patch string) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return kube.Patch(ctx, get(ctx, t, kube, inc), client.RawPatch(types.MergePatchType, []byte(patch)))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func get(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident) *v1alpha1.Incident {
	t.Helper()
	var got v1alpha1.Incident
	if err := kube.Get(ctx, client.ObjectKeyFromObject(inc), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func eventuallyReanalysis(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident, want *v1alpha1.ReanalysisRequest) {
	t.Helper()
	var got *v1alpha1.ReanalysisRequest
	eventually(t, "spec.reanalysis", func() bool {
		got = get(ctx, t, kube, inc).Spec.Reanalysis
		return got != nil
	})
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("spec.reanalysis (-want +got):\n%s", d)
	}
}

// holds fails unless check returns "" for all of d.
func holds(t *testing.T, d time.Duration, check func() string) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if msg := check(); msg != "" {
			t.Fatal(msg)
		}
	}
}

// requestEvents counts the ReanalysisRequested events on inc.
func requestEvents(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident) int32 {
	t.Helper()
	var events corev1.EventList
	if err := kube.List(ctx, &events, client.InNamespace(inc.Namespace)); err != nil {
		t.Fatal(err)
	}
	var n int32
	for _, e := range events.Items {
		if e.InvolvedObject.Name == inc.Name && e.Reason == reasonReanalysisRequested {
			n += max(e.Count, 1)
		}
	}
	return n
}

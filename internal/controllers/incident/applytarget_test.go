package incident

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/test"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

func withApplyAction(apiVersion, resource string) incOpt {
	return func(inc *v1alpha1.Incident) {
		inc.Status.HowToFix.ApplyAction = &v1alpha1.ApplyAction{Verb: "patch", APIVersion: apiVersion, Resource: resource, Namespace: "demo", Name: "snowplow"}
	}
}

func withStale(s metav1.ConditionStatus, reason prv1.ConditionReason, msg string, d time.Duration) incOpt {
	return func(inc *v1alpha1.Incident) {
		inc.SetConditions(prv1.Condition{Type: v1alpha1.TypeApplyActionStale, Status: s, Reason: reason, Message: msg, LastTransitionTime: at(d)})
	}
}

func TestObserveApplyAction(t *testing.T) {
	now := t0.Add(time.Hour)
	const (
		gvSnowplow = "composition.krateo.io/v1-12-43 snowplows"
		staleMsg   = "applyAction targets " + gvSnowplow + ", which this cluster does not serve; served: v1-12-44, v1-12-49"
		servedMsg  = "applyAction targets " + gvSnowplow + ", which this cluster serves"
	)
	withTarget := withApplyAction("composition.krateo.io/v1-12-43", "snowplows")
	cases := []struct {
		name        string
		inc         *v1alpha1.Incident
		target      *applyTarget
		want        *prv1.Condition
		wantChanges []string
	}{
		{name: "not served at the version: True, VersionNotServed, with the served versions",
			inc: newIncident(v1alpha1.StateOpen, withTarget), target: &applyTarget{versions: []string{"v1-12-44", "v1-12-49"}},
			want:        &prv1.Condition{Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVersionNotServed, Message: staleMsg, LastTransitionTime: metav1.NewTime(now)},
			wantChanges: []string{"ApplyActionStale"}},
		{name: "served at no version: True, ResourceNotFound, served none",
			inc: newIncident(v1alpha1.StateOpen, withTarget), target: &applyTarget{},
			want: &prv1.Condition{Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonResourceNotFound,
				Message: "applyAction targets " + gvSnowplow + ", which this cluster does not serve; served: none", LastTransitionTime: metav1.NewTime(now)},
			wantChanges: []string{"ApplyActionStale"}},
		{name: "served: False, Served",
			inc: newIncident(v1alpha1.StateOpen, withTarget), target: &applyTarget{served: true},
			want: &prv1.Condition{Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonServed, Message: servedMsg, LastTransitionTime: metav1.NewTime(now)}},
		{name: "served again: False, a new transition time",
			inc: newIncident(v1alpha1.StateVerifying, withTarget, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0)), target: &applyTarget{served: true},
			want:        &prv1.Condition{Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonServed, Message: servedMsg, LastTransitionTime: metav1.NewTime(now)},
			wantChanges: []string{"ApplyActionServed"}},
		{name: "still not served, other versions now: the message changes, the transition time does not",
			inc: newIncident(v1alpha1.StateOpen, withTarget, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0)), target: &applyTarget{versions: []string{"v1-12-50"}},
			want: &prv1.Condition{Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVersionNotServed,
				Message: "applyAction targets " + gvSnowplow + ", which this cluster does not serve; served: v1-12-50", LastTransitionTime: at(0)}},
		{name: "still served: the transition time does not change",
			inc: newIncident(v1alpha1.StateOpen, withTarget, withStale(metav1.ConditionFalse, v1alpha1.ReasonServed, servedMsg, 0)), target: &applyTarget{served: true},
			want: &prv1.Condition{Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonServed, Message: servedMsg, LastTransitionTime: at(0)}},
		{name: "unread: the condition stays",
			inc:  newIncident(v1alpha1.StateOpen, withTarget, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0)),
			want: &prv1.Condition{Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVersionNotServed, Message: staleMsg, LastTransitionTime: at(0)}},
		{name: "unread, never read: no condition",
			inc: newIncident(v1alpha1.StateOpen, withTarget)},
		{name: "no applyAction: the condition is removed",
			inc:    newIncident(v1alpha1.StateOpen, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0)),
			target: &applyTarget{served: true}},
		{name: "Open without howToFix: the condition is removed",
			inc: newIncident(v1alpha1.StateOpen, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0), withoutHowToFix)},
		{name: "Analyzing, the reanalysis cleared howToFix: the condition stays, for the episode",
			inc:  newIncident(v1alpha1.StateAnalyzing, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0), withoutHowToFix),
			want: &prv1.Condition{Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVersionNotServed, Message: staleMsg, LastTransitionTime: at(0)}},
		{name: "Closed without howToFix: the condition stays",
			inc: newIncident(v1alpha1.StateClosed, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0), withoutHowToFix, func(i *v1alpha1.Incident) {
				i.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: at(0)}
			}),
			want: &prv1.Condition{Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVersionNotServed, Message: staleMsg, LastTransitionTime: at(0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before *v1alpha1.HowToFix
			if tc.inc.Status.HowToFix != nil {
				before = tc.inc.Status.HowToFix.DeepCopy()
			}
			changes := observeApplyAction(tc.inc, tc.target, now)
			var got *prv1.Condition
			for i, c := range tc.inc.Status.Conditions {
				if c.Type == v1alpha1.TypeApplyActionStale {
					got = &tc.inc.Status.Conditions[i]
				}
			}
			if tc.want != nil {
				tc.want.Type = v1alpha1.TypeApplyActionStale
			}
			if d := cmp.Diff(tc.want, got); d != "" {
				t.Errorf("ApplyActionStale (-want +got):\n%s", d)
			}
			var reasons []string
			for _, c := range changes {
				reasons = append(reasons, c.reason)
			}
			if d := cmp.Diff(tc.wantChanges, reasons); d != "" {
				t.Errorf("changes (-want +got):\n%s", d)
			}
			if d := cmp.Diff(before, tc.inc.Status.HowToFix); d != "" {
				t.Errorf("howToFix changed (-want +got):\n%s", d)
			}
		})
	}
}

// failingDiscovery is a fake discovery that fails the listing of one group version.
type failingDiscovery struct {
	*fake.FakeDiscovery
	failGV string
}

var errDiscovery = errors.New("the server is currently unable to handle the request")

func (d failingDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	if gv == d.failGV {
		return nil, errDiscovery
	}
	return d.FakeDiscovery.ServerResourcesForGroupVersion(gv)
}

// TestLookupApplyTarget reads a discovery without aggregated discovery, so it lists the group's
// versions one by one. The envtest reads a real API server's, aggregated.
func TestLookupApplyTarget(t *testing.T) {
	list := func(gv string, resources ...string) *metav1.APIResourceList {
		l := &metav1.APIResourceList{GroupVersion: gv}
		for _, r := range resources {
			l.APIResources = append(l.APIResources, metav1.APIResource{Name: r}, metav1.APIResource{Name: r + "/status"})
		}
		return l
	}
	disc := &fake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: []*metav1.APIResourceList{
		list("v1", "configmaps", "pods"),
		list("composition.krateo.io/v1-12-49", "snowplows"),
		list("composition.krateo.io/v1-12-44", "snowplows"),
		list("composition.krateo.io/v0-1-26", "otelcollectordaemonsets"),
	}}}

	cases := []struct {
		name               string
		failGV             string
		apiVersion, target string
		want               applyTarget
		wantErr            bool
	}{
		{name: "served", apiVersion: "composition.krateo.io/v1-12-49", target: "snowplows", want: applyTarget{served: true}},
		{name: "core group served", apiVersion: "v1", target: "configmaps", want: applyTarget{served: true}},
		{name: "a version not served: the versions that serve it, sorted", apiVersion: "composition.krateo.io/v1-12-43", target: "snowplows",
			want: applyTarget{versions: []string{"v1-12-44", "v1-12-49"}}},
		{name: "a served version without the resource", apiVersion: "composition.krateo.io/v0-1-26", target: "snowplows",
			want: applyTarget{versions: []string{"v1-12-44", "v1-12-49"}}},
		{name: "a resource at no version", apiVersion: "composition.krateo.io/v0-1-4", target: "krateoauditworms", want: applyTarget{}},
		{name: "a group not served", apiVersion: "example.org/v1", target: "widgets", want: applyTarget{}},
		{name: "an apiVersion that does not parse", apiVersion: "a/b/c", target: "snowplows", want: applyTarget{}},
		{name: "the target version cannot be listed: no answer", failGV: "composition.krateo.io/v1-12-43", apiVersion: "composition.krateo.io/v1-12-43", target: "snowplows", wantErr: true},
		{name: "another version cannot be listed: no answer", failGV: "composition.krateo.io/v1-12-44", apiVersion: "composition.krateo.io/v1-12-43", target: "snowplows", wantErr: true},
		{name: "another group cannot be listed: it does not matter", failGV: "v1", apiVersion: "composition.krateo.io/v1-12-43", target: "snowplows",
			want: applyTarget{versions: []string{"v1-12-44", "v1-12-49"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lookupApplyTarget(failingDiscovery{FakeDiscovery: disc, failGV: tc.failGV}, tc.apiVersion, tc.target)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %t", err, tc.wantErr)
			}
			if d := cmp.Diff(tc.want, got, cmp.AllowUnexported(applyTarget{})); d != "" {
				t.Errorf("target (-want +got):\n%s", d)
			}
		})
	}
}

func TestStaleReanalysisDue(t *testing.T) {
	stale := withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, "m", 10*time.Minute)
	consumed := func(d time.Duration) incOpt {
		return func(i *v1alpha1.Incident) {
			i.Status.Reanalysis = &v1alpha1.Reanalysis{At: at(d), Count: 1, Cause: v1alpha1.ReanalysisCausePerson}
		}
	}
	pending := func(i *v1alpha1.Incident) {
		i.Spec.Reanalysis = &v1alpha1.ReanalysisRequest{Cause: v1alpha1.ReanalysisCausePerson}
	}
	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		want bool
	}{
		{name: "Open, stale: due", inc: newIncident(v1alpha1.StateOpen, stale), want: true},
		{name: "Verifying, stale: due", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(0), stale), want: true},
		{name: "a reanalysis consumed before the episode: due", inc: newIncident(v1alpha1.StateOpen, stale, consumed(5*time.Minute)), want: true},
		{name: "a reanalysis consumed in the episode: answered", inc: newIncident(v1alpha1.StateOpen, stale, consumed(15*time.Minute))},
		{name: "a reanalysis consumed as the episode began: answered", inc: newIncident(v1alpha1.StateOpen, stale, consumed(10*time.Minute))},
		{name: "a request pending: it wins", inc: newIncident(v1alpha1.StateOpen, stale, pending)},
		{name: "served", inc: newIncident(v1alpha1.StateOpen, withStale(metav1.ConditionFalse, v1alpha1.ReasonServed, "m", 10*time.Minute))},
		{name: "no condition", inc: newIncident(v1alpha1.StateOpen)},
		{name: "Analyzing", inc: newIncident(v1alpha1.StateAnalyzing, stale)},
		{name: "Resolved", inc: newIncident(v1alpha1.StateResolved, stale)},
		{name: "Closed", inc: newIncident(v1alpha1.StateClosed, stale)},
		{name: "a person analyzes it: the writer would drop the request", inc: newIncident(v1alpha1.StateOpen, stale, func(i *v1alpha1.Incident) {
			i.Labels = map[string]string{v1alpha1.LabelAnalysis: v1alpha1.AnalysisHuman}
		})},
		{name: "another analysis label: due", inc: newIncident(v1alpha1.StateOpen, stale, func(i *v1alpha1.Incident) {
			i.Labels = map[string]string{v1alpha1.LabelAnalysis: "agent"}
		}), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleReanalysisDue(tc.inc); got != tc.want {
				t.Errorf("staleReanalysisDue = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestRequestReanalysis covers the write: a merge patch of spec.reanalysis alone, with the
// resourceVersion read, and on a conflict a fresh read and the guards checked again.
func TestRequestReanalysis(t *testing.T) {
	stale := withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, "applyAction targets x/v1 widgets, which this cluster does not serve; served: v2", 0)
	conflict := apierrors.NewConflict(schema.GroupResource{Group: "observability.krateo.io", Resource: "incidents"}, "x", errors.New("the object has been modified"))
	cases := []struct {
		name string
		// fresh is what a read after a conflict returns.
		fresh       func() *v1alpha1.Incident
		conflicts   int
		wantPatches []string
		wantErr     bool
	}{
		{name: "no conflict: one patch", wantPatches: []string{
			`{"metadata":{"resourceVersion":"7"},"spec":{"reanalysis":{"cause":"ApplyActionStale","feedback":"applyAction targets a version this cluster no longer serves"}}}`}},
		{name: "a conflict, still due: the patch again, with the new resourceVersion", conflicts: 1,
			fresh: func() *v1alpha1.Incident {
				i := newIncident(v1alpha1.StateOpen, withApplyAction("x/v1", "widgets"), stale)
				i.ResourceVersion = "8"
				return i
			},
			wantPatches: []string{
				`{"metadata":{"resourceVersion":"7"},"spec":{"reanalysis":{"cause":"ApplyActionStale","feedback":"applyAction targets a version this cluster no longer serves"}}}`,
				`{"metadata":{"resourceVersion":"8"},"spec":{"reanalysis":{"cause":"ApplyActionStale","feedback":"applyAction targets a version this cluster no longer serves"}}}`}},
		{name: "a conflict with a person's request: it wins, no second patch", conflicts: 1,
			fresh: func() *v1alpha1.Incident {
				i := newIncident(v1alpha1.StateOpen, withApplyAction("x/v1", "widgets"), stale)
				i.ResourceVersion = "8"
				i.Spec.Reanalysis = &v1alpha1.ReanalysisRequest{Cause: v1alpha1.ReanalysisCausePerson}
				return i
			},
			wantPatches: []string{
				`{"metadata":{"resourceVersion":"7"},"spec":{"reanalysis":{"cause":"ApplyActionStale","feedback":"applyAction targets a version this cluster no longer serves"}}}`}},
		{name: "conflicts every time: an error", conflicts: maxRequestAttempts, wantErr: true,
			fresh: func() *v1alpha1.Incident {
				return newIncident(v1alpha1.StateOpen, withApplyAction("x/v1", "widgets"), stale)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc := newIncident(v1alpha1.StateOpen, withApplyAction("x/v1", "widgets"), stale)
			inc.ResourceVersion = "7"
			var patches []string
			k := newKube()
			k.MockPatch = func(_ context.Context, _ client.Object, p client.Patch, _ ...client.PatchOption) error {
				if p.Type() != types.MergePatchType {
					t.Errorf("patch type %s, want %s", p.Type(), types.MergePatchType)
				}
				b, _ := p.Data(nil)
				patches = append(patches, string(b))
				if len(patches) <= tc.conflicts {
					return conflict
				}
				return nil
			}
			reader := test.NewMockClient()
			reader.MockGet = func(_ context.Context, _ client.ObjectKey, o client.Object) error {
				tc.fresh().DeepCopyInto(o.(*v1alpha1.Incident))
				return nil
			}
			e := newExternal(k, t0)
			e.reader = reader
			err := e.requestReanalysis(context.Background(), inc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %t", err, tc.wantErr)
			}
			if tc.wantPatches != nil {
				if d := cmp.Diff(tc.wantPatches, patches); d != "" {
					t.Errorf("patches (-want +got):\n%s", d)
				}
			}
		})
	}
}

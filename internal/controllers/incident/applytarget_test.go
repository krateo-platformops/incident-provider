package incident

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

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
		{name: "no howToFix: the condition is removed",
			inc: newIncident(v1alpha1.StateAnalyzing, withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, staleMsg, 0), withoutHowToFix)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.inc.Status.HowToFix.DeepCopy()
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

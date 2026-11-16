//go:build integration

package incident

import (
	"context"
	"strconv"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// envGizmoCRD is gizmoCRD as the API server takes it: gizmos at v1 and v2, both served, stored at
// v2, every version with gizmoSchema, which declares spec.replicas.
func envGizmoCRD() *apiextensionsv1.CustomResourceDefinition {
	crd := gizmoCRD("v2", map[string]bool{"v1": true, "v2": true})
	crd.Spec.Group = widgetsGroup
	crd.Spec.Scope = apiextensionsv1.NamespaceScoped
	crd.Spec.Names = apiextensionsv1.CustomResourceDefinitionNames{Plural: "gizmos", Singular: "gizmo", Kind: "Gizmo", ListKind: "GizmoList"}
	return crd
}

// setGizmoServed sets which versions of the gizmo CRD are served.
func setGizmoServed(ctx context.Context, t *testing.T, kube client.Client, served map[string]bool) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := kube.Get(ctx, client.ObjectKey{Name: "gizmos." + widgetsGroup}, &crd); err != nil {
			return err
		}
		for i, v := range crd.Spec.Versions {
			if s, ok := served[v.Name]; ok {
				crd.Spec.Versions[i].Served = s
			}
		}
		return kube.Update(ctx, &crd)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRetargetEnvtest: a version-only staleness whose fields the storage version declares is
// re-targeted by the controller, with no reanalysis; one whose script names the old version is not.
func TestRetargetEnvtest(t *testing.T) {
	ctx, kube, _ := startEnvtest(t)

	gizmo := func(name string, verify string) *v1alpha1.Incident {
		inc := newEnvIncident(ctx, t, kube, name, nil)
		writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) {
			st.State = v1alpha1.StateOpen
			st.HowToFix = &v1alpha1.HowToFix{Precondition: "exit 1", Apply: "true", Verify: verify,
				ApplyAction: &v1alpha1.ApplyAction{Verb: "patch", APIVersion: widgetsGroup + "/v1", Resource: "gizmos", Namespace: "demo", Name: "g1",
					Payload: &runtime.RawExtension{Raw: []byte(`{"spec":{"replicas":2}}`)}}}
		})
		return inc
	}
	moved := gizmo("gizmo-moves", "exit 0")
	pinned := gizmo("gizmo-script-names-v1", "kubectl get gizmos."+"v1."+widgetsGroup+" g1")
	isServed := func(c prv1.Condition) bool {
		return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonServed
	}
	waitFor(ctx, t, kube, moved, "False, Served", isServed)
	waitFor(ctx, t, kube, pinned, "False, Served", isServed)

	time.Sleep(1100 * time.Millisecond)
	setGizmoServed(ctx, t, kube, map[string]bool{"v1": false})

	t.Run("re-targeted to the storage version, no reanalysis", func(t *testing.T) {
		want := "applyAction re-targeted from " + widgetsGroup + "/v1 to " + widgetsGroup + "/v2 gizmos: " + widgetsGroup +
			"/v1 is not served, and every field it sets is declared in " + widgetsGroup + "/v2; review and apply it again"
		waitFor(ctx, t, kube, moved, "False, Retargeted", func(c prv1.Condition) bool {
			return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonRetargeted && c.Message == want
		})
		if got := get(ctx, t, kube, moved).Status.HowToFix.ApplyAction.APIVersion; got != widgetsGroup+"/v2" {
			t.Errorf("applyAction.apiVersion = %s, want %s/v2", got, widgetsGroup)
		}
		holds(t, 3*poll, func() string {
			inc := get(ctx, t, kube, moved)
			if inc.Spec.Reanalysis != nil {
				return "a re-targeted applyAction got a reanalysis request"
			}
			if c := inc.GetCondition(v1alpha1.TypeApplyActionStale); c.Reason != v1alpha1.ReasonRetargeted {
				return "the re-target record was replaced: " + string(c.Reason)
			}
			return ""
		})
		if n := eventsWithReason(ctx, t, kube, moved, reasonApplyActionRetargeted); n != 1 {
			t.Errorf("%d %s events, want 1", n, reasonApplyActionRetargeted)
		}
	})

	t.Run("a script names the old version: stale, a reanalysis, applyAction unchanged", func(t *testing.T) {
		waitFor(ctx, t, kube, pinned, "True, not re-targeted", func(c prv1.Condition) bool {
			return c.Status == metav1.ConditionTrue && c.Message == "applyAction targets "+widgetsGroup+"/v1 gizmos, which this cluster does not serve; served: v2"+
				"; not re-targeted: the verify script names gizmos.v1."+widgetsGroup
		})
		eventuallyReanalysis(ctx, t, kube, pinned, &v1alpha1.ReanalysisRequest{Cause: v1alpha1.ReanalysisCauseApplyActionStale, Feedback: v1alpha1.ReanalysisFeedbackApplyActionStale})
		if got := get(ctx, t, kube, pinned).Status.HowToFix.ApplyAction.APIVersion; got != widgetsGroup+"/v1" {
			t.Errorf("applyAction.apiVersion = %s, want unchanged", got)
		}
	})
}

// TestReanalysisBudgetEnvtest: the controller requests a reanalysis for StaleReanalysisBudget
// episodes of one incident, then sets ReanalysisBudgetExhausted and requests none.
func TestReanalysisBudgetEnvtest(t *testing.T) {
	ctx, kube, _ := startEnvtest(t)
	autoReq := &v1alpha1.ReanalysisRequest{Cause: v1alpha1.ReanalysisCauseApplyActionStale, Feedback: v1alpha1.ReanalysisFeedbackApplyActionStale}

	inc := newEnvIncident(ctx, t, kube, "budget", nil)
	openWith(ctx, t, kube, inc, "v1")
	isServed := func(c prv1.Condition) bool {
		return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonServed
	}
	isStale := func(c prv1.Condition) bool { return c.Status == metav1.ConditionTrue }

	for i := 1; i <= v1alpha1.StaleReanalysisBudget; i++ {
		waitFor(ctx, t, kube, inc, "False, Served", isServed)
		time.Sleep(1100 * time.Millisecond)
		setServed(ctx, t, kube, map[string]bool{"v1": false})
		waitFor(ctx, t, kube, inc, "True", isStale)
		eventuallyReanalysis(ctx, t, kube, inc, autoReq)
		if got := get(ctx, t, kube, inc).Annotations[v1alpha1.AnnotationStaleReanalyses]; got != strconv.Itoa(i) {
			t.Fatalf("episode %d: %s = %q, want %d", i, v1alpha1.AnnotationStaleReanalyses, got, i)
		}
		consume(ctx, t, kube, inc)
		openWith(ctx, t, kube, inc, "v1")
		time.Sleep(1100 * time.Millisecond)
		setServed(ctx, t, kube, map[string]bool{"v1": true})
	}

	waitFor(ctx, t, kube, inc, "False, Served", isServed)
	time.Sleep(1100 * time.Millisecond)
	setServed(ctx, t, kube, map[string]bool{"v1": false})
	waitFor(ctx, t, kube, inc, "True, budget spent", isStale)

	eventually(t, "ReanalysisBudgetExhausted True", func() bool {
		c := get(ctx, t, kube, inc).GetCondition(v1alpha1.TypeReanalysisBudgetExhausted)
		return c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonBudgetSpent
	})
	holds(t, 4*poll, func() string {
		if r := get(ctx, t, kube, inc).Spec.Reanalysis; r != nil {
			return "a request past the budget"
		}
		return ""
	})
	if n := requestEvents(ctx, t, kube, inc); n != int32(v1alpha1.StaleReanalysisBudget) {
		t.Errorf("%d ReanalysisRequested events, want %d", n, v1alpha1.StaleReanalysisBudget)
	}
	if n := eventsWithReason(ctx, t, kube, inc, string(v1alpha1.TypeReanalysisBudgetExhausted)); n != 1 {
		t.Errorf("%d %s events, want 1", n, v1alpha1.TypeReanalysisBudgetExhausted)
	}

	t.Run("a person can still ask", func(t *testing.T) {
		patchSpec(ctx, t, kube, inc, `{"spec":{"reanalysis":{"feedback":"look again"}}}`)
		holds(t, 2*poll, func() string {
			r := get(ctx, t, kube, inc).Spec.Reanalysis
			if r == nil || r.Feedback != "look again" {
				return "the person's request was changed or removed"
			}
			return ""
		})
	})
}

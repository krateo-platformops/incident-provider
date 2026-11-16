package incident

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// gizmoSchema declares spec.replicas (integer), spec.mode (string), spec.size (int-or-string),
// spec.labels (a map of strings), spec.ports (an array of objects with port), spec.enabled
// (boolean), and spec.extra, kept only by x-kubernetes-preserve-unknown-fields.
func gizmoSchema() *apiextensionsv1.JSONSchemaProps {
	str := apiextensionsv1.JSONSchemaProps{Type: "string"}
	return &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
		"spec": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"replicas": {Type: "integer"},
			"mode":     str,
			"size":     {XIntOrString: true},
			"enabled":  {Type: "boolean"},
			"labels":   {Type: "object", AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true, Schema: &str}},
			"ports": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"port": {Type: "integer"}}}}},
			"extra": {Type: "object", XPreserveUnknownFields: ptrTo(true)},
		}},
	}}
}

func ptrTo[T any](v T) *T { return &v }

// gizmoCRD serves gizmos.example.krateo.io at the versions in served, stores at storage, with
// gizmoSchema at every version.
func gizmoCRD(storage string, served map[string]bool) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "gizmos.example.krateo.io"}}
	for _, v := range []string{"v1", "v2", "v3"} {
		s, ok := served[v]
		if !ok {
			continue
		}
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name: v, Served: s, Storage: v == storage,
			Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: gizmoSchema()}})
	}
	return crd
}

func getterOf(crd *apiextensionsv1.CustomResourceDefinition, err error) crdGetter {
	return func(_ context.Context, name string) (*apiextensionsv1.CustomResourceDefinition, error) {
		if err != nil {
			return nil, err
		}
		if crd == nil || crd.Name != name {
			return nil, apierrors.NewNotFound(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, name)
		}
		return crd, nil
	}
}

func gizmoAction(verb, payload string) *v1alpha1.ApplyAction {
	a := &v1alpha1.ApplyAction{Verb: verb, APIVersion: "example.krateo.io/v1", Resource: "gizmos", Namespace: "demo", Name: "g1"}
	if payload != "" {
		a.Payload = &runtime.RawExtension{Raw: []byte(payload)}
	}
	return a
}

func TestPlanRetarget(t *testing.T) {
	v2 := gizmoCRD("v2", map[string]bool{"v1": false, "v2": true})
	served := []string{"v2"}
	cases := []struct {
		name    string
		crd     *apiextensionsv1.CustomResourceDefinition
		err     error
		action  *v1alpha1.ApplyAction
		served  []string
		wantTo  string
		wantWhy string
		wantErr bool
	}{
		{name: "patch of declared fields: to the storage version", crd: v2, action: gizmoAction("patch", `{"spec":{"replicas":2,"mode":"fast"}}`), wantTo: "example.krateo.io/v2"},
		{name: "a merge patch's removal of a declared field", crd: v2, action: gizmoAction("patch", `{"spec":{"mode":null}}`), wantTo: "example.krateo.io/v2"},
		{name: "metadata at the root is not in the schema, and fits", crd: v2, action: gizmoAction("patch", `{"metadata":{"labels":{"a":"b"}},"spec":{"replicas":1}}`), wantTo: "example.krateo.io/v2"},
		{name: "a map's keys are declared by additionalProperties", crd: v2, action: gizmoAction("patch", `{"spec":{"labels":{"anything":"x"}}}`), wantTo: "example.krateo.io/v2"},
		{name: "an array's items", crd: v2, action: gizmoAction("patch", `{"spec":{"ports":[{"port":80},{"port":443}]}}`), wantTo: "example.krateo.io/v2"},
		{name: "int-or-string takes a string", crd: v2, action: gizmoAction("patch", `{"spec":{"size":"10Gi"}}`), wantTo: "example.krateo.io/v2"},
		{name: "delete has no payload", crd: v2, action: gizmoAction("delete", ""), wantTo: "example.krateo.io/v2"},
		{name: "create with its own apiVersion", crd: v2, action: gizmoAction("create", `{"apiVersion":"example.krateo.io/v1","kind":"Gizmo","metadata":{"name":"g1"},"spec":{"replicas":1}}`), wantTo: "example.krateo.io/v2"},

		{name: "an undeclared field", crd: v2, action: gizmoAction("patch", `{"spec":{"replicaCount":2}}`), wantWhy: "spec.replicaCount is not declared in example.krateo.io/v2"},
		{name: "a field kept only by preserve-unknown-fields", crd: v2, action: gizmoAction("patch", `{"spec":{"extra":{"x":1}}}`), wantWhy: "spec.extra.x is not declared in example.krateo.io/v2"},
		{name: "a type that does not match", crd: v2, action: gizmoAction("patch", `{"spec":{"replicas":"two"}}`), wantWhy: "spec.replicas is a string, declared integer in example.krateo.io/v2"},
		{name: "a fraction for an integer", crd: v2, action: gizmoAction("patch", `{"spec":{"replicas":1.5}}`), wantWhy: "spec.replicas is the number 1.5, declared integer in example.krateo.io/v2"},
		{name: "a boolean for an int-or-string", crd: v2, action: gizmoAction("patch", `{"spec":{"size":true}}`), wantWhy: "spec.size is not an int-or-string in example.krateo.io/v2"},
		{name: "an item's undeclared field", crd: v2, action: gizmoAction("patch", `{"spec":{"ports":[{"port":80,"name":"http"}]}}`), wantWhy: "spec.ports[0].name is not declared in example.krateo.io/v2"},
		{name: "an object for a scalar", crd: v2, action: gizmoAction("patch", `{"spec":{"enabled":{"a":1}}}`), wantWhy: "spec.enabled is an object, declared boolean in example.krateo.io/v2"},
		{name: "create naming another apiVersion", crd: v2, action: gizmoAction("create", `{"apiVersion":"example.krateo.io/v3","kind":"Gizmo"}`), wantWhy: "the payload's apiVersion example.krateo.io/v3 is not the applyAction's in example.krateo.io/v2"},

		{name: "a core resource", action: &v1alpha1.ApplyAction{Verb: "patch", APIVersion: "v1", Resource: "configmaps", Name: "x"}, wantWhy: "the target is not a custom resource"},
		{name: "no CRD", action: gizmoAction("patch", `{"spec":{"replicas":2}}`), wantWhy: "no CustomResourceDefinition gizmos.example.krateo.io"},
		{name: "the storage version is not served", crd: gizmoCRD("v3", map[string]bool{"v1": false, "v2": true, "v3": false}),
			action: gizmoAction("patch", `{"spec":{"replicas":2}}`), wantWhy: "the storage version v3 of gizmos.example.krateo.io is not served"},
		{name: "discovery does not list the storage version yet", crd: v2, served: []string{"v3"},
			action: gizmoAction("patch", `{"spec":{"replicas":2}}`), wantWhy: "the storage version v2 of gizmos.example.krateo.io is not served"},
		{name: "the old, unserved version is the storage version", crd: gizmoCRD("v1", map[string]bool{"v1": false, "v2": true}),
			action: gizmoAction("patch", `{"spec":{"replicas":2}}`), wantWhy: "the storage version v1 of gizmos.example.krateo.io is not served"},
		{name: "no schema", crd: func() *apiextensionsv1.CustomResourceDefinition {
			c := gizmoCRD("v2", map[string]bool{"v1": false, "v2": true})
			c.Spec.Versions[1].Schema = nil
			return c
		}(), action: gizmoAction("patch", `{"spec":{"replicas":2}}`), wantWhy: "v2 of gizmos.example.krateo.io has no schema"},
		{name: "the CRD cannot be read: an error, nothing decided", err: errors.New("connection refused"),
			action: gizmoAction("patch", `{"spec":{"replicas":2}}`), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.served
			if s == nil {
				s = served
			}
			got, err := planRetarget(context.Background(), getterOf(tc.crd, tc.err), tc.action, s)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %t", err, tc.wantErr)
			}
			if got.to != tc.wantTo || got.why != tc.wantWhy {
				t.Errorf("planRetarget = {to: %q, why: %q}, want {to: %q, why: %q}", got.to, got.why, tc.wantTo, tc.wantWhy)
			}
		})
	}
}

func TestScriptsNaming(t *testing.T) {
	cases := []struct {
		name string
		fix  v1alpha1.HowToFix
		want string
	}{
		{name: "none", fix: v1alpha1.HowToFix{Apply: "kubectl patch gizmos g1 --type merge -p '{}'"}},
		{name: "the apiVersion in a heredoc", fix: v1alpha1.HowToFix{Apply: "kubectl apply -f - <<EOF\napiVersion: example.krateo.io/v1\nEOF"},
			want: "the apply script names example.krateo.io/v1"},
		{name: "kubectl's resource.version.group", fix: v1alpha1.HowToFix{Verify: "kubectl get gizmos.v1.example.krateo.io g1"},
			want: "the verify script names gizmos.v1.example.krateo.io"},
		{name: "the rollback counts too", fix: v1alpha1.HowToFix{Rollback: "kubectl get gizmos.v1.example.krateo.io"},
			want: "the rollback script names gizmos.v1.example.krateo.io"},
		{name: "another version does not", fix: v1alpha1.HowToFix{Apply: "kubectl get gizmos.v2.example.krateo.io"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scriptsNaming(&tc.fix, "example.krateo.io/v1", "gizmos"); got != tc.want {
				t.Errorf("scriptsNaming = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestObserveApplyActionRetarget(t *testing.T) {
	now := t0.Add(time.Hour)
	const retargetedMsg = "applyAction re-targeted from example.krateo.io/v1 to example.krateo.io/v2 gizmos: example.krateo.io/v1 is not served, " +
		"and every field it sets is declared in example.krateo.io/v2; review and apply it again"
	withGizmo := func(verb, payload string) incOpt {
		return func(i *v1alpha1.Incident) { i.Status.HowToFix.ApplyAction = gizmoAction(verb, payload) }
	}
	provable := &applyTarget{versions: []string{"v2"}, retarget: &retarget{to: "example.krateo.io/v2"}}

	t.Run("provable: applyAction moves, False Retargeted, one change", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen, withGizmo("patch", `{"spec":{"replicas":2}}`))
		changes := observeApplyAction(inc, provable, now)
		if got := applyActionOf(inc).APIVersion; got != "example.krateo.io/v2" {
			t.Errorf("apiVersion = %s, want example.krateo.io/v2", got)
		}
		want := prv1.Condition{Type: v1alpha1.TypeApplyActionStale, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonRetargeted,
			Message: retargetedMsg, LastTransitionTime: metav1.NewTime(now)}
		if d := cmp.Diff(want, inc.GetCondition(v1alpha1.TypeApplyActionStale)); d != "" {
			t.Errorf("condition (-want +got):\n%s", d)
		}
		if len(changes) != 1 || changes[0].reason != reasonApplyActionRetargeted {
			t.Errorf("changes = %+v, want one %s", changes, reasonApplyActionRetargeted)
		}
		if staleReanalysisDue(inc) {
			t.Error("a re-targeted applyAction is due a reanalysis")
		}

		t.Run("read again, served: the record stays, no change", func(t *testing.T) {
			before := inc.GetCondition(v1alpha1.TypeApplyActionStale)
			if changes := observeApplyAction(inc, &applyTarget{served: true}, now.Add(time.Minute)); len(changes) != 0 {
				t.Errorf("changes = %+v, want none", changes)
			}
			if d := cmp.Diff(before, inc.GetCondition(v1alpha1.TypeApplyActionStale)); d != "" {
				t.Errorf("condition (-want +got):\n%s", d)
			}
		})
		t.Run("a new analysis targets another served version: the plain served message", func(t *testing.T) {
			inc.Status.HowToFix.ApplyAction.APIVersion = "example.krateo.io/v3"
			observeApplyAction(inc, &applyTarget{served: true}, now.Add(2*time.Minute))
			c := inc.GetCondition(v1alpha1.TypeApplyActionStale)
			if c.Reason != v1alpha1.ReasonServed {
				t.Errorf("reason = %s, want Served", c.Reason)
			}
		})
	})

	t.Run("create: the payload's apiVersion moves too", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen, withGizmo("create", `{"apiVersion":"example.krateo.io/v1","kind":"Gizmo","spec":{"replicas":1}}`))
		observeApplyAction(inc, provable, now)
		if got := string(applyActionOf(inc).Payload.Raw); !strings.Contains(got, `"apiVersion":"example.krateo.io/v2"`) {
			t.Errorf("payload = %s, want apiVersion example.krateo.io/v2", got)
		}
	})

	t.Run("a stale episode becomes a re-target: False, a new transition time", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen, withGizmo("delete", ""),
			withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, "applyAction targets example.krateo.io/v1 gizmos, which this cluster does not serve; served: v2", 0))
		observeApplyAction(inc, provable, now)
		if c := inc.GetCondition(v1alpha1.TypeApplyActionStale); c.Status != metav1.ConditionFalse || !c.LastTransitionTime.Equal(ptrTo(metav1.NewTime(now))) {
			t.Errorf("condition = %+v, want False at now", c)
		}
	})

	t.Run("a script names the old version: True, not re-targeted, applyAction unchanged", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen, withGizmo("patch", `{"spec":{"replicas":2}}`), func(i *v1alpha1.Incident) {
			i.Status.HowToFix.Verify = "kubectl get gizmos.v1.example.krateo.io g1"
		})
		observeApplyAction(inc, provable, now)
		if got := applyActionOf(inc).APIVersion; got != "example.krateo.io/v1" {
			t.Errorf("apiVersion = %s, want unchanged", got)
		}
		c := inc.GetCondition(v1alpha1.TypeApplyActionStale)
		want := "applyAction targets example.krateo.io/v1 gizmos, which this cluster does not serve; served: v2; not re-targeted: the verify script names gizmos.v1.example.krateo.io"
		if c.Status != metav1.ConditionTrue || c.Message != want {
			t.Errorf("condition = %s %q, want True %q", c.Status, c.Message, want)
		}
		if !staleReanalysisDue(inc) {
			t.Error("not due a reanalysis")
		}
	})

	t.Run("not provable: True, the reason in the message", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen, withGizmo("patch", `{"spec":{"replicaCount":2}}`))
		observeApplyAction(inc, &applyTarget{versions: []string{"v2"}, retarget: &retarget{why: "spec.replicaCount is not declared in example.krateo.io/v2"}}, now)
		c := inc.GetCondition(v1alpha1.TypeApplyActionStale)
		if !strings.HasSuffix(c.Message, "; served: v2; not re-targeted: spec.replicaCount is not declared in example.krateo.io/v2") || c.Status != metav1.ConditionTrue {
			t.Errorf("condition = %s %q", c.Status, c.Message)
		}
	})
}

func hasCondition(inc *v1alpha1.Incident, ct prv1.ConditionType) bool {
	for _, c := range inc.Status.Conditions {
		if c.Type == ct {
			return true
		}
	}
	return false
}

// counted sets the controller's count of its stale reanalysis requests.
func counted(v string) incOpt {
	return func(i *v1alpha1.Incident) {
		if i.Annotations == nil {
			i.Annotations = map[string]string{}
		}
		i.Annotations[v1alpha1.AnnotationStaleReanalyses] = v
	}
}

func TestStaleReanalysisBudget(t *testing.T) {
	stale := withStale(metav1.ConditionTrue, v1alpha1.ReasonVersionNotServed, "m", 10*time.Minute)
	earlier := recorded(2*time.Minute, 0)
	cases := []struct {
		name        string
		inc         *v1alpha1.Incident
		wantDue     bool
		wantSpent   bool
		wantMessage string
	}{
		{name: "no count, no record: 0, due", inc: newIncident(v1alpha1.StateOpen, stale), wantDue: true},
		{name: "a record from before the count: 1, due", inc: newIncident(v1alpha1.StateOpen, stale, earlier), wantDue: true},
		{name: "2 requested: due", inc: newIncident(v1alpha1.StateOpen, stale, earlier, counted("2")), wantDue: true},
		{name: "3 requested: spent", inc: newIncident(v1alpha1.StateOpen, stale, earlier, counted("3")), wantSpent: true,
			wantMessage: "the controller has requested 3 reanalyses for this incident's stale applyActions, its budget of 3; " +
				"a person must re-run the analysis (the portal's Re-run analysis, spec.reanalysis)"},
		{name: "a count that does not parse: spent", inc: newIncident(v1alpha1.StateOpen, stale, earlier, counted("many")), wantSpent: true,
			wantMessage: "the controller's count of its reanalysis requests (annotation observability.krateo.io/stale-reanalyses) does not parse, so it requests none; " +
				"a person must re-run the analysis (the portal's Re-run analysis, spec.reanalysis)"},
		{name: "spent, but this episode was asked for: no condition", inc: newIncident(v1alpha1.StateOpen, stale, recorded(10*time.Minute, 0), counted("3"))},
		{name: "spent, served: no condition", inc: newIncident(v1alpha1.StateOpen, withStale(metav1.ConditionFalse, v1alpha1.ReasonServed, "m", 0), counted("3"))},
		{name: "spent, a person analyzes it: no condition", inc: newIncident(v1alpha1.StateOpen, stale, counted("3"), func(i *v1alpha1.Incident) {
			i.Labels = map[string]string{v1alpha1.LabelAnalysis: v1alpha1.AnalysisHuman}
		})},
	}
	now := t0.Add(time.Hour)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleReanalysisDue(tc.inc); got != tc.wantDue {
				t.Errorf("staleReanalysisDue = %t, want %t", got, tc.wantDue)
			}
			changes := observeBudget(tc.inc, now)
			c := tc.inc.GetCondition(v1alpha1.TypeReanalysisBudgetExhausted)
			if !tc.wantSpent {
				if hasCondition(tc.inc, v1alpha1.TypeReanalysisBudgetExhausted) || len(changes) != 0 {
					t.Errorf("condition %s, changes %+v; want none", c.Status, changes)
				}
				return
			}
			want := prv1.Condition{Type: v1alpha1.TypeReanalysisBudgetExhausted, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonBudgetSpent,
				Message: tc.wantMessage, LastTransitionTime: metav1.NewTime(now)}
			if d := cmp.Diff(want, c); d != "" {
				t.Errorf("condition (-want +got):\n%s", d)
			}
			if len(changes) != 1 {
				t.Errorf("changes = %+v, want one", changes)
			}
			if again := observeBudget(tc.inc, now.Add(time.Minute)); len(again) != 0 {
				t.Errorf("a second observation announced %+v, want nothing", again)
			}
			if got := tc.inc.GetCondition(v1alpha1.TypeReanalysisBudgetExhausted).LastTransitionTime; !got.Equal(ptrTo(metav1.NewTime(now))) {
				t.Errorf("lastTransitionTime moved to %s", got)
			}
		})
	}

	t.Run("served again: the condition is removed", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen, stale, earlier, counted("3"))
		observeBudget(inc, now)
		inc.SetConditions(prv1.Condition{Type: v1alpha1.TypeApplyActionStale, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonServed, LastTransitionTime: at(0)})
		observeBudget(inc, now)
		if hasCondition(inc, v1alpha1.TypeReanalysisBudgetExhausted) {
			t.Error("condition kept, want removed")
		}
	})
	t.Run("Analyzing: left as it is", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen, stale, earlier, counted("3"))
		observeBudget(inc, now)
		inc.Status.State = v1alpha1.StateAnalyzing
		observeBudget(inc, now)
		if c := inc.GetCondition(v1alpha1.TypeReanalysisBudgetExhausted); c.Status != metav1.ConditionTrue {
			t.Errorf("condition %s, want True kept", c.Status)
		}
	})
}

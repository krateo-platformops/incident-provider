package incident

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

const (
	writerManager = "alert-provider"
	personManager = "kubectl-edit"
)

// Field sets as the apiserver writes them in managedFields.
const (
	fieldsClosed        = `{"f:spec":{"f:closed":{}}}`
	fieldsReason        = `{"f:metadata":{"f:annotations":{"f:observability.krateo.io/closed-reason":{}}}}`
	fieldsClosedReason  = `{"f:metadata":{"f:annotations":{".":{},"f:observability.krateo.io/closed-reason":{}}},"f:spec":{"f:closed":{}}}`
	fieldsStatusOfState = `{"f:status":{"f:state":{}}}`
)

func entry(manager, subresource, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "observability.krateo.io/v1alpha1",
		FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(fields)}, Subresource: subresource}
}

// closedWith closes the incident in spec, with the closed-reason annotation reason and the
// managedFields entries mf.
func closedWith(reason string, mf ...metav1.ManagedFieldsEntry) incOpt {
	return func(inc *v1alpha1.Incident) {
		inc.Spec.Closed = true
		if reason != "" {
			inc.Annotations = map[string]string{v1alpha1.AnnotationClosedReason: reason}
		}
		inc.ManagedFields = mf
	}
}

func TestClosedBy(t *testing.T) {
	failed := v1alpha1.ClosedReasonAnalysisFailed
	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		want v1alpha1.ResolvedBy
	}{
		{name: "the writer closed a failed analysis in one patch: analysis-failed",
			inc: newIncident(v1alpha1.StateAnalyzing, closedWith(failed, entry(writerManager, "", fieldsClosedReason))), want: v1alpha1.ResolvedByAnalysisFailed},
		{name: "the writer annotated, then closed: analysis-failed",
			inc: newIncident(v1alpha1.StateOpen, closedWith(failed, entry(writerManager, "", fieldsReason),
				metav1.ManagedFieldsEntry{Manager: writerManager, Operation: metav1.ManagedFieldsOperationApply, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(fieldsClosed)}})),
			want: v1alpha1.ResolvedByAnalysisFailed},
		{name: "a person closed, no annotation: user",
			inc: newIncident(v1alpha1.StateOpen, closedWith("", entry(personManager, "", fieldsClosed))), want: v1alpha1.ResolvedByUser},
		{name: "a person closed an incident the writer had annotated: user",
			inc:  newIncident(v1alpha1.StateOpen, closedWith(failed, entry(writerManager, "", fieldsReason), entry(personManager, "", fieldsClosed))),
			want: v1alpha1.ResolvedByUser},
		{name: "spec.closed co-owned by the writer and a person: user",
			inc:  newIncident(v1alpha1.StateOpen, closedWith(failed, entry(writerManager, "", fieldsClosedReason), entry(personManager, "", fieldsClosed))),
			want: v1alpha1.ResolvedByUser},
		{name: "another closed reason: user",
			inc: newIncident(v1alpha1.StateOpen, closedWith("superseded", entry(writerManager, "", fieldsClosedReason))), want: v1alpha1.ResolvedByUser},
		{name: "no managedFields: user",
			inc: newIncident(v1alpha1.StateOpen, closedWith(failed)), want: v1alpha1.ResolvedByUser},
		{name: "a status entry owns no spec field: user",
			inc:  newIncident(v1alpha1.StateOpen, closedWith(failed, entry(writerManager, "", fieldsReason), entry(writerManager, "status", fieldsStatusOfState))),
			want: v1alpha1.ResolvedByUser},
		{name: "unreadable fields own nothing: user",
			inc: newIncident(v1alpha1.StateOpen, closedWith(failed, entry(writerManager, "", `{"f:spec":`))), want: v1alpha1.ResolvedByUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := closedBy(tc.inc); got != tc.want {
				t.Errorf("closedBy = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClosedByAfterRealWrites runs the writes through the fake client's field manager, the
// apiserver's own managedFields code, so the ownership closedBy reads is what the apiserver
// records: a merge patch owns the fields it changes, and a patch of an unchanged value takes
// nothing.
func TestClosedByAfterRealWrites(t *testing.T) {
	failedPatch := `{"metadata":{"annotations":{"observability.krateo.io/closed-reason":"analysis-failed"}},"spec":{"closed":true}}`
	closePatch := `{"spec":{"closed":true}}`
	reasonPatch := `{"metadata":{"annotations":{"observability.krateo.io/closed-reason":"analysis-failed"}}}`
	type write struct{ manager, patch string }
	cases := []struct {
		name   string
		writes []write
		want   v1alpha1.ResolvedBy
	}{
		{name: "the writer closes a failed analysis: analysis-failed", writes: []write{{writerManager, failedPatch}}, want: v1alpha1.ResolvedByAnalysisFailed},
		{name: "a person closes: user", writes: []write{{personManager, closePatch}}, want: v1alpha1.ResolvedByUser},
		{name: "a person closes first, then the writer's close lands: user",
			writes: []write{{personManager, closePatch}, {writerManager, failedPatch}}, want: v1alpha1.ResolvedByUser},
		{name: "the writer annotates, then a person closes: user",
			writes: []write{{writerManager, reasonPatch}, {personManager, closePatch}}, want: v1alpha1.ResolvedByUser},
		{name: "a person closes with the annotation in the same write: analysis-failed (documented)",
			writes: []write{{personManager, failedPatch}}, want: v1alpha1.ResolvedByAnalysisFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := runtime.NewScheme()
			if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
				t.Fatal(err)
			}
			kube := fake.NewClientBuilder().WithScheme(s).WithReturnManagedFields().WithStatusSubresource(&v1alpha1.Incident{}).Build()
			inc := newIncident("")
			inc.UID = ""
			if err := kube.Create(ctx, inc, client.FieldOwner(writerManager)); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.writes {
				if err := kube.Patch(ctx, inc, client.RawPatch(types.MergePatchType, []byte(w.patch)), client.FieldOwner(w.manager)); err != nil {
					t.Fatal(err)
				}
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(inc), inc); err != nil {
				t.Fatal(err)
			}
			if got := closedBy(inc); got != tc.want {
				t.Errorf("closedBy = %q, want %q; managedFields:\n%s", got, tc.want, describe(inc.ManagedFields))
			}
			applySpec(inc, t0.Add(time.Minute))
			if r := inc.Status.Resolution; inc.Status.State != v1alpha1.StateClosed || r == nil || r.By != tc.want {
				t.Errorf("after applySpec: state %q, resolution %+v, want Closed by %q", inc.Status.State, r, tc.want)
			}
		})
	}
}

func describe(mf []metav1.ManagedFieldsEntry) string {
	out := ""
	for _, e := range mf {
		raw := ""
		if e.FieldsV1 != nil {
			raw = string(e.FieldsV1.Raw)
		}
		out += e.Manager + " " + string(e.Operation) + " " + e.Subresource + " " + raw + "\n"
	}
	return out
}

package incident

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

func closedFixture(t *testing.T, resolvedAgo time.Duration, deleting bool) (*closedReconciler, client.Client, *v1alpha1.Incident, time.Time) {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.SchemeBuilder.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	inc := &v1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: "closed-1", Namespace: "krateo-system", UID: "u-1", Finalizers: []string{reconciler.FinalizerName}},
		Spec:       v1alpha1.IncidentSpec{AlertRef: v1alpha1.AlertRef{Name: "a", Namespace: "krateo-system"}, Closed: true},
		Status: v1alpha1.IncidentStatus{State: v1alpha1.StateClosed,
			Resolution: &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: metav1.NewTime(now.Add(-resolvedAgo))}},
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "check-cm", Namespace: "krateo-checks", Labels: map[string]string{labelIncidentUID: "u-1"}}}
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other-cm", Namespace: "krateo-checks", Labels: map[string]string{labelIncidentUID: "u-2"}}}
	kube := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.Incident{}).WithObjects(inc, cm, other).Build()
	if deleting {
		if err := kube.Delete(context.Background(), inc); err != nil {
			t.Fatal(err)
		}
	}
	r := &closedReconciler{cache: kube, kube: kube, reader: kube, pods: "krateo-checks",
		cfg: Config{Retention: 30 * 24 * time.Hour}, log: logging.NewNopLogger(), now: func() time.Time { return now }}
	return r, kube, inc, now
}

func reqOf(inc *v1alpha1.Incident) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: inc.Namespace, Name: inc.Name}}
}

func TestClosedRetention(t *testing.T) {
	t.Run("within retention: kept, requeued at most maxRetentionRequeue later", func(t *testing.T) {
		r, kube, inc, _ := closedFixture(t, 24*time.Hour, false)
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != maxRetentionRequeue {
			t.Errorf("RequeueAfter = %v, want %v (29 days remain)", res.RequeueAfter, maxRetentionRequeue)
		}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(inc), &v1alpha1.Incident{}); err != nil {
			t.Errorf("incident gone: %v", err)
		}
	})
	t.Run("close to its end: requeued for exactly then", func(t *testing.T) {
		r, _, inc, _ := closedFixture(t, 30*24*time.Hour-time.Hour, false)
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != time.Hour+time.Second {
			t.Errorf("RequeueAfter = %v, want 1h1s", res.RequeueAfter)
		}
	})
	t.Run("past retention: deleted (the fake has no finalizer handling: it is marked deleted)", func(t *testing.T) {
		r, kube, inc, _ := closedFixture(t, 31*24*time.Hour, false)
		if _, err := r.Reconcile(context.Background(), reqOf(inc)); err != nil {
			t.Fatal(err)
		}
		got := &v1alpha1.Incident{}
		err := kube.Get(context.Background(), client.ObjectKeyFromObject(inc), got)
		if err == nil && got.DeletionTimestamp.IsZero() {
			t.Error("incident past its retention neither deleted nor deleting")
		}
	})
	t.Run("retention 0: kept for ever, no requeue", func(t *testing.T) {
		r, _, inc, _ := closedFixture(t, 365*24*time.Hour, false)
		r.cfg.Retention = 0
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil || res.RequeueAfter != 0 {
			t.Errorf("res = %+v, err = %v; want nothing", res, err)
		}
	})
	t.Run("not Closed after all: left to the Incident controller", func(t *testing.T) {
		r, kube, inc, _ := closedFixture(t, 31*24*time.Hour, false)
		cur := &v1alpha1.Incident{}
		_ = kube.Get(context.Background(), client.ObjectKeyFromObject(inc), cur)
		cur.Status.State = v1alpha1.StateResolved
		if err := kube.Status().Update(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(context.Background(), reqOf(inc)); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(inc), &v1alpha1.Incident{}); err != nil {
			t.Errorf("a Resolved incident was deleted by the Closed controller: %v", err)
		}
	})
}

func TestClosedFinalize(t *testing.T) {
	r, kube, inc, _ := closedFixture(t, time.Hour, true)
	if _, err := r.Reconcile(context.Background(), reqOf(inc)); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(inc), &v1alpha1.Incident{}); !apierrors.IsNotFound(err) {
		t.Errorf("deleting incident still present after finalize: %v", err)
	}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "krateo-checks", Name: "check-cm"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("its check ConfigMap was kept: %v", err)
	}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "krateo-checks", Name: "other-cm"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("another incident's ConfigMap was deleted: %v", err)
	}
}

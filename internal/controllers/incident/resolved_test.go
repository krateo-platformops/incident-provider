package incident

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

func resolvedFixture(t *testing.T, state v1alpha1.State, resolvedAgo time.Duration, funcs *interceptor.Funcs) (*resolvedReconciler, client.Client, *v1alpha1.Incident) {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.SchemeBuilder.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 21, 17, 0, 0, 0, time.UTC)
	inc := &v1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: "resolved-1", Namespace: "krateo-system", UID: "u-1", Finalizers: []string{reconciler.FinalizerName}},
		Spec:       v1alpha1.IncidentSpec{AlertRef: v1alpha1.AlertRef{Name: "a", Namespace: "krateo-system"}},
		Status: v1alpha1.IncidentStatus{State: state,
			Resolution: &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: metav1.NewTime(now.Add(-resolvedAgo))}},
	}
	b := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.Incident{}).WithObjects(inc)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	kube := b.Build()
	r := &resolvedReconciler{cache: kube, kube: kube, reader: kube,
		cfg: Config{Retention: 30 * 24 * time.Hour}, log: logging.NewNopLogger(), now: func() time.Time { return now }}
	return r, kube, inc
}

func present(t *testing.T, kube client.Client, inc *v1alpha1.Incident) bool {
	t.Helper()
	got := &v1alpha1.Incident{}
	err := kube.Get(context.Background(), client.ObjectKeyFromObject(inc), got)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil && got.DeletionTimestamp.IsZero()
}

func TestResolvedRetention(t *testing.T) {
	t.Run("within retention: kept, requeued at most maxRetentionRequeue later", func(t *testing.T) {
		r, kube, inc := resolvedFixture(t, v1alpha1.StateResolved, 24*time.Hour, nil)
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != maxRetentionRequeue {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, maxRetentionRequeue)
		}
		if !present(t, kube, inc) {
			t.Error("an incident within its retention was deleted")
		}
	})
	t.Run("close to its end: requeued for exactly then", func(t *testing.T) {
		r, _, inc := resolvedFixture(t, v1alpha1.StateResolved, 30*24*time.Hour-time.Minute, nil)
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != time.Minute+time.Second {
			t.Errorf("RequeueAfter = %v, want 1m1s", res.RequeueAfter)
		}
	})
	t.Run("past retention: deleted, pinned to the UID and resourceVersion read", func(t *testing.T) {
		var pre *metav1.Preconditions
		funcs := &interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			do := &client.DeleteOptions{}
			for _, op := range opts {
				op.ApplyToDelete(do)
			}
			pre = do.Preconditions
			return c.Delete(ctx, o, opts...)
		}}
		r, kube, inc := resolvedFixture(t, v1alpha1.StateResolved, 31*24*time.Hour, funcs)
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil || res.RequeueAfter != 0 {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if present(t, kube, inc) {
			t.Error("an incident past its retention was kept")
		}
		if pre == nil || pre.UID == nil || *pre.UID != "u-1" || pre.ResourceVersion == nil || *pre.ResourceVersion == "" {
			t.Errorf("delete preconditions = %+v, want the UID and resourceVersion read", pre)
		}
	})
	t.Run("changed since the read: not deleted, read again shortly", func(t *testing.T) {
		funcs := &interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "observability.krateo.io", Resource: "incidents"}, "resolved-1", nil)
		}}
		r, kube, inc := resolvedFixture(t, v1alpha1.StateResolved, 31*24*time.Hour, funcs)
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil {
			t.Fatalf("a precondition conflict is not an error: %v", err)
		}
		if res.RequeueAfter != time.Second {
			t.Errorf("RequeueAfter = %v, want 1s", res.RequeueAfter)
		}
		if !present(t, kube, inc) {
			t.Error("deleted despite the conflict")
		}
	})
	t.Run("not Resolved: left to the Incident controller", func(t *testing.T) {
		for _, st := range []v1alpha1.State{v1alpha1.StateOpen, v1alpha1.StateVerifying, v1alpha1.StateClosed} {
			r, kube, inc := resolvedFixture(t, st, 365*24*time.Hour, nil)
			res, err := r.Reconcile(context.Background(), reqOf(inc))
			if err != nil || res.RequeueAfter != 0 {
				t.Errorf("%s: res = %+v, err = %v; want nothing", st, res, err)
			}
			if !present(t, kube, inc) {
				t.Errorf("a %s incident was deleted by the Resolved controller", st)
			}
		}
	})
	t.Run("retention 0: kept for ever, no requeue", func(t *testing.T) {
		r, kube, inc := resolvedFixture(t, v1alpha1.StateResolved, 365*24*time.Hour, nil)
		r.cfg.Retention = 0
		res, err := r.Reconcile(context.Background(), reqOf(inc))
		if err != nil || res.RequeueAfter != 0 || !present(t, kube, inc) {
			t.Errorf("res = %+v, err = %v; want nothing done", res, err)
		}
	})
	t.Run("gone: nothing to do", func(t *testing.T) {
		r, kube, inc := resolvedFixture(t, v1alpha1.StateResolved, 365*24*time.Hour, nil)
		cur := &v1alpha1.Incident{}
		_ = kube.Get(context.Background(), client.ObjectKeyFromObject(inc), cur)
		cur.Finalizers = nil
		if err := kube.Update(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		if err := kube.Delete(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		if res, err := r.Reconcile(context.Background(), reqOf(inc)); err != nil || res.RequeueAfter != 0 {
			t.Errorf("res = %+v, err = %v", res, err)
		}
	})
}

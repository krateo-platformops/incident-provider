//go:build integration

package incident

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// TestClosedEnvtest: Closed incidents are not in the manager's cache; deleting one still
// completes (the Closed controller removes the finalizer and the incident's check ConfigMap);
// a request to apply a Closed incident is refused as Closed, not as missing.
func TestClosedEnvtest(t *testing.T) {
	ctx, kube, _ := startEnvtest(t) // retention 0: nothing expires
	cached := func(inc *v1alpha1.Incident) bool {
		err := envMgr.GetCache().Get(ctx, client.ObjectKeyFromObject(inc), &v1alpha1.Incident{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return err == nil
	}

	inc := newEnvIncident(ctx, t, kube, "to-close", nil)
	writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) { st.State = v1alpha1.StateOpen })
	eventually(t, "the Open incident is in the manager's cache, with the finalizer", func() bool {
		return cached(inc) && len(get(ctx, t, kube, inc).Finalizers) > 0
	})

	patchSpec(ctx, t, kube, inc, `{"spec":{"closed":true}}`)
	eventually(t, "it turns Closed", func() bool { return get(ctx, t, kube, inc).Status.State == v1alpha1.StateClosed })
	eventually(t, "a Closed incident leaves the manager's cache", func() bool { return !cached(inc) })
	if f := get(ctx, t, kube, inc).Finalizers; len(f) == 0 || f[0] != reconciler.FinalizerName {
		t.Fatalf("finalizers = %v, want %s still on the Closed incident", f, reconciler.FinalizerName)
	}

	t.Run("deleting a Closed incident completes and removes its check ConfigMap", func(t *testing.T) {
		uid := get(ctx, t, kube, inc).UID
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "left-behind", Namespace: checksNamespace,
			Labels: map[string]string{labelIncidentUID: string(uid), LabelComponent: ComponentCheck}}}
		if err := kube.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
		if err := kube.Delete(ctx, get(ctx, t, kube, inc)); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the Closed incident is gone, finalizer removed", func() bool {
			err := kube.Get(ctx, client.ObjectKeyFromObject(inc), &v1alpha1.Incident{})
			return apierrors.IsNotFound(err)
		})
		eventually(t, "its check ConfigMap is gone", func() bool {
			return apierrors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}))
		})
	})

	// An Incident is created without a status (the status subresource drops it on create), so it is
	// cached until the watch delivers the write that makes it Closed. Once out, it stays out.
	t.Run("an Incident written Closed leaves the cache and stays out", func(t *testing.T) {
		pre := newEnvIncident(ctx, t, kube, "written-closed", nil)
		patchSpec(ctx, t, kube, pre, `{"spec":{"closed":true}}`)
		writeStatus(ctx, t, kube, pre, func(st *v1alpha1.IncidentStatus) {
			st.State = v1alpha1.StateClosed
			st.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: metav1.NewTime(time.Now()).Rfc3339Copy()}
		})
		eventually(t, "the Closed incident leaves the manager's cache", func() bool { return !cached(pre) })
		holds(t, 3*poll, func() string {
			if cached(pre) {
				return "a Closed incident is in the manager's cache"
			}
			return ""
		})
	})
}

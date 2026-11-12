//go:build integration

package incident

import (
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// TestRetentionEnvtest runs the Incident reconciler against a real API server with a 3s
// retention: a Resolved incident that ended long ago is deleted, finalizer and all; one a person
// closes during the test is deleted once its retention passes, not before; an Open one stays.
func TestRetentionEnvtest(t *testing.T) {
	const retention = 3 * time.Second
	ctx, kube, _ := startEnvtest(t, func(c *Config) { c.Retention = retention })

	gone := func(inc *v1alpha1.Incident) bool {
		err := kube.Get(ctx, client.ObjectKeyFromObject(inc), &v1alpha1.Incident{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return apierrors.IsNotFound(err)
	}

	old := newEnvIncident(ctx, t, kube, "resolved-long-ago", nil)
	writeStatus(ctx, t, kube, old, func(st *v1alpha1.IncidentStatus) {
		st.State = v1alpha1.StateResolved
		st.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: metav1.NewTime(time.Now().Add(-time.Hour)).Rfc3339Copy()}
	})
	open := newEnvIncident(ctx, t, kube, "still-open", nil)
	writeStatus(ctx, t, kube, open, func(st *v1alpha1.IncidentStatus) { st.State = v1alpha1.StateOpen })
	closing := newEnvIncident(ctx, t, kube, "closed-now", nil)
	writeStatus(ctx, t, kube, closing, func(st *v1alpha1.IncidentStatus) { st.State = v1alpha1.StateOpen })

	eventually(t, "the incident resolved long ago is deleted", func() bool { return gone(old) })

	patchSpec(ctx, t, kube, closing, `{"spec":{"closed":true}}`)
	var closedAt time.Time
	eventually(t, "the closed incident carries resolution.at", func() bool {
		got := get(ctx, t, kube, closing)
		if got.Status.State != v1alpha1.StateClosed || got.Status.Resolution == nil {
			return false
		}
		closedAt = got.Status.Resolution.At.Time
		return true
	})
	eventually(t, "the closed incident is deleted after its retention", func() bool { return gone(closing) })
	// resolution.at is RFC 3339 (second precision): allow that second.
	if early := closedAt.Add(retention).Sub(time.Now()); early > time.Second {
		t.Errorf("deleted %v before its retention ran out", early)
	}

	holds(t, 2*retention, func() string {
		if gone(open) {
			return "an Open incident was deleted"
		}
		return ""
	})
}

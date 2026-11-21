package incident

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// maxRetentionRequeue bounds how long an incident waits for its retention before it is read again.
const maxRetentionRequeue = 6 * time.Hour

// retain deletes inc once its retention has passed and, until then, says when to read it again.
// inc must be read past any cache: status.resolution.at decides, and a cached copy can lag the
// incident's own status writes.
//
// The delete is pinned to the UID and resourceVersion read, so an incident recreated under the same
// name, or changed since the read (a Resolved incident that reopened), is never deleted in its
// place: the conflict sends it back for a fresh read. retain never writes the incident. That is why
// retention is not done by the Incident controller: provider-runtime writes a managed resource's
// status after every successful Update, so a delete there was followed by a status write onto the
// deleted object, an error logged for every incident retention removed.
func retain(ctx context.Context, kube client.Client, inc *v1alpha1.Incident, cfg Config, now time.Time, log logging.Logger) (reconcile.Result, error) {
	if !expired(inc, cfg, now) {
		if cfg.Retention <= 0 || inc.Status.Resolution == nil || inc.Status.Resolution.At.IsZero() {
			return reconcile.Result{}, nil
		}
		wait := inc.Status.Resolution.At.Add(cfg.Retention).Sub(now) + time.Second
		return reconcile.Result{RequeueAfter: min(wait, maxRetentionRequeue)}, nil
	}
	uid, rv := inc.UID, inc.ResourceVersion
	if err := kube.Delete(ctx, inc, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return reconcile.Result{}, nil
		case apierrors.IsConflict(err):
			// Recreated or changed since the read: decide again on a fresh one.
			return reconcile.Result{RequeueAfter: time.Second}, nil
		}
		return reconcile.Result{}, fmt.Errorf("cannot delete incident past its retention: %w", err)
	}
	log.Info("deleted after its retention", "incident", inc.Namespace+"/"+inc.Name, "state", string(inc.Status.State),
		"resolvedBy", string(inc.Status.Resolution.By), "resolvedAt", inc.Status.Resolution.At.UTC().Format(time.RFC3339),
		"retention", cfg.Retention.String())
	return reconcile.Result{}, nil
}

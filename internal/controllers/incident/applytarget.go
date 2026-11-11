package incident

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// applyTarget is what API discovery says about the apiVersion and resource that
// status.howToFix.applyAction targets.
type applyTarget struct {
	// served: the cluster serves the resource at the apiVersion.
	served bool
	// versions are the versions of the target's group that serve the resource, sorted. Read only
	// when served is false; empty when no version serves it.
	versions []string
}

// applyActionOf is the incident's applyAction, nil without one.
func applyActionOf(inc *v1alpha1.Incident) *v1alpha1.ApplyAction {
	if inc.Status.HowToFix == nil {
		return nil
	}
	return inc.Status.HowToFix.ApplyAction
}

// lookupApplyTarget asks discovery whether the cluster serves resource at apiVersion and, when it
// does not, which versions of the group do. It reads discovery itself, never a cache, so a version
// that stopped being served is seen as soon as the API server stops serving it. Only a NotFound
// for a group version, or a group version whose listing lacks the resource, counts as not served:
// any other failure is returned and decides nothing. An apiVersion that does not parse is served
// nowhere.
func lookupApplyTarget(d discovery.DiscoveryInterface, apiVersion, resource string) (applyTarget, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return applyTarget{}, nil
	}
	list, err := d.ServerResourcesForGroupVersion(gv.String())
	switch {
	case err == nil && hasResource(list, resource):
		return applyTarget{served: true}, nil
	case err != nil && !apierrors.IsNotFound(err):
		return applyTarget{}, fmt.Errorf("cannot read the resources of %s: %w", gv, err)
	}
	versions, err := servingVersions(d, gv.Group, resource)
	if err != nil {
		return applyTarget{}, err
	}
	return applyTarget{versions: versions}, nil
}

// servingVersions are the versions of group that serve resource, sorted. It reads aggregated
// discovery when the API server offers it (two requests), and otherwise each version of the group.
func servingVersions(d discovery.DiscoveryInterface, group, resource string) ([]string, error) {
	var groups *metav1.APIGroupList
	if ad, ok := d.(discovery.AggregatedDiscoveryInterface); ok {
		g, lists, failed, err := ad.GroupsAndMaybeResources()
		if err != nil {
			return nil, fmt.Errorf("cannot read the API groups: %w", err)
		}
		for gv, err := range failed {
			if gv.Group == group {
				return nil, fmt.Errorf("cannot read the resources of %s: %w", gv, err)
			}
		}
		if lists != nil {
			var versions []string
			for gv, l := range lists {
				if gv.Group == group && hasResource(l, resource) {
					versions = append(versions, gv.Version)
				}
			}
			sort.Strings(versions)
			return versions, nil
		}
		groups = g
	} else {
		g, err := d.ServerGroups()
		if err != nil {
			return nil, fmt.Errorf("cannot read the API groups: %w", err)
		}
		groups = g
	}
	var versions []string
	for _, g := range groups.Groups {
		if g.Name != group {
			continue
		}
		for _, v := range g.Versions {
			l, err := d.ServerResourcesForGroupVersion(v.GroupVersion)
			switch {
			case apierrors.IsNotFound(err):
				// It stopped being served since the groups were read.
			case err != nil:
				return nil, fmt.Errorf("cannot read the resources of %s: %w", v.GroupVersion, err)
			case hasResource(l, resource):
				versions = append(versions, v.Version)
			}
		}
	}
	sort.Strings(versions)
	return versions, nil
}

func hasResource(l *metav1.APIResourceList, resource string) bool {
	if l == nil {
		return false
	}
	return slices.ContainsFunc(l.APIResources, func(r metav1.APIResource) bool { return r.Name == resource })
}

// observeApplyAction sets ApplyActionStale from t, what discovery says about the target of the
// incident's applyAction: True when it is not served, False when it is. Without an applyAction the
// condition is removed. t nil means discovery was not read, or the read failed: the condition stays
// as it is. lastTransitionTime changes only when the status flips. applyAction itself is never
// changed.
func observeApplyAction(inc *v1alpha1.Incident, t *applyTarget, now time.Time) []change {
	a := applyActionOf(inc)
	if a == nil {
		removeCondition(inc, v1alpha1.TypeApplyActionStale)
		return nil
	}
	if t == nil {
		return nil
	}
	target := a.APIVersion + " " + a.Resource
	c := prv1.Condition{
		Type:    v1alpha1.TypeApplyActionStale,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.ReasonServed,
		Message: "applyAction targets " + target + ", which this cluster serves",
	}
	if !t.served {
		served := "none"
		c.Status, c.Reason = metav1.ConditionTrue, v1alpha1.ReasonResourceNotFound
		if len(t.versions) > 0 {
			served = strings.Join(t.versions, ", ")
			c.Reason = v1alpha1.ReasonVersionNotServed
		}
		c.Message = "applyAction targets " + target + ", which this cluster does not serve; served: " + served
	}
	old := inc.GetCondition(v1alpha1.TypeApplyActionStale)
	c.LastTransitionTime = metav1.NewTime(now).Rfc3339Copy()
	if old.Status == c.Status {
		c.LastTransitionTime = old.LastTransitionTime
	}
	inc.SetConditions(c)

	kv := []any{"state", string(inc.Status.State), "apiVersion", a.APIVersion, "resource", a.Resource}
	switch {
	case c.Status == metav1.ConditionTrue && old.Status != metav1.ConditionTrue:
		return []change{{reason: "ApplyActionStale", message: c.Message, kv: kv}}
	case c.Status == metav1.ConditionFalse && old.Status == metav1.ConditionTrue:
		return []change{{reason: "ApplyActionServed", message: c.Message, kv: kv}}
	}
	return nil
}

// removeCondition removes the condition of type ct, if any.
func removeCondition(inc *v1alpha1.Incident, ct prv1.ConditionType) {
	inc.Status.Conditions = slices.DeleteFunc(inc.Status.Conditions, func(c prv1.Condition) bool { return c.Type == ct })
}

// targetReads is when each incident's applyAction target was last read from discovery, and which
// target it was. Held in memory only.
type targetReads struct {
	mu sync.Mutex
	at map[types.UID]targetRead
}

type targetRead struct {
	at     time.Time
	target string
}

func newTargetReads() *targetReads {
	return &targetReads{at: map[types.UID]targetRead{}}
}

// due reports whether target, the incident's current applyAction target, should be read at now:
// it was never read, it is not the target read last, or the last read was at least every ago.
func (r *targetReads) due(uid types.UID, target string, now time.Time, every time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.at[uid]
	return !ok || last.target != target || now.Sub(last.at) >= every
}

func (r *targetReads) seen(uid types.UID, target string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at[uid] = targetRead{at: at, target: target}
}

func (r *targetReads) forget(uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.at, uid)
}

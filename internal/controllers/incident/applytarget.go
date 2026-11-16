package incident

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
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
	// retarget is whether the applyAction can move to another version without a reanalysis. Set
	// only when served is false and versions is not empty.
	retarget *retarget
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
// incident's applyAction: True when it is not served, False when it is. An Open or Verifying
// incident without an applyAction loses the condition. In the other states it is left as it is:
// a reanalysis (Analyzing) clears howToFix, and the condition must outlive it, so that a new
// analysis that targets an unserved version again continues the episode instead of starting a new
// one. t nil means discovery was not read, or the read failed: the condition stays as it is.
// lastTransitionTime changes only when the status flips. applyAction itself is never changed.
func observeApplyAction(inc *v1alpha1.Incident, t *applyTarget, now time.Time) []change {
	a := applyActionOf(inc)
	if a == nil {
		if s := inc.Status.State; s == v1alpha1.StateOpen || s == v1alpha1.StateVerifying {
			removeCondition(inc, v1alpha1.TypeApplyActionStale)
		}
		return nil
	}
	if t == nil {
		return nil
	}
	target := a.APIVersion + " " + a.Resource
	old := inc.GetCondition(v1alpha1.TypeApplyActionStale)
	kv := []any{"state", string(inc.Status.State), "apiVersion", a.APIVersion, "resource", a.Resource}
	if t.served && old.Status == metav1.ConditionFalse && old.Reason == v1alpha1.ReasonRetargeted &&
		strings.Contains(old.Message, " to "+target+":") {
		// The controller's own re-target, still served: its record stays.
		return nil
	}
	notRetargeted := ""
	if !t.served && t.retarget != nil {
		why := t.retarget.why
		if why == "" {
			why = scriptsNaming(inc.Status.HowToFix, a.APIVersion, a.Resource)
		}
		if why == "" {
			return retargetTo(inc, a, t.retarget.to, old, now, kv)
		}
		notRetargeted = "; not re-targeted: " + why
	}
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
		c.Message = "applyAction targets " + target + ", which this cluster does not serve; served: " + served + notRetargeted
	}
	c.LastTransitionTime = metav1.NewTime(now).Rfc3339Copy()
	if old.Status == c.Status {
		c.LastTransitionTime = old.LastTransitionTime
	}
	// An episode whose own reanalysis was answered and is still stale says so: the controller asks
	// once per episode, so from here only a person can ask again.
	answered := c.Status == metav1.ConditionTrue && staleAnswered(inc, c)
	if answered {
		c.Message += "; " + answeredSuffix(inc)
	}
	inc.SetConditions(c)

	switch {
	case c.Status == metav1.ConditionTrue && old.Status != metav1.ConditionTrue:
		return []change{{reason: "ApplyActionStale", message: c.Message, kv: kv}}
	case c.Status == metav1.ConditionFalse && old.Status == metav1.ConditionTrue:
		return []change{{reason: "ApplyActionServed", message: c.Message, kv: kv}}
	case answered && !strings.Contains(old.Message, answeredMarker):
		return []change{{reason: reasonStaleAfterReanalysis, message: c.Message, kv: kv}}
	}
	return nil
}

// retargetTo moves a, the incident's applyAction, to apiVersion to, and records it in a False
// ApplyActionStale with reason Retargeted. The status write that persists it carries the
// resourceVersion read, so it never overwrites an analysis the writer wrote in between.
func retargetTo(inc *v1alpha1.Incident, a *v1alpha1.ApplyAction, to string, old prv1.Condition, now time.Time, kv []any) []change {
	from := a.APIVersion
	if err := retargetAction(a, to); err != nil {
		// planRetarget parsed the same payload: unreachable, and then nothing changes.
		return nil
	}
	c := prv1.Condition{
		Type:   v1alpha1.TypeApplyActionStale,
		Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonRetargeted,
		Message: "applyAction re-targeted from " + from + " to " + to + " " + a.Resource + ": " + from +
			" is not served, and every field it sets is declared in " + to + "; review and apply it again",
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	}
	if old.Status == c.Status {
		c.LastTransitionTime = old.LastTransitionTime
	}
	inc.SetConditions(c)
	return []change{{reason: reasonApplyActionRetargeted, message: c.Message, kv: append(kv, "to", to)}}
}

// reasonApplyActionRetargeted is the Event and log reason of the controller's re-target.
const reasonApplyActionRetargeted = "ApplyActionRetargeted"

// reasonStaleAfterReanalysis is the Event and log reason of an episode whose own reanalysis was
// answered and still targets an unserved version.
const reasonStaleAfterReanalysis = "StaleAfterReanalysis"

// answeredMarker begins the suffix of the message of an answered episode that is still stale.
const answeredMarker = "the reanalysis requested for this episode"

// answeredSuffix is what the message of an answered episode that is still stale adds: when the
// answering reanalysis was consumed, and that only a person can ask again.
func answeredSuffix(inc *v1alpha1.Incident) string {
	at := ""
	if r := inc.Status.Reanalysis; r != nil && !r.At.IsZero() {
		at = " (consumed " + r.At.UTC().Format(time.RFC3339) + ")"
	}
	return answeredMarker + " was answered" + at + " and still targets an unserved version; " +
		"a person must re-run the analysis (the portal's Re-run analysis, spec.reanalysis)"
}

// staleRecord is the controller's record of its reanalysis request for an episode
// (v1alpha1.AnnotationStaleReanalysis).
type staleRecord struct {
	// episode is the condition's lastTransitionTime when the controller asked.
	episode string
	// count is status.reanalysis.count when the controller asked.
	count int32
}

// episodeOf is the key of the episode c belongs to: its lastTransitionTime, RFC 3339 in UTC.
func episodeOf(c prv1.Condition) string {
	return c.LastTransitionTime.UTC().Format(time.RFC3339)
}

// reanalysisCount is status.reanalysis.count, 0 when the incident has had none.
func reanalysisCount(inc *v1alpha1.Incident) int32 {
	if inc.Status.Reanalysis == nil {
		return 0
	}
	return inc.Status.Reanalysis.Count
}

// recordValue is the annotation value that records a request for c's episode at count.
func recordValue(c prv1.Condition, count int32) string {
	return fmt.Sprintf("%s/%d", episodeOf(c), count)
}

// staleRecordOf parses the controller's record, if inc carries a well-formed one.
func staleRecordOf(inc *v1alpha1.Incident) (staleRecord, bool) {
	v, ok := inc.Annotations[v1alpha1.AnnotationStaleReanalysis]
	if !ok {
		return staleRecord{}, false
	}
	ep, n, ok := strings.Cut(v, "/")
	if !ok || ep == "" {
		return staleRecord{}, false
	}
	var count int32
	if _, err := fmt.Sscanf(n, "%d", &count); err != nil || count < 0 {
		return staleRecord{}, false
	}
	return staleRecord{episode: ep, count: count}, true
}

// requested reports whether the controller has asked for a reanalysis in c's episode.
func requested(inc *v1alpha1.Incident, c prv1.Condition) bool {
	r, ok := staleRecordOf(inc)
	return ok && r.episode == episodeOf(c)
}

// staleAnswered reports whether the controller's request for c's episode has been answered: it
// asked in this episode, no request is pending, and the writer has consumed a reanalysis since it
// asked (status.reanalysis.count grew). Counts, not times: both sides of the comparison come from
// one writer each.
func staleAnswered(inc *v1alpha1.Incident, c prv1.Condition) bool {
	r, ok := staleRecordOf(inc)
	return ok && r.episode == episodeOf(c) && inc.Spec.Reanalysis == nil && reanalysisCount(inc) > r.count
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

// staleReanalysisDue reports whether the incident's ApplyActionStale episode calls for the
// controller's reanalysis request: the writer would answer a request (writerAnswers),
// ApplyActionStale is True, no request is pending (a person's wins), and the controller has not
// asked in this episode (its record, v1alpha1.AnnotationStaleReanalysis, names another episode or
// none). The record is written in the same patch as the request, so whether an episode was asked
// for never depends on comparing the writer's clock with the controller's. A new analysis that
// targets an unserved version again keeps the condition True with the same lastTransitionTime, so
// the episode stays asked for: there is one request per episode. A request the writer drops
// unanswered can only come from a race, the state changing between the request and its
// consumption; the record still names the episode, so nothing is filed again.
func staleReanalysisDue(inc *v1alpha1.Incident) bool {
	if !writerAnswers(inc) {
		return false
	}
	c := inc.GetCondition(v1alpha1.TypeApplyActionStale)
	if c.Status != metav1.ConditionTrue || inc.Spec.Reanalysis != nil {
		return false
	}
	return !requested(inc, c) && !budgetSpent(inc)
}

// staleRequests is how many reanalyses the controller has requested for the incident because an
// applyAction went stale (v1alpha1.AnnotationStaleReanalyses). Without the count, an incident with
// the controller's episode record was asked for once. ok is false when the count does not parse.
func staleRequests(inc *v1alpha1.Incident) (n int, ok bool) {
	v, has := inc.Annotations[v1alpha1.AnnotationStaleReanalyses]
	if !has {
		if _, asked := inc.Annotations[v1alpha1.AnnotationStaleReanalysis]; asked {
			return 1, true
		}
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// budgetSpent reports whether the controller has requested v1alpha1.StaleReanalysisBudget
// reanalyses for the incident, or its count does not parse.
func budgetSpent(inc *v1alpha1.Incident) bool {
	n, ok := staleRequests(inc)
	return !ok || n >= v1alpha1.StaleReanalysisBudget
}

// observeBudget sets ReanalysisBudgetExhausted on an Open or Verifying incident: True when its
// ApplyActionStale episode would call for the controller's request but the budget is spent, and
// removed otherwise. In other states it is left as it is.
func observeBudget(inc *v1alpha1.Incident, now time.Time) []change {
	if s := inc.Status.State; s != v1alpha1.StateOpen && s != v1alpha1.StateVerifying {
		return nil
	}
	stale := inc.GetCondition(v1alpha1.TypeApplyActionStale)
	if !writerAnswers(inc) || stale.Status != metav1.ConditionTrue || requested(inc, stale) || !budgetSpent(inc) {
		removeCondition(inc, v1alpha1.TypeReanalysisBudgetExhausted)
		return nil
	}
	n, ok := staleRequests(inc)
	msg := fmt.Sprintf("the controller has requested %d reanalyses for this incident's stale applyActions, its budget of %d",
		n, v1alpha1.StaleReanalysisBudget)
	if !ok {
		msg = "the controller's count of its reanalysis requests (annotation " + v1alpha1.AnnotationStaleReanalyses +
			") does not parse, so it requests none"
	}
	msg += "; a person must re-run the analysis (the portal's Re-run analysis, spec.reanalysis)"
	old := inc.GetCondition(v1alpha1.TypeReanalysisBudgetExhausted)
	c := prv1.Condition{Type: v1alpha1.TypeReanalysisBudgetExhausted, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ReasonBudgetSpent, Message: msg, LastTransitionTime: metav1.NewTime(now).Rfc3339Copy()}
	if old.Status == c.Status {
		c.LastTransitionTime = old.LastTransitionTime
	}
	inc.SetConditions(c)
	if old.Status == metav1.ConditionTrue {
		return nil
	}
	return []change{{reason: string(v1alpha1.TypeReanalysisBudgetExhausted), message: msg,
		kv: []any{"state", string(inc.Status.State), "requests", n}}}
}

// writerAnswers reports whether the writer would answer a reanalysis request on inc rather than
// drop it: inc is Open or Verifying and no person analyzes it. It mirrors alert-provider's
// Writer.Reanalyze (internal/incident/reanalysis.go at a469b1a), which removes the request
// unanswered (dropRequest) from an incident labelled LabelAnalysis=AnalysisHuman or in any other
// state.
func writerAnswers(inc *v1alpha1.Incident) bool {
	if inc.Labels[v1alpha1.LabelAnalysis] == v1alpha1.AnalysisHuman {
		return false
	}
	s := inc.Status.State
	return s == v1alpha1.StateOpen || s == v1alpha1.StateVerifying
}

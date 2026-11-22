package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/plumbing/kubeutil/event"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// A check whose very first run exits with a code other than 0 or 1 is a broken script, not a slow
// problem: a jq program that indexes a string, a resource type the cluster does not serve. Left to
// itself it would run NoVerdictRuns times and end in ChecksUnknown, and the incident would never
// have a verdict. Instead the controller asks the writer for a new analysis at once (cause
// CheckNoVerdict), with the script, its exit code and its output as feedback, once per analysis
// and at most v1alpha1.NoVerdictReanalysisBudget times per incident.
//
// verify normally first runs only once the incident is Verifying, after the fix. So on an Open
// incident's first pass the controller also runs verify once, read-only, through the same check
// pod path (the probe): a verify that cannot answer is caught before anyone applies the fix. The
// probe's exit 0 or 1 is discarded: it says the script can answer, not what the answer is, and it
// never moves the incident.

// firstNoVerdict is a check's first run in the current analysis that exited with a code other
// than 0 or 1.
type firstNoVerdict struct {
	script v1alpha1.Script
	exit   int32
	output string
}

// noVerdictOnFirstRun is r as a firstNoVerdict when it is a first run (first: the script had no
// recorded result in this analysis) that exited with a code other than 0 or 1. A run without an
// exit code (a timeout, a pod that never ran) is not the script's fault and is left to the
// no-verdict streak.
func noVerdictOnFirstRun(r result, first bool) *firstNoVerdict {
	if !first || r.exit == nil || known(r.exit) {
		return nil
	}
	return &firstNoVerdict{script: r.script, exit: *r.exit, output: r.output}
}

// analysisID names the incident's current analysis: status.reanalysis.count, 0 for the first.
func analysisID(inc *v1alpha1.Incident) string {
	return strconv.Itoa(int(reanalysisCount(inc)))
}

// firstRun reports whether s has no recorded result in the current analysis. A reanalysis clears
// status.lastChecks, so a new analysis starts over.
func firstRun(inc *v1alpha1.Incident, s v1alpha1.Script) bool {
	return inc.Status.LastChecks == nil || *slot(inc.Status.LastChecks, s) == nil
}

// noVerdictRequests is how many CheckNoVerdict reanalyses the controller has requested for inc; ok
// is false when the count does not parse.
func noVerdictRequests(inc *v1alpha1.Incident) (n int, ok bool) {
	v, has := inc.Annotations[v1alpha1.AnnotationNoVerdictReanalyses]
	if !has {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// noVerdictReanalysisDue reports whether the controller may request a CheckNoVerdict reanalysis
// for inc now: the writer would answer it, no request is pending, the current analysis has not been
// asked for, and the budget is not spent.
func noVerdictReanalysisDue(inc *v1alpha1.Incident) bool {
	if !writerAnswers(inc) || inc.Spec.Reanalysis != nil {
		return false
	}
	if inc.Annotations[v1alpha1.AnnotationNoVerdictReanalysis] == analysisID(inc) {
		return false
	}
	n, ok := noVerdictRequests(inc)
	return ok && n < v1alpha1.NoVerdictReanalysisBudget
}

// maxFeedback is spec.reanalysis.feedback's MaxLength.
const maxFeedback = 2000

// noVerdictFeedback is the request's feedback: the script, its exit code and its output, cut to
// the field's limit.
func noVerdictFeedback(f firstNoVerdict) string {
	msg := fmt.Sprintf("%s exited %d on its first run, so it can never give a verdict (0 or 1)", f.script, f.exit)
	if out := strings.TrimSpace(f.output); out != "" {
		msg += ": " + out
	}
	return excerpt(msg, maxFeedback)
}

// requestNoVerdictReanalysis sets spec.reanalysis (cause CheckNoVerdict) with f's feedback and the
// controller's record, in one merge patch carrying the resourceVersion read, as requestReanalysis
// does. It reports whether it filed the request.
func (e *external) requestNoVerdictReanalysis(ctx context.Context, inc *v1alpha1.Incident, f firstNoVerdict) (bool, error) {
	cur := inc
	for range maxRequestAttempts {
		if !noVerdictReanalysisDue(cur) {
			return false, nil
		}
		n, _ := noVerdictRequests(cur) // due checked that it parses
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{
				"resourceVersion": cur.ResourceVersion,
				"annotations": map[string]any{
					v1alpha1.AnnotationNoVerdictReanalysis: analysisID(cur),
					v1alpha1.AnnotationNoVerdictReanalyses: strconv.Itoa(n + 1),
				},
			},
			"spec": map[string]any{"reanalysis": v1alpha1.ReanalysisRequest{
				Cause:    v1alpha1.ReanalysisCauseCheckNoVerdict,
				Feedback: noVerdictFeedback(f),
			}},
		})
		if err != nil {
			return false, fmt.Errorf("cannot encode the reanalysis request: %w", err)
		}
		err = e.kube.Patch(ctx, cur, client.RawPatch(types.MergePatchType, patch))
		if err == nil {
			msg := fmt.Sprintf("requested a reanalysis, cause CheckNoVerdict: %s exited %d on its first run", f.script, f.exit)
			e.rec.Event(cur, event.Normal(event.Reason(reasonReanalysisRequested), actionReconcile, msg))
			e.log.Info(msg, "reason", reasonReanalysisRequested, "cause", string(v1alpha1.ReanalysisCauseCheckNoVerdict),
				"script", string(f.script), "exit", f.exit, "state", string(cur.Status.State))
			return true, nil
		}
		if !apierrors.IsConflict(err) {
			return false, fmt.Errorf("cannot request a reanalysis: %w", err)
		}
		fresh := &v1alpha1.Incident{}
		if err := e.reader.Get(ctx, client.ObjectKeyFromObject(cur), fresh); err != nil {
			return false, fmt.Errorf("cannot read the incident again to request a reanalysis: %w", err)
		}
		cur = fresh
	}
	return false, fmt.Errorf("cannot request a reanalysis: %d conflicting writes", maxRequestAttempts)
}

const (
	// labelProbe marks the probe: verify run once, read-only, on an Open incident's first pass.
	labelProbe = "observability.krateo.io/check-probe"
	// annotationProbeAnalysis is the analysis (analysisID) a probe ran the verify of.
	annotationProbeAnalysis = "observability.krateo.io/probe-analysis"
)

// isProbe reports whether pod is a probe.
func isProbe(pod *corev1.Pod) bool {
	return pod.Labels[labelProbe] == "true"
}

// probeDue reports whether inc's verify should run once now: the incident is Open, the writer would
// answer a request for it, it has a verify that has not run in this analysis, and this analysis has
// not been probed.
func probeDue(inc *v1alpha1.Incident) bool {
	return inc.Status.State == v1alpha1.StateOpen && writerAnswers(inc) &&
		scriptBody(inc, v1alpha1.ScriptVerify) != "" && firstRun(inc, v1alpha1.ScriptVerify) &&
		inc.Annotations[v1alpha1.AnnotationVerifyProbed] != analysisID(inc)
}

// probeName names the probe pod and its ConfigMap: one per incident and analysis.
func probeName(inc *v1alpha1.Incident) string {
	return checkName(inc, v1alpha1.ScriptVerify, time.Time{}) + "-probe-" + analysisID(inc)
}

// ProbePod is the pod that runs inc's verify once, read-only: a check pod in every respect but its
// name, its label and the analysis it records.
func (p CheckPods) ProbePod(inc *v1alpha1.Incident) *corev1.Pod {
	m := p.meta(inc, probeName(inc), v1alpha1.ScriptVerify)
	m.Labels[labelProbe] = "true"
	m.Annotations[annotationProbeAnalysis] = analysisID(inc)
	pod := p.sandbox(m, p.Timeout)
	pod.Spec.AutomountServiceAccountToken = ptr.To(true)
	return pod
}

// startProbe creates the probe pod, then the ConfigMap with inc's verify, owned by the pod.
func (e *external) startProbe(ctx context.Context, inc *v1alpha1.Incident) error {
	pod := e.pods.ProbePod(inc)
	if err := e.kube.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("cannot create the probe pod: %w", err)
		}
		if err := e.kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return fmt.Errorf("cannot get the probe pod: %w", err)
		}
	}
	if err := e.kube.Create(ctx, e.pods.ConfigMap(inc, pod, v1alpha1.ScriptVerify)); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("cannot create the probe ConfigMap: %w", err)
	}
	e.log.Debug("started the verify probe", "pod", pod.Name)
	return nil
}

// markProbed records that the current analysis' verify has been probed (AnnotationVerifyProbed),
// in a merge patch carrying the resourceVersion read; on a conflict it reads the incident again
// and records it only if that analysis is still current.
func (e *external) markProbed(ctx context.Context, inc *v1alpha1.Incident, analysis string) error {
	cur := inc
	for range maxRequestAttempts {
		if analysisID(cur) != analysis || cur.Annotations[v1alpha1.AnnotationVerifyProbed] == analysis {
			return nil
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"resourceVersion": cur.ResourceVersion,
			"annotations":     map[string]any{v1alpha1.AnnotationVerifyProbed: analysis},
		}})
		if err != nil {
			return fmt.Errorf("cannot encode the probe record: %w", err)
		}
		err = e.kube.Patch(ctx, cur, client.RawPatch(types.MergePatchType, patch))
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("cannot record the verify probe: %w", err)
		}
		fresh := &v1alpha1.Incident{}
		if err := e.reader.Get(ctx, client.ObjectKeyFromObject(cur), fresh); err != nil {
			return fmt.Errorf("cannot read the incident again to record the verify probe: %w", err)
		}
		cur = fresh
	}
	return fmt.Errorf("cannot record the verify probe: %d conflicting writes", maxRequestAttempts)
}

// observeProbe reads a probe pod: running, it is left alone (probing); finished, it is deleted, and
// when it ran the current analysis' verify the analysis is recorded as probed and an exit other
// than 0 or 1 is a first-run no-verdict. A probe of an earlier analysis is deleted unread.
func (e *external) observeProbe(inc *v1alpha1.Incident, pod *corev1.Pod, now time.Time) {
	r, done := resultOf(pod, e.cfg.CheckTimeout, now)
	if !done && pod.Annotations[annotationProbeAnalysis] == analysisID(inc) {
		e.plan.probing = true
		return
	}
	e.plan.stale = append(e.plan.stale, *pod)
	if !done || pod.Annotations[annotationProbeAnalysis] != analysisID(inc) {
		return
	}
	e.plan.probed = analysisID(inc)
	r.script = v1alpha1.ScriptVerify
	if f := noVerdictOnFirstRun(r, true); f != nil && e.plan.noVerdict == nil {
		e.plan.noVerdict = f
	}
}

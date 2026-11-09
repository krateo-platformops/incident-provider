package incident

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// Config is the check cadence and its limits.
type Config struct {
	// PollInterval is the time from the end of one check to the start of the next.
	PollInterval time.Duration

	// SettleWindow is how long, from entering Verifying, a verify exit 1 is retried instead of
	// moving the incident back to Open.
	SettleWindow time.Duration

	// CheckTimeout bounds one check run. It is the check pod's activeDeadlineSeconds.
	CheckTimeout time.Duration

	// NoVerdictRuns stops the current script once this many consecutive runs gave no verdict.
	// Zero disables this bound.
	NoVerdictRuns int32

	// NoVerdictFor stops the current script once its runs have given no verdict for this long,
	// from the end of the first run of the streak to the end of the latest. Zero disables this
	// bound.
	NoVerdictFor time.Duration

	// AlertGoneGrace is how long the Alert in spec.alertRef must stay NotFound before an Open or
	// Verifying incident is closed with resolution.by alert-gone. Zero closes it at the first
	// NotFound.
	AlertGoneGrace time.Duration
}

// pendingGrace is how long past CheckTimeout a check pod that has not finished counts as timed
// out. It covers the pods activeDeadlineSeconds cannot stop: those that never start.
const pendingGrace = time.Minute

// result is one finished run of a script.
type result struct {
	script v1alpha1.Script
	// exit is nil when the run produced no exit code: a timeout, or a pod that never ran.
	exit *int32
	at   time.Time
	// output is what the run printed, or why it gave no exit code.
	output string
}

// maxOutput bounds lastChecks output, in bytes.
const maxOutput = 512

// known reports whether exit is a verdict: 0 the incident is gone, 1 it holds.
func known(exit *int32) bool {
	return exit != nil && (*exit == 0 || *exit == 1)
}

// outputOf is the output kept for a run of exit: the start of out for an unknown result, cut to
// maxOutput on a UTF-8 boundary, and nothing for a verdict.
func outputOf(exit *int32, out string) string {
	if known(exit) {
		return ""
	}
	out = strings.TrimSpace(out)
	if len(out) <= maxOutput {
		return out
	}
	cut := maxOutput
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut]
}

// change is a state change worth an event and an INFO log line.
type change struct {
	reason  string
	message string
	// kv are the log line's structured fields besides the incident and the reason.
	kv []any
}

// currentScript is the script the incident runs in its current state, or "" when it runs none.
func currentScript(inc *v1alpha1.Incident) v1alpha1.Script {
	h := inc.Status.HowToFix
	if h == nil {
		return ""
	}
	s := scriptOf(inc.Status.State)
	if s == "" || scriptBody(inc, s) == "" || stopped(inc, s) {
		return ""
	}
	return s
}

// scriptOf is the script a state runs, or "" for a state that runs none.
func scriptOf(state v1alpha1.State) v1alpha1.Script {
	switch state {
	case v1alpha1.StateOpen:
		return v1alpha1.ScriptPrecondition
	case v1alpha1.StateVerifying:
		return v1alpha1.ScriptVerify
	}
	return ""
}

// noVerdictReason is the ChecksUnknown reason that names s.
func noVerdictReason(s v1alpha1.Script) prv1.ConditionReason {
	if s == v1alpha1.ScriptVerify {
		return v1alpha1.ReasonVerifyNoVerdict
	}
	return v1alpha1.ReasonPreconditionNoVerdict
}

// stopped reports whether s hit the no-verdict bound and no longer runs: ChecksUnknown is True and
// names s, and the latest result of s is still an unknown streak. A reanalysis clears lastChecks,
// so its new scripts run.
func stopped(inc *v1alpha1.Incident, s v1alpha1.Script) bool {
	c := inc.GetCondition(v1alpha1.TypeChecksUnknown)
	if c.Status != metav1.ConditionTrue || c.Reason != noVerdictReason(s) || inc.Status.LastChecks == nil {
		return false
	}
	last := *slot(inc.Status.LastChecks, s)
	return last != nil && last.NoVerdict != nil
}

// syncChecksUnknown sets ChecksUnknown False once the script it stopped is no longer current: the
// state changed, a human applied again while Verifying, or a reanalysis replaced the scripts.
func syncChecksUnknown(inc *v1alpha1.Incident, now time.Time) {
	if inc.GetCondition(v1alpha1.TypeChecksUnknown).Status != metav1.ConditionTrue {
		return
	}
	if s := scriptOf(inc.Status.State); s != "" && stopped(inc, s) {
		return
	}
	inc.SetConditions(prv1.Condition{
		Type:               v1alpha1.TypeChecksUnknown,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonChecksResumed,
		Message:            "the script that gave no verdict is no longer current",
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	})
}

// scriptBody is the bash source of a script.
func scriptBody(inc *v1alpha1.Incident, s v1alpha1.Script) string {
	h := inc.Status.HowToFix
	if h == nil {
		return ""
	}
	switch s {
	case v1alpha1.ScriptPrecondition:
		return h.Precondition
	case v1alpha1.ScriptVerify:
		return h.Verify
	}
	return ""
}

// reproduced reports whether the first precondition run has a verdict, 0 or 1.
func reproduced(inc *v1alpha1.Incident) bool {
	return inc.GetCondition(v1alpha1.TypeReproduced).Status != metav1.ConditionUnknown
}

// slot is the lastChecks entry of s.
func slot(lc *v1alpha1.LastChecks, s v1alpha1.Script) **v1alpha1.LastCheck {
	switch s {
	case v1alpha1.ScriptPrecondition:
		return &lc.Precondition
	case v1alpha1.ScriptApply:
		return &lc.Apply
	}
	return &lc.Verify
}

// setLastCheck records a result of s at at, with the output of an unknown precondition or verify
// result. A precondition or verify result equal to the one recorded keeps its since and output, so
// a check that keeps giving the same verdict changes nothing. An unknown precondition or verify
// result extends the no-verdict streak, once per run; a verdict ends it. Every apply run replaces
// the entry.
func setLastCheck(inc *v1alpha1.Incident, s v1alpha1.Script, exit *int32, output string, at time.Time) {
	if inc.Status.LastChecks == nil {
		inc.Status.LastChecks = &v1alpha1.LastChecks{}
	}
	p := slot(inc.Status.LastChecks, s)
	since := metav1.NewTime(at).Rfc3339Copy()
	if s == v1alpha1.ScriptApply {
		*p = &v1alpha1.LastCheck{Exit: exit, Since: since}
		return
	}
	prev := *p
	var nv *v1alpha1.NoVerdict
	if !known(exit) {
		if nv = streak(prev, since); nv == nil {
			return
		}
	}
	if prev != nil && ptr.Equal(prev.Exit, exit) {
		prev.NoVerdict = nv
		return
	}
	*p = &v1alpha1.LastCheck{Exit: exit, Since: since, Output: outputOf(exit, output), NoVerdict: nv}
}

// streak is the no-verdict streak after prev with one more unknown run, ended at at. It is nil
// when the run is already counted: its pod outlived a failed delete and is read again.
func streak(prev *v1alpha1.LastCheck, at metav1.Time) *v1alpha1.NoVerdict {
	if prev == nil || known(prev.Exit) {
		return &v1alpha1.NoVerdict{Runs: 1, Since: at, Last: at}
	}
	nv := prev.NoVerdict
	if nv == nil {
		// An unknown result recorded before streaks were kept (0.7.0 and earlier): the streak
		// started at its since.
		return &v1alpha1.NoVerdict{Runs: 2, Since: prev.Since, Last: at}
	}
	if !at.After(nv.Last.Time) {
		return nil
	}
	return &v1alpha1.NoVerdict{Runs: nv.Runs + 1, Since: nv.Since, Last: at}
}

func clearLastCheck(inc *v1alpha1.Incident, s v1alpha1.Script) {
	if inc.Status.LastChecks != nil {
		*slot(inc.Status.LastChecks, s) = nil
	}
}

// checkFrom is when the wait for the next check started: the end of the last run, or the start of
// the settle window when that is later. It is the zero time when neither is known.
func checkFrom(inc *v1alpha1.Incident, lastRun time.Time) time.Time {
	if v := inc.Status.VerifyingSince; v != nil && v.After(lastRun) {
		return v.Time
	}
	return lastRun
}

// nextCheckAt is when the next check may start: a poll interval after checkFrom. It is the zero
// time when no check has run.
func nextCheckAt(inc *v1alpha1.Incident, lastRun time.Time, poll time.Duration) time.Time {
	from := checkFrom(inc, lastRun)
	if from.IsZero() {
		return from
	}
	return from.Add(poll)
}

// checkDue reports whether the current script should start at now.
func checkDue(inc *v1alpha1.Incident, cfg Config, lastRun, now time.Time) bool {
	return currentScript(inc) != "" && !now.Before(nextCheckAt(inc, lastRun, cfg.PollInterval))
}

// enterVerifying starts the settle window at at. The verify result of an earlier window is
// dropped: it describes a fix that no longer applies.
func enterVerifying(inc *v1alpha1.Incident, at time.Time) {
	inc.Status.VerifyingSince = ptr.To(metav1.NewTime(at).Rfc3339Copy())
	clearLastCheck(inc, v1alpha1.ScriptVerify)
}

func setState(inc *v1alpha1.Incident, to v1alpha1.State, why string, at time.Time) change {
	from := inc.Status.State
	inc.Status.State = to
	switch to {
	case v1alpha1.StateVerifying:
		enterVerifying(inc, at)
	case v1alpha1.StateOpen:
		// The precondition result that left Open describes a state that has moved on.
		clearLastCheck(inc, v1alpha1.ScriptPrecondition)
	}
	if to != v1alpha1.StateVerifying {
		inc.Status.VerifyingSince = nil
	}
	if from == "" {
		from = "none"
	}
	return change{reason: "StateChanged", message: fmt.Sprintf("%s -> %s: %s", from, to, why),
		kv: []any{"from", string(from), "to", string(to), "why", why}}
}

// applySpec applies the human decisions in spec to the status. consumed reports that
// spec.applied was acted on and must be set back to false.
func applySpec(inc *v1alpha1.Incident, now time.Time) (changes []change, consumed bool) {
	state := inc.Status.State
	if inc.Spec.Closed {
		if state == v1alpha1.StateClosed {
			return nil, false
		}
		c := setState(inc, v1alpha1.StateClosed, "spec.closed", now)
		inc.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: metav1.NewTime(now).Rfc3339Copy()}
		return []change{c}, false
	}
	if !inc.Spec.Applied {
		return nil, false
	}
	switch state {
	case v1alpha1.StateOpen:
		setLastCheck(inc, v1alpha1.ScriptApply, nil, "", now)
		return []change{setState(inc, v1alpha1.StateVerifying, "spec.applied", now)}, true
	case v1alpha1.StateVerifying:
		// A human applied again: record it and restart the settle window.
		setLastCheck(inc, v1alpha1.ScriptApply, nil, "", now)
		enterVerifying(inc, now)
		return nil, true
	}
	// Analyzing has no fix to apply yet; Resolved and Closed are over.
	return nil, false
}

// record sets a finished run of the current script as its latest result and applies its
// transition. The exit codes: 0 the incident is gone, 1 it holds, anything else or none is
// unknown and moves no state; enough unknown runs in a row stop the script (see noVerdict).
// Recording the same result twice changes nothing.
func record(inc *v1alpha1.Incident, cfg Config, r result, now time.Time) []change {
	setLastCheck(inc, r.script, r.exit, r.output, r.at)
	if !known(r.exit) {
		return noVerdict(inc, cfg, r, now)
	}
	holds := *r.exit == 1

	switch r.script {
	case v1alpha1.ScriptPrecondition:
		if !reproduced(inc) {
			if holds {
				inc.SetConditions(reproducedCondition(metav1.ConditionTrue, v1alpha1.ReasonPreconditionHolds,
					"the first precondition run exited 1", now))
				return nil
			}
			// The problem was gone by the first check. Verify decides: exit 0 resolves the
			// incident, exit 1 after the settle window sends it back to Open.
			inc.SetConditions(reproducedCondition(metav1.ConditionFalse, v1alpha1.ReasonPreconditionPassed,
				"the first precondition run exited 0: the problem was gone by the first check, so verify decides", now))
			return []change{setState(inc, v1alpha1.StateVerifying, "the first precondition run exited 0", r.at)}
		}
		if !holds {
			return []change{setState(inc, v1alpha1.StateVerifying, "precondition exited 0", r.at)}
		}
	case v1alpha1.ScriptVerify:
		if !holds {
			inc.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: metav1.NewTime(r.at).Rfc3339Copy()}
			return []change{setState(inc, v1alpha1.StateResolved, "verify exited 0", r.at)}
		}
		since := inc.Status.VerifyingSince
		if since == nil {
			// A Verifying incident without a window starts it at this run.
			inc.Status.VerifyingSince = ptr.To(metav1.NewTime(r.at).Rfc3339Copy())
			return nil
		}
		if r.at.Sub(since.Time) >= cfg.SettleWindow {
			return []change{setState(inc, v1alpha1.StateOpen, "verify exited 1 after the settle window", r.at)}
		}
	}
	return nil
}

// noVerdict stops the script of an unknown result once its no-verdict streak reaches
// cfg.NoVerdictRuns runs or lasts cfg.NoVerdictFor: it sets ChecksUnknown True, naming the script,
// with the last exit code and output. currentScript then returns no script for the state.
func noVerdict(inc *v1alpha1.Incident, cfg Config, r result, now time.Time) []change {
	if stopped(inc, r.script) {
		return nil
	}
	nv := (*slot(inc.Status.LastChecks, r.script)).NoVerdict
	if nv == nil {
		return nil
	}
	lasted := nv.Last.Sub(nv.Since.Time)
	if (cfg.NoVerdictRuns <= 0 || nv.Runs < cfg.NoVerdictRuns) && (cfg.NoVerdictFor <= 0 || lasted < cfg.NoVerdictFor) {
		return nil
	}
	last := "no exit code"
	if r.exit != nil {
		last = fmt.Sprintf("exit %d", *r.exit)
	}
	msg := fmt.Sprintf("%s gave no verdict in %d consecutive runs over %s, the last %s", r.script, nv.Runs, lasted, last)
	if out := excerpt(outputOf(r.exit, r.output), maxConditionOutput); out != "" {
		msg += ": " + out
	}
	msg += "; it no longer runs: spec.applied, spec.closed or spec.reanalysis moves the incident"
	inc.SetConditions(prv1.Condition{
		Type:               v1alpha1.TypeChecksUnknown,
		Status:             metav1.ConditionTrue,
		Reason:             noVerdictReason(r.script),
		Message:            msg,
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	})
	return []change{{reason: "ChecksUnknown", message: msg,
		kv: []any{"state", string(inc.Status.State), "script", string(r.script), "exit", last, "runs", nv.Runs}}}
}

// maxConditionOutput bounds the output excerpt in the ChecksUnknown message, in bytes.
const maxConditionOutput = 256

// excerpt is the start of s, cut to n bytes on a UTF-8 boundary.
func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// alertState is what reading the Alert in spec.alertRef found.
type alertState int

const (
	// alertUnread: it was not read, or the read failed for another reason than NotFound.
	alertUnread alertState = iota
	alertFound
	alertNotFound
)

// observeAlert applies what reading the incident's Alert found at now to an Open or Verifying
// incident. NotFound sets AlertGone True; once it has been True for cfg.AlertGoneGrace the
// incident is closed with resolution.by alert-gone. Found again sets it False.
func observeAlert(inc *v1alpha1.Incident, cfg Config, found alertState, now time.Time) []change {
	if s := inc.Status.State; s != v1alpha1.StateOpen && s != v1alpha1.StateVerifying {
		return nil
	}
	c := inc.GetCondition(v1alpha1.TypeAlertGone)
	ref := inc.Spec.AlertRef.Namespace + "/" + inc.Spec.AlertRef.Name
	switch found {
	case alertFound:
		if c.Status == metav1.ConditionTrue {
			inc.SetConditions(alertGoneCondition(metav1.ConditionFalse, v1alpha1.ReasonAlertFound, "Alert "+ref+" exists again", now))
		}
		return nil
	case alertNotFound:
	default:
		return nil
	}
	var changes []change
	if c.Status != metav1.ConditionTrue {
		c = alertGoneCondition(metav1.ConditionTrue, v1alpha1.ReasonAlertNotFound, "Alert "+ref+" does not exist", now)
		inc.SetConditions(c)
		changes = append(changes, change{reason: "AlertGone", message: "Alert " + ref + " does not exist",
			kv: []any{"state", string(inc.Status.State), "alert", ref}})
	}
	gone := now.Sub(c.LastTransitionTime.Time)
	if gone < cfg.AlertGoneGrace {
		return changes
	}
	changes = append(changes, setState(inc, v1alpha1.StateClosed, fmt.Sprintf("Alert %s does not exist (since %s)", ref, c.LastTransitionTime.UTC().Format(time.RFC3339)), now))
	inc.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByAlertGone, At: metav1.NewTime(now).Rfc3339Copy()}
	return changes
}

func alertGoneCondition(s metav1.ConditionStatus, reason prv1.ConditionReason, msg string, now time.Time) prv1.Condition {
	return prv1.Condition{
		Type:               v1alpha1.TypeAlertGone,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	}
}

// recordApply records a finished IncidentApply run on its incident as the latest apply result.
// Exit 0 moves an Open incident to Verifying, as spec.applied does, and restarts the settle window
// of a Verifying one; any other result moves nothing. Only Open and Verifying incidents record a
// run; the others are left alone. changed reports whether inc changed: recording the same run
// twice does not.
func recordApply(inc *v1alpha1.Incident, r result, by string) (changes []change, changed bool) {
	state := inc.Status.State
	if state != v1alpha1.StateOpen && state != v1alpha1.StateVerifying {
		return nil, false
	}
	before := inc.Status.DeepCopy()
	setLastCheck(inc, v1alpha1.ScriptApply, r.exit, "", r.at)
	if r.exit != nil && *r.exit == 0 {
		if state == v1alpha1.StateOpen {
			changes = append(changes, setState(inc, v1alpha1.StateVerifying, "apply exited 0, run by "+by, r.at))
		} else if v := inc.Status.VerifyingSince; v == nil || v.Before(ptr.To(metav1.NewTime(r.at).Rfc3339Copy())) {
			enterVerifying(inc, r.at)
		}
	}
	return changes, !equality.Semantic.DeepEqual(before, &inc.Status)
}

func reproducedCondition(s metav1.ConditionStatus, reason prv1.ConditionReason, msg string, now time.Time) prv1.Condition {
	return prv1.Condition{
		Type:               v1alpha1.TypeReproduced,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(now).Rfc3339Copy(),
	}
}

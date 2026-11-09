package incident

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/test"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

var (
	t0  = time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	cfg = Config{PollInterval: time.Minute, SettleWindow: 5 * time.Minute, CheckTimeout: time.Minute}
)

func at(d time.Duration) metav1.Time { return metav1.NewTime(t0.Add(d)) }

func exit(code int32) *int32 { return ptr.To(code) }

type incOpt func(*v1alpha1.Incident)

func newIncident(state v1alpha1.State, opts ...incOpt) *v1alpha1.Incident {
	inc := &v1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: "cd-not-ready-20260925-140000", Namespace: "krateo-system", UID: "0b7c3a3e-1d2f-4c56-9a8b-7e6f5d4c3b2a"},
		Spec:       v1alpha1.IncidentSpec{AlertRef: v1alpha1.AlertRef{Name: "cd-not-ready", Namespace: "krateo-system"}},
		Status: v1alpha1.IncidentStatus{
			State:    state,
			HowToFix: &v1alpha1.HowToFix{Precondition: "exit 1", Apply: "true", Verify: "exit 0"},
		},
	}
	for _, o := range opts {
		o(inc)
	}
	return inc
}

func withLastChecks(lc v1alpha1.LastChecks) incOpt {
	return func(inc *v1alpha1.Incident) { inc.Status.LastChecks = &lc }
}

func withVerifyingSince(d time.Duration) incOpt {
	return func(inc *v1alpha1.Incident) { inc.Status.VerifyingSince = ptr.To(at(d)) }
}

func withReproduced(s metav1.ConditionStatus) incOpt {
	return func(inc *v1alpha1.Incident) {
		inc.SetConditions(prv1.Condition{Type: v1alpha1.TypeReproduced, Status: s, Reason: "test", LastTransitionTime: at(0)})
	}
}

func withSpec(applied, closed bool) incOpt {
	return func(inc *v1alpha1.Incident) { inc.Spec.Applied, inc.Spec.Closed = applied, closed }
}

func withoutHowToFix(inc *v1alpha1.Incident) { inc.Status.HowToFix = nil }

// lc is a result with exit code, given since d after t0.
func lc(code *int32, d time.Duration) *v1alpha1.LastCheck {
	return &v1alpha1.LastCheck{Exit: code, Since: at(d)}
}

func withOutput(c *v1alpha1.LastCheck, out string) *v1alpha1.LastCheck {
	c.Output = out
	return c
}

// streakOf gives c a no-verdict streak of runs runs, from since to last after t0.
func streakOf(c *v1alpha1.LastCheck, runs int32, since, last time.Duration) *v1alpha1.LastCheck {
	c.NoVerdict = &v1alpha1.NoVerdict{Runs: runs, Since: at(since), Last: at(last)}
	return c
}

func printed(r result, out string) result {
	r.output = out
	return r
}

// want is the part of the status a transition decides.
type want struct {
	state          v1alpha1.State
	lastChecks     *v1alpha1.LastChecks
	verifyingSince *metav1.Time
	resolution     *v1alpha1.Resolution
	reproduced     metav1.ConditionStatus
}

func got(inc *v1alpha1.Incident) want {
	return want{
		state:          inc.Status.State,
		lastChecks:     inc.Status.LastChecks,
		verifyingSince: inc.Status.VerifyingSince,
		resolution:     inc.Status.Resolution,
		reproduced:     inc.GetCondition(v1alpha1.TypeReproduced).Status,
	}
}

func diff(t *testing.T, w, g want) {
	t.Helper()
	if d := cmp.Diff(w, g, cmp.AllowUnexported(want{}), test.EquateConditions()); d != "" {
		t.Errorf("status (-want +got):\n%s", d)
	}
}

func TestApplySpec(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	nowT := ptr.To(metav1.NewTime(now))
	closedBy := &v1alpha1.Resolution{By: v1alpha1.ResolvedByUser, At: metav1.NewTime(now)}
	pre1 := lc(exit(1), 0)
	applyNow := lc(nil, 10*time.Minute)

	cases := []struct {
		name         string
		inc          *v1alpha1.Incident
		want         want
		wantConsumed bool
	}{
		{name: "closed before the first status write", inc: newIncident("", withSpec(false, true)),
			want: want{state: v1alpha1.StateClosed, resolution: closedBy, reproduced: metav1.ConditionUnknown}},
		{name: "closed while Analyzing", inc: newIncident(v1alpha1.StateAnalyzing, withSpec(false, true)),
			want: want{state: v1alpha1.StateClosed, resolution: closedBy, reproduced: metav1.ConditionUnknown}},
		{name: "closed while Open", inc: newIncident(v1alpha1.StateOpen, withSpec(false, true), withLastChecks(v1alpha1.LastChecks{Precondition: pre1})),
			want: want{state: v1alpha1.StateClosed, lastChecks: &v1alpha1.LastChecks{Precondition: pre1}, resolution: closedBy, reproduced: metav1.ConditionUnknown}},
		{name: "closed while Verifying ends the settle window", inc: newIncident(v1alpha1.StateVerifying, withSpec(false, true), withVerifyingSince(0)),
			want: want{state: v1alpha1.StateClosed, resolution: closedBy, reproduced: metav1.ConditionUnknown}},
		{name: "closed after Resolved", inc: newIncident(v1alpha1.StateResolved, withSpec(false, true), func(i *v1alpha1.Incident) {
			i.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: at(0)}
		}), want: want{state: v1alpha1.StateClosed, resolution: closedBy, reproduced: metav1.ConditionUnknown}},
		{name: "closed wins over applied", inc: newIncident(v1alpha1.StateOpen, withSpec(true, true)),
			want: want{state: v1alpha1.StateClosed, resolution: closedBy, reproduced: metav1.ConditionUnknown}},
		{name: "applied moves Open to Verifying and records an apply result", inc: newIncident(v1alpha1.StateOpen, withSpec(true, false), withLastChecks(v1alpha1.LastChecks{Precondition: pre1})),
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Precondition: pre1, Apply: applyNow},
				verifyingSince: nowT, reproduced: metav1.ConditionUnknown}, wantConsumed: true},
		{name: "applied moves a flagged incident to Verifying", inc: newIncident(v1alpha1.StateOpen, withSpec(true, false), withReproduced(metav1.ConditionFalse)),
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Apply: applyNow},
				verifyingSince: nowT, reproduced: metav1.ConditionFalse}, wantConsumed: true},
		{name: "applied again while Verifying restarts the settle window", inc: newIncident(v1alpha1.StateVerifying, withSpec(true, false), withVerifyingSince(3*time.Minute),
			withLastChecks(v1alpha1.LastChecks{Apply: lc(nil, 3*time.Minute), Verify: lc(exit(1), 4*time.Minute)})),
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Apply: applyNow},
				verifyingSince: nowT, reproduced: metav1.ConditionUnknown}, wantConsumed: true},
		{name: "applied waits while Analyzing", inc: newIncident(v1alpha1.StateAnalyzing, withSpec(true, false)),
			want: want{state: v1alpha1.StateAnalyzing, reproduced: metav1.ConditionUnknown}},
		{name: "applied is ignored once Resolved", inc: newIncident(v1alpha1.StateResolved, withSpec(true, false), func(i *v1alpha1.Incident) {
			i.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: at(0)}
		}), want: want{state: v1alpha1.StateResolved, resolution: &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: at(0)}, reproduced: metav1.ConditionUnknown}},
		{name: "nothing to apply", inc: newIncident(v1alpha1.StateOpen),
			want: want{state: v1alpha1.StateOpen, reproduced: metav1.ConditionUnknown}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, consumed := applySpec(tc.inc, now)
			if consumed != tc.wantConsumed {
				t.Errorf("consumed = %v, want %v", consumed, tc.wantConsumed)
			}
			diff(t, tc.want, got(tc.inc))
		})
	}
}

func TestRecordPrecondition(t *testing.T) {
	now := t0.Add(time.Minute)
	run := func(code *int32) result {
		return result{script: v1alpha1.ScriptPrecondition, exit: code, at: t0.Add(time.Minute)}
	}
	ran := func(code *int32) *v1alpha1.LastChecks {
		return &v1alpha1.LastChecks{Precondition: lc(code, time.Minute)}
	}
	unknown := func(code *int32) *v1alpha1.LastChecks {
		return &v1alpha1.LastChecks{Precondition: streakOf(lc(code, time.Minute), 1, time.Minute, time.Minute)}
	}
	reproducedOpen := func(opts ...incOpt) *v1alpha1.Incident {
		return newIncident(v1alpha1.StateOpen, append([]incOpt{withReproduced(metav1.ConditionTrue)}, opts...)...)
	}

	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		r    result
		want want
	}{
		{name: "the first run exits 1: reproduced", inc: newIncident(v1alpha1.StateOpen), r: run(exit(1)),
			want: want{state: v1alpha1.StateOpen, lastChecks: ran(exit(1)), reproduced: metav1.ConditionTrue}},
		{name: "the first run exits 0: recorded as not reproduced, and Verifying", inc: newIncident(v1alpha1.StateOpen), r: run(exit(0)),
			want: want{state: v1alpha1.StateVerifying, lastChecks: ran(exit(0)), verifyingSince: ptr.To(at(time.Minute)), reproduced: metav1.ConditionFalse}},
		{name: "a first run with another exit is not the first verdict", inc: newIncident(v1alpha1.StateOpen), r: run(exit(2)),
			want: want{state: v1alpha1.StateOpen, lastChecks: unknown(exit(2)), reproduced: metav1.ConditionUnknown}},
		{name: "a first run that timed out is not the first verdict", inc: newIncident(v1alpha1.StateOpen), r: run(nil),
			want: want{state: v1alpha1.StateOpen, lastChecks: unknown(nil), reproduced: metav1.ConditionUnknown}},
		{name: "exit 0 once reproduced: Verifying", inc: reproducedOpen(), r: run(exit(0)),
			want: want{state: v1alpha1.StateVerifying, lastChecks: ran(exit(0)), verifyingSince: ptr.To(at(time.Minute)), reproduced: metav1.ConditionTrue}},
		{name: "exit 1 once reproduced: stays Open", inc: reproducedOpen(), r: run(exit(1)),
			want: want{state: v1alpha1.StateOpen, lastChecks: ran(exit(1)), reproduced: metav1.ConditionTrue}},
		{name: "another exit once reproduced: unknown, stays Open", inc: reproducedOpen(), r: run(exit(127)),
			want: want{state: v1alpha1.StateOpen, lastChecks: unknown(exit(127)), reproduced: metav1.ConditionTrue}},
		{name: "a timeout once reproduced: unknown, stays Open", inc: reproducedOpen(), r: run(nil),
			want: want{state: v1alpha1.StateOpen, lastChecks: unknown(nil), reproduced: metav1.ConditionTrue}},
		{name: "the same result again keeps its since", inc: reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)})), r: run(exit(1)),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Precondition: lc(exit(1), 0)}, reproduced: metav1.ConditionTrue}},
		{name: "a timeout again keeps its since and counts the run", inc: reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: streakOf(lc(nil, 0), 1, 0, 0)})), r: run(nil),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Precondition: streakOf(lc(nil, 0), 2, 0, time.Minute)}, reproduced: metav1.ConditionTrue}},
		{name: "an unknown result recorded before streaks were kept starts the streak at its since", inc: reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: lc(nil, 0)})), r: run(nil),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Precondition: streakOf(lc(nil, 0), 2, 0, time.Minute)}, reproduced: metav1.ConditionTrue}},
		{name: "a run already counted is not counted again", inc: reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: streakOf(lc(exit(2), 0), 3, 0, time.Minute)})), r: run(exit(2)),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Precondition: streakOf(lc(exit(2), 0), 3, 0, time.Minute)}, reproduced: metav1.ConditionTrue}},
		{name: "another unknown exit replaces the result and continues the streak", inc: reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: streakOf(lc(nil, 0), 2, -time.Minute, 0)})), r: run(exit(2)),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Precondition: streakOf(lc(exit(2), time.Minute), 3, -time.Minute, time.Minute)}, reproduced: metav1.ConditionTrue}},
		{name: "another result replaces it", inc: reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)})), r: run(exit(2)),
			want: want{state: v1alpha1.StateOpen, lastChecks: unknown(exit(2)), reproduced: metav1.ConditionTrue}},
		{name: "an unknown result keeps its output", inc: reproducedOpen(), r: printed(run(exit(2)), "  error: you must specify only one resource\n"),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Precondition: streakOf(withOutput(lc(exit(2), time.Minute), "error: you must specify only one resource"), 1, time.Minute, time.Minute)}, reproduced: metav1.ConditionTrue}},
		{name: "the same unknown result again keeps the first output", inc: reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: streakOf(withOutput(lc(exit(2), 0), "first"), 1, 0, 0)})), r: printed(run(exit(2)), "second"),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Precondition: streakOf(withOutput(lc(exit(2), 0), "first"), 2, 0, time.Minute)}, reproduced: metav1.ConditionTrue}},
		{name: "a verdict keeps no output", inc: reproducedOpen(), r: printed(run(exit(1)), "holds"),
			want: want{state: v1alpha1.StateOpen, lastChecks: ran(exit(1)), reproduced: metav1.ConditionTrue}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record(tc.inc, cfg, tc.r, now)
			diff(t, tc.want, got(tc.inc))
		})
	}
}

func TestRecordVerify(t *testing.T) {
	// The incident entered Verifying at t0 with a precondition exit 0.
	entered := lc(exit(0), 0)
	run := func(code *int32, d time.Duration) result {
		return result{script: v1alpha1.ScriptVerify, exit: code, at: t0.Add(d)}
	}
	verifying := func(opts ...incOpt) *v1alpha1.Incident {
		return newIncident(v1alpha1.StateVerifying, append([]incOpt{withReproduced(metav1.ConditionTrue), withVerifyingSince(0),
			withLastChecks(v1alpha1.LastChecks{Precondition: entered})}, opts...)...)
	}
	ran := func(code *int32, d time.Duration) *v1alpha1.LastChecks {
		return &v1alpha1.LastChecks{Precondition: entered, Verify: lc(code, d)}
	}
	resolvedAt := func(d time.Duration) *v1alpha1.Resolution {
		return &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: at(d)}
	}
	window := ptr.To(at(0))

	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		r    result
		want want
	}{
		{name: "exit 0: Resolved by verify", inc: verifying(), r: run(exit(0), time.Minute),
			want: want{state: v1alpha1.StateResolved, lastChecks: ran(exit(0), time.Minute), resolution: resolvedAt(time.Minute), reproduced: metav1.ConditionTrue}},
		{name: "exit 1 inside the settle window: retried", inc: verifying(), r: run(exit(1), time.Minute),
			want: want{state: v1alpha1.StateVerifying, lastChecks: ran(exit(1), time.Minute), verifyingSince: window, reproduced: metav1.ConditionTrue}},
		{name: "exit 1 just before the window ends: retried", inc: verifying(), r: run(exit(1), 5*time.Minute-time.Second),
			want: want{state: v1alpha1.StateVerifying, lastChecks: ran(exit(1), 5*time.Minute-time.Second), verifyingSince: window, reproduced: metav1.ConditionTrue}},
		{name: "exit 1 when the window ends: back to Open, without the precondition result that left it", inc: verifying(), r: run(exit(1), 5*time.Minute),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Verify: lc(exit(1), 5*time.Minute)}, reproduced: metav1.ConditionTrue}},
		{name: "exit 0 after the window: Resolved", inc: verifying(), r: run(exit(0), 9*time.Minute),
			want: want{state: v1alpha1.StateResolved, lastChecks: ran(exit(0), 9*time.Minute), resolution: resolvedAt(9 * time.Minute), reproduced: metav1.ConditionTrue}},
		{name: "another exit after the window: unknown, stays Verifying", inc: verifying(), r: run(exit(2), 9*time.Minute),
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Precondition: entered, Verify: streakOf(lc(exit(2), 9*time.Minute), 1, 9*time.Minute, 9*time.Minute)},
				verifyingSince: window, reproduced: metav1.ConditionTrue}},
		{name: "a timeout after the window: unknown, stays Verifying", inc: verifying(), r: run(nil, 9*time.Minute),
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Precondition: entered, Verify: streakOf(lc(nil, 9*time.Minute), 1, 9*time.Minute, 9*time.Minute)},
				verifyingSince: window, reproduced: metav1.ConditionTrue}},
		{name: "the window starts at verifyingSince", inc: verifying(withVerifyingSince(4 * time.Minute)), r: run(exit(1), 6*time.Minute),
			want: want{state: v1alpha1.StateVerifying, lastChecks: ran(exit(1), 6*time.Minute), verifyingSince: ptr.To(at(4 * time.Minute)), reproduced: metav1.ConditionTrue}},
		{name: "the same result again keeps its since", inc: verifying(withLastChecks(v1alpha1.LastChecks{Precondition: entered, Verify: lc(exit(1), time.Minute)})), r: run(exit(1), 2*time.Minute),
			want: want{state: v1alpha1.StateVerifying, lastChecks: ran(exit(1), time.Minute), verifyingSince: window, reproduced: metav1.ConditionTrue}},
		{name: "without a window, the run starts it", inc: verifying(func(i *v1alpha1.Incident) { i.Status.VerifyingSince = nil }), r: run(exit(1), 6*time.Minute),
			want: want{state: v1alpha1.StateVerifying, lastChecks: ran(exit(1), 6*time.Minute), verifyingSince: ptr.To(at(6 * time.Minute)), reproduced: metav1.ConditionTrue}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record(tc.inc, cfg, tc.r, tc.r.at)
			diff(t, tc.want, got(tc.inc))
		})
	}
}

func TestRecordApply(t *testing.T) {
	run := func(code *int32) result {
		return result{script: v1alpha1.ScriptApply, exit: code, at: t0.Add(time.Minute)}
	}
	applied := func(code *int32) *v1alpha1.LastCheck { return lc(code, time.Minute) }

	cases := []struct {
		name    string
		inc     *v1alpha1.Incident
		r       result
		changed bool
		want    want
	}{
		{name: "Open, exit 0: Verifying", inc: newIncident(v1alpha1.StateOpen), r: run(exit(0)), changed: true,
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Apply: applied(exit(0))}, verifyingSince: ptr.To(at(time.Minute))}},
		{name: "Open, exit 1: recorded, stays Open", inc: newIncident(v1alpha1.StateOpen), r: run(exit(1)), changed: true,
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Apply: applied(exit(1))}}},
		{name: "Open, timed out: recorded, stays Open", inc: newIncident(v1alpha1.StateOpen), r: run(nil), changed: true,
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Apply: applied(nil)}}},
		{name: "Verifying, exit 0: restarts the settle window", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(0),
			withLastChecks(v1alpha1.LastChecks{Verify: lc(exit(1), 30*time.Second)})), r: run(exit(0)), changed: true,
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Apply: applied(exit(0))}, verifyingSince: ptr.To(at(time.Minute))}},
		{name: "Verifying, exit 1: the window goes on", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(0)), r: run(exit(1)), changed: true,
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Apply: applied(exit(1))}, verifyingSince: ptr.To(at(0))}},
		{name: "already recorded", inc: newIncident(v1alpha1.StateOpen, withLastChecks(v1alpha1.LastChecks{Apply: applied(exit(1))})), r: run(exit(1)),
			want: want{state: v1alpha1.StateOpen, lastChecks: &v1alpha1.LastChecks{Apply: applied(exit(1))}}},
		{name: "a success already recorded", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(time.Minute),
			withLastChecks(v1alpha1.LastChecks{Apply: applied(exit(0)), Verify: lc(exit(1), 2*time.Minute)})), r: run(exit(0)),
			want: want{state: v1alpha1.StateVerifying, lastChecks: &v1alpha1.LastChecks{Apply: applied(exit(0)), Verify: lc(exit(1), 2*time.Minute)}, verifyingSince: ptr.To(at(time.Minute))}},
		{name: "Resolved: nothing", inc: newIncident(v1alpha1.StateResolved), r: run(exit(0)),
			want: want{state: v1alpha1.StateResolved}},
		{name: "Closed: nothing", inc: newIncident(v1alpha1.StateClosed), r: run(exit(0)),
			want: want{state: v1alpha1.StateClosed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, changed := recordApply(tc.inc, tc.r, "alice")
			if changed != tc.changed {
				t.Errorf("changed: want %v, got %v", tc.changed, changed)
			}
			tc.want.reproduced = metav1.ConditionUnknown
			diff(t, tc.want, got(tc.inc))
		})
	}
}

func TestCurrentScript(t *testing.T) {
	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		want v1alpha1.Script
	}{
		{name: "no state yet", inc: newIncident(""), want: ""},
		{name: "Analyzing", inc: newIncident(v1alpha1.StateAnalyzing), want: ""},
		{name: "Open", inc: newIncident(v1alpha1.StateOpen), want: v1alpha1.ScriptPrecondition},
		{name: "Open and reproduced", inc: newIncident(v1alpha1.StateOpen, withReproduced(metav1.ConditionTrue)), want: v1alpha1.ScriptPrecondition},
		{name: "Open and flagged by an earlier version: the precondition runs again", inc: newIncident(v1alpha1.StateOpen, withReproduced(metav1.ConditionFalse)), want: v1alpha1.ScriptPrecondition},
		{name: "Open with the precondition stopped", inc: newIncident(v1alpha1.StateOpen, stoppedOn(v1alpha1.ScriptPrecondition)), want: ""},
		{name: "Open with verify stopped: the precondition runs", inc: newIncident(v1alpha1.StateOpen, withChecksUnknown(v1alpha1.ReasonVerifyNoVerdict),
			withLastChecks(v1alpha1.LastChecks{Precondition: streakOf(lc(exit(2), 0), 1, 0, 0)})), want: v1alpha1.ScriptPrecondition},
		{name: "Open with a stale ChecksUnknown after a reanalysis cleared lastChecks", inc: newIncident(v1alpha1.StateOpen, withChecksUnknown(v1alpha1.ReasonPreconditionNoVerdict)), want: v1alpha1.ScriptPrecondition},
		{name: "Verifying with verify stopped", inc: newIncident(v1alpha1.StateVerifying, stoppedOn(v1alpha1.ScriptVerify)), want: ""},
		{name: "Open without scripts", inc: newIncident(v1alpha1.StateOpen, withoutHowToFix), want: ""},
		{name: "Open without a precondition", inc: newIncident(v1alpha1.StateOpen, func(i *v1alpha1.Incident) { i.Status.HowToFix.Precondition = "" }), want: ""},
		{name: "Verifying", inc: newIncident(v1alpha1.StateVerifying), want: v1alpha1.ScriptVerify},
		{name: "Verifying and flagged", inc: newIncident(v1alpha1.StateVerifying, withReproduced(metav1.ConditionFalse)), want: v1alpha1.ScriptVerify},
		{name: "Verifying without a verify script", inc: newIncident(v1alpha1.StateVerifying, func(i *v1alpha1.Incident) { i.Status.HowToFix.Verify = "" }), want: ""},
		{name: "Resolved", inc: newIncident(v1alpha1.StateResolved), want: ""},
		{name: "Closed", inc: newIncident(v1alpha1.StateClosed), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if g := currentScript(tc.inc); g != tc.want {
				t.Errorf("currentScript = %q, want %q", g, tc.want)
			}
		})
	}
}

func TestCheckDue(t *testing.T) {
	cases := []struct {
		name    string
		inc     *v1alpha1.Incident
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{name: "no check has run", inc: newIncident(v1alpha1.StateOpen), now: t0, want: true},
		{name: "inside the poll interval", inc: newIncident(v1alpha1.StateOpen), lastRun: t0, now: t0.Add(59 * time.Second), want: false},
		{name: "a poll interval after the last check", inc: newIncident(v1alpha1.StateOpen), lastRun: t0, now: t0.Add(time.Minute), want: true},
		{name: "inside the poll interval from entering Verifying", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(30*time.Second)),
			lastRun: t0, now: t0.Add(time.Minute), want: false},
		{name: "a poll interval after entering Verifying", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(30*time.Second)),
			lastRun: t0, now: t0.Add(90 * time.Second), want: true},
		{name: "no script to run", inc: newIncident(v1alpha1.StateAnalyzing), now: t0.Add(time.Hour), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if g := checkDue(tc.inc, cfg, tc.lastRun, tc.now); g != tc.want {
				t.Errorf("checkDue = %v, want %v", g, tc.want)
			}
		})
	}
}

// TestLifecycle walks one incident through every script-driven edge.
func TestLifecycle(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen)
	step := func(r result, wantState v1alpha1.State) {
		t.Helper()
		record(inc, cfg, r, r.at)
		if inc.Status.State != wantState {
			t.Fatalf("after %s exit %v at %v: state %s, want %s", r.script, r.exit, r.at.Sub(t0), inc.Status.State, wantState)
		}
	}
	pre := func(code int32, d time.Duration) result {
		return result{script: v1alpha1.ScriptPrecondition, exit: exit(code), at: t0.Add(d)}
	}
	ver := func(code int32, d time.Duration) result {
		return result{script: v1alpha1.ScriptVerify, exit: exit(code), at: t0.Add(d)}
	}

	step(pre(1, 0), v1alpha1.StateOpen) // first run: reproduced
	step(pre(1, time.Minute), v1alpha1.StateOpen)
	if s := inc.Status.LastChecks.Precondition.Since; !s.Equal(ptr.To(at(0))) {
		t.Fatalf("precondition since = %v, want the first of the runs that exited 1", s)
	}
	step(pre(0, 2*time.Minute), v1alpha1.StateVerifying)
	step(ver(1, 3*time.Minute), v1alpha1.StateVerifying) // inside the settle window
	step(ver(1, 7*time.Minute), v1alpha1.StateOpen)      // window over: the fix did not hold

	inc.Spec.Applied = true
	if _, consumed := applySpec(inc, t0.Add(8*time.Minute)); !consumed || inc.Status.State != v1alpha1.StateVerifying {
		t.Fatalf("spec.applied: consumed %v, state %s", consumed, inc.Status.State)
	}
	step(ver(0, 9*time.Minute), v1alpha1.StateResolved)
	if r := inc.Status.Resolution; r == nil || r.By != v1alpha1.ResolvedByVerify {
		t.Fatalf("resolution = %+v, want by verify", r)
	}
	if inc.Status.VerifyingSince != nil {
		t.Fatalf("verifyingSince = %v, want unset once Resolved", inc.Status.VerifyingSince)
	}
}

func TestOutputOf(t *testing.T) {
	long := strings.Repeat("a", maxOutput-1) + "é" + "tail"
	cases := []struct {
		name string
		exit *int32
		out  string
		want string
	}{
		{name: "a verdict keeps nothing", exit: exit(0), out: "gone", want: ""},
		{name: "an unknown exit keeps the output, trimmed", exit: exit(2), out: "\n jq: error \n", want: "jq: error"},
		{name: "no exit keeps the reason", exit: nil, out: "timed out", want: "timed out"},
		{name: "a long output is cut on a rune boundary", exit: exit(2), out: long, want: strings.Repeat("a", maxOutput-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outputOf(tc.exit, tc.out); got != tc.want {
				t.Errorf("outputOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// withChecksUnknown sets ChecksUnknown True with reason.
func withChecksUnknown(reason prv1.ConditionReason) incOpt {
	return func(inc *v1alpha1.Incident) {
		inc.SetConditions(prv1.Condition{Type: v1alpha1.TypeChecksUnknown, Status: metav1.ConditionTrue, Reason: reason, LastTransitionTime: at(0)})
	}
}

// stoppedOn is an incident whose script s hit the no-verdict bound.
func stoppedOn(s v1alpha1.Script) incOpt {
	return func(inc *v1alpha1.Incident) {
		withChecksUnknown(noVerdictReason(s))(inc)
		if inc.Status.LastChecks == nil {
			inc.Status.LastChecks = &v1alpha1.LastChecks{}
		}
		*slot(inc.Status.LastChecks, s) = streakOf(lc(exit(2), 0), 6, 0, 5*time.Minute)
	}
}

func withAlertGone(d time.Duration) incOpt {
	return func(inc *v1alpha1.Incident) {
		inc.SetConditions(prv1.Condition{Type: v1alpha1.TypeAlertGone, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonAlertNotFound, LastTransitionTime: at(d)})
	}
}

// run is a finished run of s that ended d after t0; a nil code is a timeout.
type run struct {
	code *int32
	d    time.Duration
}

// TestNoVerdictBound feeds runs of the current script, one by one, as Observe does: only while the
// script is current. It checks after each run whether ChecksUnknown is set.
func TestNoVerdictBound(t *testing.T) {
	bounded := cfg
	bounded.NoVerdictRuns, bounded.NoVerdictFor = 3, time.Hour
	byTime := cfg
	byTime.NoVerdictFor = time.Hour
	verifying := func() *v1alpha1.Incident {
		return newIncident(v1alpha1.StateVerifying, withReproduced(metav1.ConditionTrue), withVerifyingSince(0))
	}
	reproducedOpen := func(opts ...incOpt) *v1alpha1.Incident {
		return newIncident(v1alpha1.StateOpen, append([]incOpt{withReproduced(metav1.ConditionTrue)}, opts...)...)
	}
	m := time.Minute

	cases := []struct {
		name string
		cfg  Config
		inc  *v1alpha1.Incident
		runs []run
		// unknownAfter is the 1-based run after which ChecksUnknown must be True; 0 for never.
		unknownAfter int
		wantReason   prv1.ConditionReason
		wantRuns     int32 // the streak after the last run; 0 for none
		wantState    v1alpha1.State
	}{
		{name: "N unknown runs in a row stop the precondition", cfg: bounded, inc: reproducedOpen(),
			runs:         []run{{exit(2), m}, {exit(2), 2 * m}, {nil, 3 * m}},
			unknownAfter: 3, wantReason: v1alpha1.ReasonPreconditionNoVerdict, wantRuns: 3, wantState: v1alpha1.StateOpen},
		{name: "a verdict resets the streak", cfg: bounded, inc: reproducedOpen(),
			runs:     []run{{exit(2), m}, {exit(2), 2 * m}, {exit(1), 3 * m}, {exit(2), 4 * m}, {exit(2), 5 * m}},
			wantRuns: 2, wantState: v1alpha1.StateOpen},
		{name: "after a reset the bound counts from the verdict", cfg: bounded, inc: reproducedOpen(),
			runs:         []run{{exit(2), m}, {exit(2), 2 * m}, {exit(1), 3 * m}, {exit(2), 4 * m}, {exit(2), 5 * m}, {exit(2), 6 * m}},
			unknownAfter: 6, wantReason: v1alpha1.ReasonPreconditionNoVerdict, wantRuns: 3, wantState: v1alpha1.StateOpen},
		{name: "an unknown streak lasting T stops the precondition", cfg: byTime, inc: reproducedOpen(),
			runs:         []run{{exit(2), 0}, {exit(2), 30 * m}, {exit(2), 59 * m}, {exit(2), 60 * m}},
			unknownAfter: 4, wantReason: v1alpha1.ReasonPreconditionNoVerdict, wantRuns: 4, wantState: v1alpha1.StateOpen},
		{name: "an unknown result left by an earlier version counts from its since", cfg: byTime,
			inc:          reproducedOpen(withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(2), -48*time.Hour)})),
			runs:         []run{{exit(2), m}},
			unknownAfter: 1, wantReason: v1alpha1.ReasonPreconditionNoVerdict, wantRuns: 2, wantState: v1alpha1.StateOpen},
		{name: "unknown verify runs stop verify, still Verifying", cfg: bounded, inc: verifying(),
			runs:         []run{{nil, m}, {exit(2), 2 * m}, {exit(127), 3 * m}},
			unknownAfter: 3, wantReason: v1alpha1.ReasonVerifyNoVerdict, wantRuns: 3, wantState: v1alpha1.StateVerifying},
		{name: "a verify streak lasting T stops verify", cfg: byTime, inc: verifying(),
			runs:         []run{{nil, m}, {nil, 61 * m}},
			unknownAfter: 2, wantReason: v1alpha1.ReasonVerifyNoVerdict, wantRuns: 2, wantState: v1alpha1.StateVerifying},
		{name: "no bounds: unknown runs never stop", cfg: cfg, inc: reproducedOpen(),
			runs:     []run{{exit(2), m}, {exit(2), 2 * m}, {exit(2), 3 * m}, {exit(2), 48 * time.Hour}},
			wantRuns: 4, wantState: v1alpha1.StateOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc := tc.inc
			for i, r := range tc.runs {
				s := currentScript(inc)
				if s == "" {
					t.Fatalf("run %d: no current script", i+1)
				}
				record(inc, tc.cfg, printed(result{script: s, exit: r.code, at: t0.Add(r.d)}, "error: the server doesn't have a resource type \"nodes\""), t0.Add(r.d))
				syncChecksUnknown(inc, t0.Add(r.d))
				c := inc.GetCondition(v1alpha1.TypeChecksUnknown)
				if wantTrue := tc.unknownAfter != 0 && i+1 >= tc.unknownAfter; (c.Status == metav1.ConditionTrue) != wantTrue {
					t.Fatalf("after run %d: ChecksUnknown = %s, want True: %v", i+1, c.Status, wantTrue)
				}
			}
			c := inc.GetCondition(v1alpha1.TypeChecksUnknown)
			if tc.unknownAfter != 0 {
				if c.Reason != tc.wantReason {
					t.Errorf("ChecksUnknown reason = %s, want %s", c.Reason, tc.wantReason)
				}
				if !strings.Contains(c.Message, `doesn't have a resource type "nodes"`) {
					t.Errorf("ChecksUnknown message %q lacks the output excerpt", c.Message)
				}
				if s := currentScript(inc); s != "" {
					t.Errorf("currentScript = %q once stopped, want none", s)
				}
			}
			if inc.Status.State != tc.wantState {
				t.Errorf("state = %s, want %s", inc.Status.State, tc.wantState)
			}
			var gotRuns int32
			if last := *slot(inc.Status.LastChecks, scriptOf(inc.Status.State)); last != nil && last.NoVerdict != nil {
				gotRuns = last.NoVerdict.Runs
			}
			if gotRuns != tc.wantRuns {
				t.Errorf("streak runs = %d, want %d", gotRuns, tc.wantRuns)
			}
		})
	}
}

func TestNoVerdictMessage(t *testing.T) {
	c := cfg
	c.NoVerdictRuns = 1
	inc := newIncident(v1alpha1.StateOpen)
	changes := record(inc, c, printed(result{script: v1alpha1.ScriptPrecondition, exit: exit(2), at: t0}, strings.Repeat("x", 600)), t0)
	cond := inc.GetCondition(v1alpha1.TypeChecksUnknown)
	if cond.Status != metav1.ConditionTrue || len(changes) != 1 || changes[0].reason != "ChecksUnknown" {
		t.Fatalf("condition %+v, changes %+v: want ChecksUnknown True and one ChecksUnknown change", cond, changes)
	}
	want := "precondition gave no verdict in 1 consecutive runs over 0s, the last exit 2: " + strings.Repeat("x", maxConditionOutput) +
		"; it no longer runs: spec.applied, spec.closed or spec.reanalysis moves the incident"
	if cond.Message != want {
		t.Errorf("message:\n got %q\nwant %q", cond.Message, want)
	}
}

func TestSyncChecksUnknown(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		// before runs before the sync, as Observe applies spec first.
		before   func(*v1alpha1.Incident)
		want     metav1.ConditionStatus
		wantNext v1alpha1.Script
	}{
		{name: "a stopped precondition stays stopped", inc: newIncident(v1alpha1.StateOpen, stoppedOn(v1alpha1.ScriptPrecondition)),
			want: metav1.ConditionTrue, wantNext: ""},
		{name: "a stopped verify stays stopped", inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(0), stoppedOn(v1alpha1.ScriptVerify)),
			want: metav1.ConditionTrue, wantNext: ""},
		{name: "spec.applied moves a stopped Open incident to Verifying, and verify runs", inc: newIncident(v1alpha1.StateOpen, withSpec(true, false), stoppedOn(v1alpha1.ScriptPrecondition)),
			before: func(i *v1alpha1.Incident) { applySpec(i, now) }, want: metav1.ConditionFalse, wantNext: v1alpha1.ScriptVerify},
		{name: "spec.applied again restarts a stopped verify", inc: newIncident(v1alpha1.StateVerifying, withSpec(true, false), withVerifyingSince(0), stoppedOn(v1alpha1.ScriptVerify)),
			before: func(i *v1alpha1.Incident) { applySpec(i, now) }, want: metav1.ConditionFalse, wantNext: v1alpha1.ScriptVerify},
		{name: "spec.closed clears it", inc: newIncident(v1alpha1.StateOpen, withSpec(false, true), stoppedOn(v1alpha1.ScriptPrecondition)),
			before: func(i *v1alpha1.Incident) { applySpec(i, now) }, want: metav1.ConditionFalse, wantNext: ""},
		{name: "a reanalysis clears lastChecks: the new precondition runs", inc: newIncident(v1alpha1.StateOpen, withChecksUnknown(v1alpha1.ReasonPreconditionNoVerdict)),
			want: metav1.ConditionFalse, wantNext: v1alpha1.ScriptPrecondition},
		{name: "back in Analyzing for a reanalysis", inc: newIncident(v1alpha1.StateAnalyzing, stoppedOn(v1alpha1.ScriptPrecondition)),
			want: metav1.ConditionFalse, wantNext: ""},
		{name: "never set: stays absent", inc: newIncident(v1alpha1.StateOpen),
			want: metav1.ConditionUnknown, wantNext: v1alpha1.ScriptPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.before != nil {
				tc.before(tc.inc)
			}
			syncChecksUnknown(tc.inc, now)
			c := tc.inc.GetCondition(v1alpha1.TypeChecksUnknown)
			if c.Status != tc.want {
				t.Errorf("ChecksUnknown = %s, want %s", c.Status, tc.want)
			}
			if tc.want == metav1.ConditionFalse && c.Reason != v1alpha1.ReasonChecksResumed {
				t.Errorf("reason = %s, want %s", c.Reason, v1alpha1.ReasonChecksResumed)
			}
			if s := currentScript(tc.inc); s != tc.wantNext {
				t.Errorf("currentScript = %q, want %q", s, tc.wantNext)
			}
		})
	}
}

func TestObserveAlert(t *testing.T) {
	now := t0.Add(time.Hour)
	graced := cfg
	graced.AlertGoneGrace = 10 * time.Minute
	closedAt := &v1alpha1.Resolution{By: v1alpha1.ResolvedByAlertGone, At: metav1.NewTime(now)}

	cases := []struct {
		name           string
		cfg            Config
		inc            *v1alpha1.Incident
		found          alertState
		wantState      v1alpha1.State
		wantAlertGone  metav1.ConditionStatus
		wantResolution *v1alpha1.Resolution
		wantChanges    []string
	}{
		{name: "Open, NotFound: AlertGone, still Open inside the grace", cfg: graced, inc: newIncident(v1alpha1.StateOpen), found: alertNotFound,
			wantState: v1alpha1.StateOpen, wantAlertGone: metav1.ConditionTrue, wantChanges: []string{"AlertGone"}},
		{name: "Open, NotFound, gone for less than the grace: still Open", cfg: graced, inc: newIncident(v1alpha1.StateOpen, withAlertGone(time.Hour-9*time.Minute)), found: alertNotFound,
			wantState: v1alpha1.StateOpen, wantAlertGone: metav1.ConditionTrue},
		{name: "Open, NotFound for the grace: Closed by alert-gone", cfg: graced, inc: newIncident(v1alpha1.StateOpen, withAlertGone(time.Hour-10*time.Minute)), found: alertNotFound,
			wantState: v1alpha1.StateClosed, wantAlertGone: metav1.ConditionTrue, wantResolution: closedAt, wantChanges: []string{"StateChanged"}},
		{name: "Verifying, NotFound for the grace: Closed by alert-gone", cfg: graced,
			inc: newIncident(v1alpha1.StateVerifying, withVerifyingSince(0), withAlertGone(0)), found: alertNotFound,
			wantState: v1alpha1.StateClosed, wantAlertGone: metav1.ConditionTrue, wantResolution: closedAt, wantChanges: []string{"StateChanged"}},
		{name: "no grace: Closed at the first NotFound", cfg: cfg, inc: newIncident(v1alpha1.StateOpen), found: alertNotFound,
			wantState: v1alpha1.StateClosed, wantAlertGone: metav1.ConditionTrue, wantResolution: closedAt, wantChanges: []string{"AlertGone", "StateChanged"}},
		{name: "a stopped incident is closed too", cfg: cfg, inc: newIncident(v1alpha1.StateOpen, stoppedOn(v1alpha1.ScriptPrecondition)), found: alertNotFound,
			wantState: v1alpha1.StateClosed, wantAlertGone: metav1.ConditionTrue, wantResolution: closedAt, wantChanges: []string{"AlertGone", "StateChanged"}},
		{name: "found again: AlertGone False", cfg: graced, inc: newIncident(v1alpha1.StateOpen, withAlertGone(time.Hour-5*time.Minute)), found: alertFound,
			wantState: v1alpha1.StateOpen, wantAlertGone: metav1.ConditionFalse},
		{name: "found: nothing", cfg: graced, inc: newIncident(v1alpha1.StateOpen), found: alertFound,
			wantState: v1alpha1.StateOpen, wantAlertGone: metav1.ConditionUnknown},
		{name: "unread: nothing", cfg: cfg, inc: newIncident(v1alpha1.StateOpen, withAlertGone(0)), found: alertUnread,
			wantState: v1alpha1.StateOpen, wantAlertGone: metav1.ConditionTrue},
		{name: "Analyzing: left to the writer", cfg: cfg, inc: newIncident(v1alpha1.StateAnalyzing), found: alertNotFound,
			wantState: v1alpha1.StateAnalyzing, wantAlertGone: metav1.ConditionUnknown},
		{name: "Resolved: over", cfg: cfg, inc: newIncident(v1alpha1.StateResolved, func(i *v1alpha1.Incident) {
			i.Status.Resolution = &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: at(0)}
		}), found: alertNotFound, wantState: v1alpha1.StateResolved, wantAlertGone: metav1.ConditionUnknown,
			wantResolution: &v1alpha1.Resolution{By: v1alpha1.ResolvedByVerify, At: at(0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes := observeAlert(tc.inc, tc.cfg, tc.found, now)
			if tc.inc.Status.State != tc.wantState {
				t.Errorf("state = %s, want %s", tc.inc.Status.State, tc.wantState)
			}
			if s := tc.inc.GetCondition(v1alpha1.TypeAlertGone).Status; s != tc.wantAlertGone {
				t.Errorf("AlertGone = %s, want %s", s, tc.wantAlertGone)
			}
			if d := cmp.Diff(tc.wantResolution, tc.inc.Status.Resolution); d != "" {
				t.Errorf("resolution (-want +got):\n%s", d)
			}
			var reasons []string
			for _, c := range changes {
				reasons = append(reasons, c.reason)
			}
			if d := cmp.Diff(tc.wantChanges, reasons); d != "" {
				t.Errorf("changes (-want +got):\n%s", d)
			}
			if tc.wantState == v1alpha1.StateClosed && currentScript(tc.inc) != "" {
				t.Errorf("a Closed incident runs %q", currentScript(tc.inc))
			}
		})
	}
}

// TestFirstRunExitsZero: the problem was gone by the first precondition run. The incident goes to
// Verifying, keeps Reproduced=False as a record, and verify decides.
func TestFirstRunExitsZero(t *testing.T) {
	pre := func(code int32, d time.Duration) result {
		return result{script: v1alpha1.ScriptPrecondition, exit: exit(code), at: t0.Add(d)}
	}
	ver := func(code int32, d time.Duration) result {
		return result{script: v1alpha1.ScriptVerify, exit: exit(code), at: t0.Add(d)}
	}
	step := func(t *testing.T, inc *v1alpha1.Incident, r result, wantState v1alpha1.State) {
		t.Helper()
		if s := currentScript(inc); s != r.script {
			t.Fatalf("before %s at %v: current script %q", r.script, r.at.Sub(t0), s)
		}
		record(inc, cfg, r, r.at)
		if inc.Status.State != wantState {
			t.Fatalf("after %s exit %d at %v: state %s, want %s", r.script, *r.exit, r.at.Sub(t0), inc.Status.State, wantState)
		}
		if s := inc.GetCondition(v1alpha1.TypeReproduced).Status; s != metav1.ConditionFalse {
			t.Fatalf("after %s at %v: Reproduced = %s, want False kept as a record", r.script, r.at.Sub(t0), s)
		}
	}

	t.Run("verify exits 0: Resolved", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen)
		step(t, inc, pre(0, 0), v1alpha1.StateVerifying)
		step(t, inc, ver(0, time.Minute), v1alpha1.StateResolved)
		if r := inc.Status.Resolution; r == nil || r.By != v1alpha1.ResolvedByVerify {
			t.Fatalf("resolution = %+v, want by verify", r)
		}
	})
	t.Run("verify exits 1 after the settle window: Open, and the precondition runs again", func(t *testing.T) {
		inc := newIncident(v1alpha1.StateOpen)
		step(t, inc, pre(0, 0), v1alpha1.StateVerifying)
		step(t, inc, ver(1, time.Minute), v1alpha1.StateVerifying) // inside the window
		step(t, inc, ver(1, 6*time.Minute), v1alpha1.StateOpen)
		step(t, inc, pre(1, 7*time.Minute), v1alpha1.StateOpen)
		step(t, inc, pre(0, 8*time.Minute), v1alpha1.StateVerifying)
	})
}

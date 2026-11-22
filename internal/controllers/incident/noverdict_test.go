package incident

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// jqScopingStderr is what a verify seen on a live cluster printed on every run: in
// `(.status.packageUrl // "") | endswith($col + (.spec.chart.version // ""))` the `.` inside the
// pipe is the packageUrl string.
const jqScopingStderr = `jq: error (at <stdin>:136): Cannot index string with string "spec"`

func TestNoVerdictOnFirstRun(t *testing.T) {
	cases := []struct {
		name  string
		exit  *int32
		first bool
		want  bool
	}{
		{"first run, exit 2", exit(2), true, true},
		{"first run, exit 127 (command not found)", exit(127), true, true},
		{"first run, exit 0", exit(0), true, false},
		{"first run, exit 1", exit(1), true, false},
		{"first run without an exit code (a timeout) is left to the streak", nil, true, false},
		{"not the first run", exit(2), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := noVerdictOnFirstRun(result{script: v1alpha1.ScriptVerify, exit: c.exit, output: "x"}, c.first)
			if (got != nil) != c.want {
				t.Errorf("got %+v, want a request %t", got, c.want)
			}
		})
	}
}

func TestNoVerdictReanalysisDue(t *testing.T) {
	cases := []struct {
		name string
		inc  *v1alpha1.Incident
		want bool
	}{
		{"first pass of an Open incident", newIncident(v1alpha1.StateOpen, withFirstPass()), true},
		{"Verifying too", newIncident(v1alpha1.StateVerifying, withFirstPass()), true},
		{"already asked in this analysis", newIncident(v1alpha1.StateOpen), false},
		{"a later analysis asks again", newIncident(v1alpha1.StateOpen, func(i *v1alpha1.Incident) {
			i.Status.Reanalysis = &v1alpha1.Reanalysis{Count: 1}
		}), true},
		{"the writer would not answer (Analyzing)", newIncident(v1alpha1.StateAnalyzing, withFirstPass()), false},
		{"a request is pending", newIncident(v1alpha1.StateOpen, withFirstPass(), func(i *v1alpha1.Incident) {
			i.Spec.Reanalysis = &v1alpha1.ReanalysisRequest{Cause: v1alpha1.ReanalysisCausePerson}
		}), false},
		{"budget spent", newIncident(v1alpha1.StateOpen, withFirstPass(), func(i *v1alpha1.Incident) {
			i.Annotations[v1alpha1.AnnotationNoVerdictReanalyses] = "3"
		}), false},
		{"a count that does not parse is the budget spent", newIncident(v1alpha1.StateOpen, withFirstPass(), func(i *v1alpha1.Incident) {
			i.Annotations[v1alpha1.AnnotationNoVerdictReanalyses] = "lots"
		}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := noVerdictReanalysisDue(c.inc); got != c.want {
				t.Errorf("due = %t, want %t", got, c.want)
			}
		})
	}
}

func TestNoVerdictFeedback(t *testing.T) {
	got := noVerdictFeedback(firstNoVerdict{script: v1alpha1.ScriptVerify, exit: 2, output: jqScopingStderr + "\n"})
	want := "verify exited 2 on its first run, so it can never give a verdict (0 or 1): " + jqScopingStderr
	if got != want {
		t.Errorf("feedback\n got %q\nwant %q", got, want)
	}
	long := noVerdictFeedback(firstNoVerdict{script: v1alpha1.ScriptPrecondition, exit: 2, output: strings.Repeat("é", 3000)})
	if len(long) > maxFeedback {
		t.Errorf("feedback is %d bytes, over the field's %d", len(long), maxFeedback)
	}
}

// patching is a kube that records every merge patch.
func patching(k *kube) *[]map[string]any {
	var patches []map[string]any
	k.MockPatch = func(_ context.Context, _ client.Object, p client.Patch, _ ...client.PatchOption) error {
		b, _ := p.Data(nil)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		patches = append(patches, m)
		return nil
	}
	return &patches
}

func requestOf(t *testing.T, patches []map[string]any) map[string]any {
	t.Helper()
	for _, p := range patches {
		if spec, ok := p["spec"].(map[string]any); ok {
			if r, ok := spec["reanalysis"].(map[string]any); ok {
				return r
			}
		}
	}
	return nil
}

// A precondition whose first run exits 2 is sent back at once, with its output, and no further
// check or probe of that analysis starts.
func TestAFirstRunWithoutAVerdictRequestsAReanalysis(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen, withFirstPass())
	inc.ResourceVersion = "7"
	pod := checkPod(inc, v1alpha1.ScriptPrecondition, t0, exit(2), t0.Add(10*time.Second))
	pod.Status.ContainerStatuses[0].State.Terminated.Message = "error: the server doesn't have a resource type \"customresourcedefinitions\""
	k := newKube(pod)
	patches := patching(k)
	e := newExternal(k, t0.Add(time.Minute))
	if _, err := e.Observe(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if err := e.Update(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	req := requestOf(t, *patches)
	if req == nil {
		t.Fatalf("no reanalysis requested; patches %v", *patches)
	}
	if req["cause"] != string(v1alpha1.ReanalysisCauseCheckNoVerdict) {
		t.Errorf("cause %v", req["cause"])
	}
	if fb, _ := req["feedback"].(string); !strings.HasPrefix(fb, "precondition exited 2 on its first run") ||
		!strings.Contains(fb, "doesn't have a resource type") {
		t.Errorf("feedback %q", fb)
	}
	ann, _ := (*patches)[len(*patches)-1]["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[v1alpha1.AnnotationNoVerdictReanalysis] != "0" || ann[v1alpha1.AnnotationNoVerdictReanalyses] != "1" {
		t.Errorf("request record %v", ann)
	}
	for _, c := range k.calls {
		if strings.HasPrefix(c, "create pod") {
			t.Errorf("a check or probe started after the request: %v", k.calls)
		}
	}
}

// The same run is not sent back once the analysis has been asked for, nor when it is not the
// script's first run: the no-verdict streak counts it, as before.
func TestNoRequestOutsideTheFirstRun(t *testing.T) {
	for name, inc := range map[string]*v1alpha1.Incident{
		"already asked":     newIncident(v1alpha1.StateOpen),
		"not the first run": newIncident(v1alpha1.StateOpen, withFirstPass(), withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)})),
	} {
		t.Run(name, func(t *testing.T) {
			pod := checkPod(inc, v1alpha1.ScriptPrecondition, t0.Add(time.Hour), exit(2), t0.Add(time.Hour+10*time.Second))
			k := newKube(pod)
			patches := patching(k)
			e := newExternal(k, t0.Add(time.Hour+time.Minute))
			if _, err := e.Observe(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			if err := e.Update(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			if req := requestOf(t, *patches); req != nil {
				t.Errorf("requested %v", req)
			}
		})
	}
}

// probePod is inc's probe pod, finished with code (running when nil).
func probePod(inc *v1alpha1.Incident, code *int32, output string) corev1.Pod {
	p := pods.ProbePod(inc)
	p.CreationTimestamp = metav1.NewTime(t0)
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = ptr.To(metav1.NewTime(t0))
	if code != nil {
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: *code, FinishedAt: metav1.NewTime(t0.Add(10 * time.Second)), Message: output},
		}}}
	}
	return *p
}

// An Open incident's first pass runs verify once as the probe, beside its precondition.
func TestTheFirstPassStartsTheVerifyProbe(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen, withFirstPass(), withReproduced(metav1.ConditionTrue),
		withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)}))
	k := newKube()
	e := newExternal(k, t0.Add(10*time.Minute))
	if _, err := e.Observe(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if err := e.Update(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	name := "check-0b7c3a3e1d2f-verify-0-probe-0"
	var probe, cm bool
	for _, c := range k.calls {
		probe = probe || c == "create pod "+name
		cm = cm || c == "create configmap "+name
	}
	if !probe || !cm {
		t.Errorf("probe pod %t, ConfigMap %t: %v", probe, cm, k.calls)
	}
	if !isProbe(ptr.To(pods.ProbePod(inc))) {
		t.Error("the probe pod is not labelled a probe")
	}
}

// A finished probe is recorded and deleted. Its exit 2 is sent back with verify's output, as for
// the live cluster's jq-scoping verify; its exit 0 or 1 only records it, and never moves the state.
func TestAFinishedProbe(t *testing.T) {
	cases := []struct {
		name    string
		code    int32
		request bool
	}{
		{"exit 2: sent back", 2, true},
		{"exit 0: recorded only, and not Resolved", 0, false},
		{"exit 1: recorded only", 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inc := newIncident(v1alpha1.StateOpen, withFirstPass(), withReproduced(metav1.ConditionTrue),
				withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)}))
			inc.ResourceVersion = "7"
			k := newKube(probePod(inc, exit(c.code), jqScopingStderr))
			patches := patching(k)
			e := newExternal(k, t0.Add(time.Minute))
			if _, err := e.Observe(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			if err := e.Update(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			if inc.Status.State != v1alpha1.StateOpen {
				t.Errorf("the probe moved the incident to %s", inc.Status.State)
			}
			if inc.Status.LastChecks.Verify != nil {
				t.Errorf("the probe recorded a verify result %+v", inc.Status.LastChecks.Verify)
			}
			probed := false
			for _, p := range *patches {
				if ann, ok := p["metadata"].(map[string]any)["annotations"].(map[string]any); ok && ann[v1alpha1.AnnotationVerifyProbed] == "0" {
					probed = true
				}
			}
			if !probed {
				t.Errorf("the probe was not recorded: %v", *patches)
			}
			deleted := false
			for _, call := range k.calls {
				deleted = deleted || call == "delete pod check-0b7c3a3e1d2f-verify-0-probe-0"
			}
			if !deleted {
				t.Errorf("the finished probe pod was kept: %v", k.calls)
			}
			req := requestOf(t, *patches)
			if (req != nil) != c.request {
				t.Fatalf("request %v, want one %t", req, c.request)
			}
			if c.request {
				if fb, _ := req["feedback"].(string); !strings.HasPrefix(fb, "verify exited 2 on its first run") || !strings.Contains(fb, jqScopingStderr) {
					t.Errorf("feedback %q", fb)
				}
			}
		})
	}
}

// A running probe is left alone, and no second one starts; a probe of an earlier analysis is
// deleted unread.
func TestProbeLifecycle(t *testing.T) {
	inc := newIncident(v1alpha1.StateOpen, withFirstPass(), withReproduced(metav1.ConditionTrue),
		withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)}))
	k := newKube(probePod(inc, nil, ""))
	e := newExternal(k, t0.Add(10*time.Second))
	if _, err := e.Observe(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if !e.plan.probing || e.plan.probe || len(e.plan.stale) != 0 {
		t.Errorf("running probe: probing %t, probe %t, stale %d", e.plan.probing, e.plan.probe, len(e.plan.stale))
	}

	old := probePod(inc, exit(2), jqScopingStderr)
	later := newIncident(v1alpha1.StateOpen, withFirstPass(), withReproduced(metav1.ConditionTrue),
		withLastChecks(v1alpha1.LastChecks{Precondition: lc(exit(1), 0)}), func(i *v1alpha1.Incident) {
			i.Status.Reanalysis = &v1alpha1.Reanalysis{Count: 1}
		})
	e = newExternal(newKube(old), t0.Add(time.Minute))
	if _, err := e.Observe(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	if e.plan.noVerdict != nil || e.plan.probed != "" || len(e.plan.stale) != 1 {
		t.Errorf("an earlier analysis' probe was read: noVerdict %+v, probed %q, stale %d", e.plan.noVerdict, e.plan.probed, len(e.plan.stale))
	}
}

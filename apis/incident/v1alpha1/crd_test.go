package v1alpha1_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

const (
	crdPath     = "../../../helm/incident-provider-crds/templates/observability.krateo.io_incidents.yaml"
	examplePath = "../../../examples/incident/incident.yaml"

	requesterPolicyPath = "../../../helm/incident-provider-crds/templates/incidentapply-requested-by.yaml"
)

func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	// util/json decodes whole numbers as int64, as the apiserver does.
	if err := utiljson.Unmarshal(j, out); err != nil {
		t.Fatal(err)
	}
}

func openAPISchema(t *testing.T) *apiextensions.JSONSchemaProps {
	t.Helper()
	return openAPISchemaOf(t, crdPath)
}

func openAPISchemaOf(t *testing.T, path string) *apiextensions.JSONSchemaProps {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	readYAML(t, path, &crd)
	var s apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &s, nil); err != nil {
		t.Fatal(err)
	}
	return &s
}

// validator runs the checks the apiserver runs on a write: the OpenAPI schema, then the CEL rules.
type validator struct {
	schema     validation.SchemaValidator
	structural *schema.Structural
	cel        *cel.Validator
}

func newValidator(t *testing.T) *validator {
	t.Helper()
	return newValidatorOf(t, crdPath)
}

func newValidatorOf(t *testing.T, path string) *validator {
	t.Helper()
	s := openAPISchemaOf(t, path)
	sv, _, err := validation.NewSchemaValidator(s)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := schema.NewStructural(s)
	if err != nil {
		t.Fatal(err)
	}
	return &validator{schema: sv, structural: ss, cel: cel.NewValidator(ss, true, celconfig.PerCallLimit)}
}

// validate checks obj as a create, or as an update of old when old is not nil.
func (v *validator) validate(obj, old map[string]any) field.ErrorList {
	var errs field.ErrorList
	var oldObj any
	if old == nil {
		errs = validation.ValidateCustomResource(nil, obj, v.schema)
	} else {
		errs = validation.ValidateCustomResourceUpdate(nil, obj, old, v.schema)
		oldObj = old
	}
	celErrs, _ := v.cel.Validate(context.Background(), nil, v.structural, obj, oldObj, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

// example returns a fresh copy of examples/incident/incident.yaml: a Resolved incident.
func example(t *testing.T) map[string]any {
	t.Helper()
	var obj map[string]any
	readYAML(t, examplePath, &obj)
	return obj
}

func spec(obj map[string]any) map[string]any   { return obj["spec"].(map[string]any) }
func status(obj map[string]any) map[string]any { return obj["status"].(map[string]any) }

func lastChecks(obj map[string]any) map[string]any {
	return status(obj)["lastChecks"].(map[string]any)
}

// withState returns the example in state, with the resolution that state needs.
func withState(t *testing.T, state string) map[string]any {
	obj := example(t)
	st := status(obj)
	st["state"] = state
	switch state {
	case "Resolved":
		st["resolution"] = map[string]any{"by": "verify", "at": "2026-09-25T14:09:00Z"}
	case "Closed":
		st["resolution"] = map[string]any{"by": "user", "at": "2026-09-25T14:09:00Z"}
		spec(obj)["closed"] = true
	default:
		delete(st, "resolution")
	}
	return obj
}

type validationCase struct {
	name string
	// old is nil for a create.
	old     func(t *testing.T) map[string]any
	obj     func(t *testing.T) map[string]any
	wantErr string
}

func TestValidation(t *testing.T) {
	v := newValidator(t)

	cases := []validationCase{
		{
			name: "the example is valid",
			obj:  example,
		},
		{
			name: "a state outside the enum",
			obj: func(t *testing.T) map[string]any {
				return withState(t, "Mitigated")
			},
			wantErr: `Unsupported value: "Mitigated"`,
		},
		{
			name: "alertRef is required",
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				delete(spec(obj), "alertRef")
				return obj
			},
			wantErr: "spec.alertRef: Required value",
		},
		{
			name: "an alert name too long for a label value",
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				spec(obj)["alertRef"].(map[string]any)["name"] = strings.Repeat("a", 64)
				return obj
			},
			wantErr: "spec.alertRef.name: Too long",
		},
		{
			name: "Resolved without a resolution",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Resolved")
				delete(status(obj), "resolution")
				return obj
			},
			wantErr: "resolution is set exactly when state is Resolved or Closed",
		},
		{
			name: "a resolution on an Open incident",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Open")
				status(obj)["resolution"] = map[string]any{"by": "verify", "at": "2026-09-25T14:09:00Z"}
				return obj
			},
			wantErr: "resolution is set exactly when state is Resolved or Closed",
		},
		{
			name: "Resolved by user",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Resolved")
				status(obj)["resolution"].(map[string]any)["by"] = "user"
				return obj
			},
			wantErr: "Resolved goes with resolution.by verify, Closed with resolution.by user, alert-gone or analysis-failed",
		},
		{
			name: "Closed by verify",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				status(obj)["resolution"].(map[string]any)["by"] = "verify"
				return obj
			},
			wantErr: "Resolved goes with resolution.by verify, Closed with resolution.by user, alert-gone or analysis-failed",
		},
		{
			name: "Closed by alert-gone",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				delete(spec(obj), "closed")
				status(obj)["resolution"].(map[string]any)["by"] = "alert-gone"
				return obj
			},
		},
		{
			name: "Resolved by alert-gone",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Resolved")
				status(obj)["resolution"].(map[string]any)["by"] = "alert-gone"
				return obj
			},
			wantErr: "Resolved goes with resolution.by verify, Closed with resolution.by user, alert-gone or analysis-failed",
		},
		{
			name: "Closed by analysis-failed",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				obj["metadata"].(map[string]any)["annotations"] = map[string]any{v1alpha1.AnnotationClosedReason: v1alpha1.ClosedReasonAnalysisFailed}
				status(obj)["resolution"].(map[string]any)["by"] = "analysis-failed"
				return obj
			},
		},
		{
			name: "Resolved by analysis-failed",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Resolved")
				status(obj)["resolution"].(map[string]any)["by"] = "analysis-failed"
				return obj
			},
			wantErr: "Resolved goes with resolution.by verify, Closed with resolution.by user, alert-gone or analysis-failed",
		},
		{
			name: "a resolution outside the enum",
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				status(obj)["resolution"].(map[string]any)["by"] = "timeout"
				return obj
			},
			wantErr: `Unsupported value: "timeout"`,
		},
		{
			name: "a no-verdict streak",
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				lastChecks(obj)["verify"] = map[string]any{"exit": int64(2), "since": "2026-09-25T14:08:00Z",
					"noVerdict": map[string]any{"runs": int64(3), "since": "2026-09-25T14:06:00Z", "last": "2026-09-25T14:08:00Z"}}
				return obj
			},
		},
		{
			name: "a no-verdict streak without runs",
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				lastChecks(obj)["verify"] = map[string]any{"exit": int64(2), "since": "2026-09-25T14:08:00Z",
					"noVerdict": map[string]any{"runs": int64(0), "since": "2026-09-25T14:06:00Z", "last": "2026-09-25T14:08:00Z"}}
				return obj
			},
			wantErr: "status.lastChecks.verify.noVerdict.runs: Invalid value",
		},
		{
			name: "an exit code above 255",
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				lastChecks(obj)["precondition"].(map[string]any)["exit"] = int64(256)
				return obj
			},
			wantErr: "status.lastChecks.precondition.exit: Invalid value",
		},
		{
			name: "a result without a since",
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				delete(lastChecks(obj)["verify"].(map[string]any), "since")
				return obj
			},
			wantErr: "status.lastChecks.verify.since: Required value",
		},
		{
			name: "the first status write",
			old: func(t *testing.T) map[string]any {
				obj := example(t)
				delete(obj, "status")
				return obj
			},
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				obj["status"] = map[string]any{"state": "Analyzing"}
				return obj
			},
		},
		{
			name: "alertRef cannot change",
			old:  example,
			obj: func(t *testing.T) map[string]any {
				obj := example(t)
				spec(obj)["alertRef"].(map[string]any)["name"] = "other"
				return obj
			},
			wantErr: "alertRef is immutable",
		},
		{
			name: "closed cannot be set back to false",
			old:  func(t *testing.T) map[string]any { return withState(t, "Closed") },
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				spec(obj)["closed"] = false
				return obj
			},
			wantErr: "closed cannot be unset",
		},
		{
			name: "closed cannot be removed",
			old:  func(t *testing.T) map[string]any { return withState(t, "Closed") },
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				delete(spec(obj), "closed")
				return obj
			},
			wantErr: "closed cannot be unset",
		},
		{
			name: "applied can be reset",
			old:  func(t *testing.T) map[string]any { return withState(t, "Verifying") },
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Open")
				spec(obj)["applied"] = false
				return obj
			},
		},
		{
			name: "Open becomes Verifying",
			old:  func(t *testing.T) map[string]any { return withState(t, "Open") },
			obj:  func(t *testing.T) map[string]any { return withState(t, "Verifying") },
		},
		{
			name: "Verifying goes back to Open",
			old:  func(t *testing.T) map[string]any { return withState(t, "Verifying") },
			obj:  func(t *testing.T) map[string]any { return withState(t, "Open") },
		},
		{
			name: "Resolved becomes Closed",
			old:  func(t *testing.T) map[string]any { return withState(t, "Resolved") },
			obj:  func(t *testing.T) map[string]any { return withState(t, "Closed") },
		},
		{
			name:    "Resolved cannot reopen",
			old:     func(t *testing.T) map[string]any { return withState(t, "Resolved") },
			obj:     func(t *testing.T) map[string]any { return withState(t, "Open") },
			wantErr: "Resolved can only become Closed, and Closed is final",
		},
		{
			name:    "Closed is final",
			old:     func(t *testing.T) map[string]any { return withState(t, "Closed") },
			obj:     func(t *testing.T) map[string]any { return withState(t, "Open") },
			wantErr: "Resolved can only become Closed, and Closed is final",
		},
		{
			name: "Closed by analysis-failed is final",
			old: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				status(obj)["resolution"].(map[string]any)["by"] = "analysis-failed"
				return obj
			},
			obj:     func(t *testing.T) map[string]any { return withState(t, "Open") },
			wantErr: "Resolved can only become Closed, and Closed is final",
		},
		{
			name: "a Closed incident keeps its state",
			old:  func(t *testing.T) map[string]any { return withState(t, "Closed") },
			obj: func(t *testing.T) map[string]any {
				obj := withState(t, "Closed")
				delete(status(obj), "state")
				delete(status(obj), "resolution")
				return obj
			},
			wantErr: "Resolved can only become Closed, and Closed is final",
		},
	}

	for _, state := range []string{"Analyzing", "Open", "Verifying", "Resolved", "Closed"} {
		cases = append(cases, validationCase{
			name: "state " + state + " is valid",
			obj:  func(t *testing.T) map[string]any { return withState(t, state) },
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var old map[string]any
			if tc.old != nil {
				old = tc.old(t)
			}
			errs := v.validate(tc.obj(t), old)
			if tc.wantErr == "" {
				if len(errs) > 0 {
					t.Fatalf("want no error, got %v", errs)
				}
				return
			}
			if !strings.Contains(errs.ToAggregate().Error(), tc.wantErr) {
				t.Fatalf("want an error containing %q, got %v", tc.wantErr, errs)
			}
		})
	}
}

// TestExampleRoundTrip decodes the example into the Go types and encodes it back: a writer's
// fields survive a Status().Update from the controller.
func TestExampleRoundTrip(t *testing.T) {
	b, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	var inc v1alpha1.Incident
	if err := yaml.UnmarshalStrict(b, &inc); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(&inc)
	if err != nil {
		t.Fatal(err)
	}
	var gotObj map[string]any
	if err := utiljson.Unmarshal(got, &gotObj); err != nil {
		t.Fatal(err)
	}
	want := example(t)
	for _, k := range []string{"spec", "status"} {
		if d := cmp.Diff(want[k], gotObj[k]); d != "" {
			t.Errorf("%s changed in a round trip (-want +got):\n%s", k, d)
		}
	}
}

// TestConstantsMatchCRD keeps the Go constants 5b uses in line with the generated schema.
func TestConstantsMatchCRD(t *testing.T) {
	s := openAPISchema(t)
	st := s.Properties["status"]

	enum := func(p apiextensions.JSONSchemaProps) []string {
		var out []string
		for _, e := range p.Enum {
			out = append(out, e.(string))
		}
		return out
	}
	states := []string{
		string(v1alpha1.StateAnalyzing), string(v1alpha1.StateOpen), string(v1alpha1.StateVerifying),
		string(v1alpha1.StateResolved), string(v1alpha1.StateClosed),
	}
	if d := cmp.Diff(states, enum(st.Properties["state"])); d != "" {
		t.Errorf("status.state enum differs from the State constants (-want +got):\n%s", d)
	}
	resolvedBy := []string{
		string(v1alpha1.ResolvedByVerify), string(v1alpha1.ResolvedByUser), string(v1alpha1.ResolvedByAlertGone),
		string(v1alpha1.ResolvedByAnalysisFailed),
	}
	if d := cmp.Diff(resolvedBy, enum(st.Properties["resolution"].Properties["by"])); d != "" {
		t.Errorf("status.resolution.by enum differs from the ResolvedBy constants (-want +got):\n%s", d)
	}
	var scripts []string
	for k := range st.Properties["lastChecks"].Properties {
		scripts = append(scripts, k)
	}
	want := []string{string(v1alpha1.ScriptApply), string(v1alpha1.ScriptPrecondition), string(v1alpha1.ScriptVerify)}
	if d := cmp.Diff(want, scripts, cmpopts.SortSlices(func(a, b string) bool { return a < b })); d != "" {
		t.Errorf("status.lastChecks keys differ from the Script constants (-want +got):\n%s", d)
	}
}

// keptOnUninstall fails unless the CRD at path tells Helm to leave it on uninstall: deleting a CRD
// deletes every object of its kind.
func keptOnUninstall(t *testing.T, path string) {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	readYAML(t, path, &crd)
	if got := crd.Annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("%s: annotation helm.sh/resource-policy = %q, want %q", crd.Name, got, "keep")
	}
}

func TestIncidentCRDKeptOnUninstall(t *testing.T) {
	keptOnUninstall(t, crdPath)
}

// TestRequesterPolicyKeptOnUninstall: a recreated policy or binding is newer than every
// IncidentApply already in the cluster, and the controller refuses those requests, since nothing
// vouched for their spec.requestedBy. The template holds no Helm directives, so it is parsed as is.
func TestRequesterPolicyKeptOnUninstall(t *testing.T) {
	f, err := os.Open(requesterPolicyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got := map[string]bool{}
	r := utilyaml.NewYAMLReader(bufio.NewReader(f))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var obj metav1.PartialObjectMetadata
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind == "" {
			continue // a document of comments only
		}
		id := obj.Kind + " " + obj.Name
		got[id] = true
		if p := obj.Annotations["helm.sh/resource-policy"]; p != "keep" {
			t.Errorf("%s: annotation helm.sh/resource-policy = %q, want %q", id, p, "keep")
		}
	}
	want := map[string]bool{
		"MutatingAdmissionPolicy " + v1alpha1.RequesterPolicy:        true,
		"MutatingAdmissionPolicyBinding " + v1alpha1.RequesterPolicy: true,
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("objects in %s (-want +got):\n%s", requesterPolicyPath, d)
	}
}

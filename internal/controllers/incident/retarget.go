package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
)

// crdGetter reads one CustomResourceDefinition by name, past any cache: a version that stops being
// served must be seen as soon as discovery sees it.
type crdGetter func(ctx context.Context, name string) (*apiextensionsv1.CustomResourceDefinition, error)

// retarget is whether an applyAction whose version is not served can be moved to another version
// without a reanalysis.
type retarget struct {
	// to is the apiVersion to move it to; empty when the move is not provably version-only.
	to string
	// why says why not, when to is empty.
	why string
}

// planRetarget decides whether a, whose apiVersion this cluster does not serve, can be moved to the
// storage version of its CRD without a reanalysis. served are the versions of a's group that serve
// a's resource, from discovery. The move is provably version-only when a's resource is a CRD whose
// storage version is served, and every field a's payload sets is declared, with a matching type, in
// that version's schema. A CRD that does not exist is not provable; any other failure to read it is
// returned and decides nothing.
func planRetarget(ctx context.Context, get crdGetter, a *v1alpha1.ApplyAction, served []string) (retarget, error) {
	gv, err := schema.ParseGroupVersion(a.APIVersion)
	if err != nil || gv.Group == "" {
		return retarget{why: "the target is not a custom resource"}, nil
	}
	name := a.Resource + "." + gv.Group
	crd, err := get(ctx, name)
	switch {
	case apierrors.IsNotFound(err):
		return retarget{why: "no CustomResourceDefinition " + name}, nil
	case err != nil:
		return retarget{}, fmt.Errorf("cannot read CustomResourceDefinition %s: %w", name, err)
	}
	var storage *apiextensionsv1.CustomResourceDefinitionVersion
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			storage = &crd.Spec.Versions[i]
		}
	}
	switch {
	case storage == nil:
		return retarget{why: name + " has no storage version"}, nil
	case !storage.Served || !contains(served, storage.Name):
		return retarget{why: "the storage version " + storage.Name + " of " + name + " is not served"}, nil
	case storage.Schema == nil || storage.Schema.OpenAPIV3Schema == nil:
		return retarget{why: storage.Name + " of " + name + " has no schema"}, nil
	}
	to := schema.GroupVersion{Group: gv.Group, Version: storage.Name}.String()
	if why := payloadFits(a, storage.Schema.OpenAPIV3Schema); why != "" {
		return retarget{why: why + " in " + to}, nil
	}
	return retarget{to: to}, nil
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// rootMeta are the top-level keys every version of every resource has: they are not in a CRD's
// schema, and the API server validates them itself.
var rootMeta = map[string]bool{"apiVersion": true, "kind": true, "metadata": true}

// payloadFits says why a's payload does not fit s, the schema of the version to move to; empty when
// it fits. A delete has no payload. A create's payload names its own apiVersion, which must be a's.
func payloadFits(a *v1alpha1.ApplyAction, s *apiextensionsv1.JSONSchemaProps) string {
	if a.Verb == "delete" {
		return ""
	}
	if a.Payload == nil || len(a.Payload.Raw) == 0 {
		return ""
	}
	var body map[string]any
	if err := json.Unmarshal(a.Payload.Raw, &body); err != nil {
		return "the payload is not a JSON object"
	}
	if a.Verb == "create" {
		if v, ok := body["apiVersion"]; ok && v != a.APIVersion {
			return fmt.Sprintf("the payload's apiVersion %v is not the applyAction's", v)
		}
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		if !rootMeta[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if why := fits(body[k], child(s, k), k); why != "" {
			return why
		}
	}
	return ""
}

// child is the schema of key k of an object whose schema is s, nil when s does not declare it:
// a property, or an additionalProperties schema. A field kept only by
// x-kubernetes-preserve-unknown-fields is not declared.
func child(s *apiextensionsv1.JSONSchemaProps, k string) *apiextensionsv1.JSONSchemaProps {
	if s == nil {
		return nil
	}
	if p, ok := s.Properties[k]; ok {
		return &p
	}
	if ap := s.AdditionalProperties; ap != nil && ap.Schema != nil {
		return ap.Schema
	}
	return nil
}

// fits says why v, at path, does not fit s; empty when it does. nil s: path is not declared. A null
// v (a merge patch's removal) fits any declared field.
func fits(v any, s *apiextensionsv1.JSONSchemaProps, path string) string {
	if s == nil {
		return path + " is not declared"
	}
	if v == nil {
		return ""
	}
	if s.XIntOrString {
		switch v.(type) {
		case string, float64:
			return ""
		}
		return path + " is not an int-or-string"
	}
	switch x := v.(type) {
	case map[string]any:
		if s.Type != "object" {
			return path + " is an object, declared " + typeOf(s)
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if why := fits(x[k], child(s, k), path+"."+k); why != "" {
				return why
			}
		}
		return ""
	case []any:
		if s.Type != "array" {
			return path + " is an array, declared " + typeOf(s)
		}
		if s.Items == nil || s.Items.Schema == nil {
			return path + " declares no item schema"
		}
		for i, e := range x {
			if why := fits(e, s.Items.Schema, fmt.Sprintf("%s[%d]", path, i)); why != "" {
				return why
			}
		}
		return ""
	case string:
		if s.Type != "string" {
			return path + " is a string, declared " + typeOf(s)
		}
	case bool:
		if s.Type != "boolean" {
			return path + " is a boolean, declared " + typeOf(s)
		}
	case float64:
		switch {
		case s.Type == "number":
		case s.Type == "integer" && x == math.Trunc(x):
		default:
			return fmt.Sprintf("%s is the number %v, declared %s", path, x, typeOf(s))
		}
	}
	return ""
}

func typeOf(s *apiextensionsv1.JSONSchemaProps) string {
	if s.Type == "" {
		return "untyped"
	}
	return s.Type
}

// scriptsNaming says which script of fix names apiVersion, or resource at its version in kubectl's
// form (<resource>.<version>.<group>); empty when none does. A re-targeted applyAction with such a
// script would disagree with it.
func scriptsNaming(fix *v1alpha1.HowToFix, apiVersion, resource string) string {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return ""
	}
	needles := []string{apiVersion}
	if gv.Group != "" {
		needles = append(needles, resource+"."+gv.Version+"."+gv.Group)
	}
	for _, s := range []struct{ name, body string }{
		{"precondition", fix.Precondition}, {"apply", fix.Apply}, {"verify", fix.Verify}, {"rollback", fix.Rollback},
	} {
		for _, n := range needles {
			if strings.Contains(s.body, n) {
				return "the " + s.name + " script names " + n
			}
		}
	}
	return ""
}

// retargetAction moves a to apiVersion to: its apiVersion and, for create, the payload's.
func retargetAction(a *v1alpha1.ApplyAction, to string) error {
	if a.Verb == "create" && a.Payload != nil && len(a.Payload.Raw) > 0 {
		var body map[string]any
		if err := json.Unmarshal(a.Payload.Raw, &body); err != nil {
			return err
		}
		if _, ok := body["apiVersion"]; ok {
			body["apiVersion"] = to
			raw, err := json.Marshal(body)
			if err != nil {
				return err
			}
			a.Payload.Raw = raw
		}
	}
	a.APIVersion = to
	return nil
}

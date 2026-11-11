//go:build integration

package incident

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
	"github.com/krateo-platformops/incident-provider/internal/controllers/common/option"
)

// switchableDiscovery is the real discovery client, with a switch that fails every read the way
// an API server that cannot answer does.
type switchableDiscovery struct {
	*discovery.DiscoveryClient
	fail   atomic.Bool
	failed atomic.Int32
}

var errUnavailable = errors.New("the server is currently unable to handle the request")

func (d *switchableDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	if d.fail.Load() {
		d.failed.Add(1)
		return nil, errUnavailable
	}
	return d.DiscoveryClient.ServerResourcesForGroupVersion(gv)
}

func (d *switchableDiscovery) GroupsAndMaybeResources() (*metav1.APIGroupList, map[schema.GroupVersion]*metav1.APIResourceList, map[schema.GroupVersion]error, error) {
	if d.fail.Load() {
		d.failed.Add(1)
		return nil, nil, nil, errUnavailable
	}
	return d.DiscoveryClient.GroupsAndMaybeResources()
}

const widgetsGroup = "example.krateo.io"

// widgetCRD serves widgets at v1 and v2, stored at v2.
func widgetCRD() *apiextensionsv1.CustomResourceDefinition {
	schema := &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
		Type: "object", XPreserveUnknownFields: ptr.To(true),
	}}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets." + widgetsGroup},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: widgetsGroup,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true, Storage: false, Schema: schema},
				{Name: "v2", Served: true, Storage: true, Schema: schema},
			},
		},
	}
}

// TestApplyActionStaleEnvtest runs the Incident reconciler against a real API server, and changes
// which versions of the target's CRD it serves.
func TestApplyActionStaleEnvtest(t *testing.T) {
	env := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{filepath.Join("..", "..", "..", "helm", "incident-provider-crds", "templates", "observability.krateo.io_incidents.yaml")},
			CRDs:  []*apiextensionsv1.CustomResourceDefinition{widgetCRD()},
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("cannot start envtest (set KUBEBUILDER_ASSETS, see make test-integration): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, v1alpha1.SchemeBuilder.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	disc := &switchableDiscovery{DiscoveryClient: dc}

	const poll = time.Second
	if err := Setup(mgr, Options{
		Controller: option.ControllerOptions{
			Options: controller.Options{
				Logger:                  logging.NewNopLogger(),
				MaxConcurrentReconciles: 1,
				PollInterval:            poll,
				GlobalRateLimiter:       ratelimiter.NewGlobal(100),
			},
			Timeout: 30 * time.Second,
		},
		Checks:    Config{SettleWindow: time.Minute, CheckTimeout: time.Minute},
		Pods:      CheckPods{Namespace: checksNamespace, Template: template, Timeout: time.Minute},
		Discovery: disc,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager stopped: %v", err)
		}
	}()

	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{checksNamespace, "demo"} {
		if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	// The writer creates the incident, then writes its analysis to the status.
	inc := &v1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-stale", Namespace: "demo"},
		Spec: v1alpha1.IncidentSpec{
			AlertRef:    v1alpha1.AlertRef{Name: "widget-not-ready", Namespace: "demo"},
			Trigger:     v1alpha1.TriggerAlert,
			Prompt:      "Widget not ready",
			TriggeredAt: ptr.To(metav1.NewTime(time.Now()).Rfc3339Copy()),
		},
	}
	if err := kube.Create(ctx, inc); err != nil {
		t.Fatal(err)
	}
	action := &v1alpha1.ApplyAction{Verb: "patch", APIVersion: widgetsGroup + "/v1", Resource: "widgets", Namespace: "demo", Name: "w1",
		Payload: &runtime.RawExtension{Raw: []byte(`{"spec":{"replicas":2}}`)}}
	writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) {
		st.State = v1alpha1.StateOpen
		st.HowToFix = &v1alpha1.HowToFix{Precondition: "exit 1", Apply: "true", Verify: "exit 0", ApplyAction: action.DeepCopy()}
	})

	const target = "applyAction targets " + widgetsGroup + "/v1 widgets, which this cluster "

	served := waitFor(ctx, t, kube, inc, "False, Served", func(c prv1.Condition) bool {
		return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonServed && c.Message == target+"serves"
	})

	// LastTransitionTime has a resolution of a second: let one pass before each flip.
	time.Sleep(1100 * time.Millisecond)
	setServed(ctx, t, kube, map[string]bool{"v1": false})
	stale := waitFor(ctx, t, kube, inc, "True, VersionNotServed, served: v2", func(c prv1.Condition) bool {
		return c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonVersionNotServed &&
			c.Message == target+"does not serve; served: v2"
	})
	if !stale.LastTransitionTime.After(served.LastTransitionTime.Time) {
		t.Errorf("served -> stale: lastTransitionTime %s, want after %s", stale.LastTransitionTime, served.LastTransitionTime)
	}

	t.Run("a new served version changes the message, not the transition time", func(t *testing.T) {
		time.Sleep(1100 * time.Millisecond)
		addVersion(ctx, t, kube, "v3")
		c := waitFor(ctx, t, kube, inc, "True, served: v2, v3", func(c prv1.Condition) bool {
			return c.Status == metav1.ConditionTrue && c.Message == target+"does not serve; served: v2, v3"
		})
		if !c.LastTransitionTime.Equal(&stale.LastTransitionTime) {
			t.Errorf("lastTransitionTime = %s, want %s: the status did not flip", c.LastTransitionTime, stale.LastTransitionTime)
		}
		stale = c
	})

	t.Run("a failed discovery read leaves the condition as it is", func(t *testing.T) {
		disc.fail.Store(true)
		defer disc.fail.Store(false)
		setServed(ctx, t, kube, map[string]bool{"v1": true})
		// Several poll intervals: each reconcile reads, fails and changes nothing.
		deadline := time.Now().Add(5 * poll)
		for time.Now().Before(deadline) {
			c := condition(ctx, t, kube, inc)
			if d := cmp.Diff(stale, c); d != "" {
				t.Fatalf("ApplyActionStale changed while discovery failed (-want +got):\n%s", d)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if disc.failed.Load() == 0 {
			t.Fatal("no discovery read failed: the reconciler did not read while the switch was on")
		}
	})

	t.Run("served again: False, Served, a new transition time", func(t *testing.T) {
		c := waitFor(ctx, t, kube, inc, "False, Served", func(c prv1.Condition) bool {
			return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonServed && c.Message == target+"serves"
		})
		if !c.LastTransitionTime.After(stale.LastTransitionTime.Time) {
			t.Errorf("stale -> served: lastTransitionTime %s, want after %s", c.LastTransitionTime, stale.LastTransitionTime)
		}
	})

	t.Run("a resource served at no version: True, ResourceNotFound", func(t *testing.T) {
		writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) { st.HowToFix.ApplyAction.Resource = "gadgets" })
		waitFor(ctx, t, kube, inc, "True, ResourceNotFound", func(c prv1.Condition) bool {
			return c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonResourceNotFound &&
				c.Message == "applyAction targets "+widgetsGroup+"/v1 gadgets, which this cluster does not serve; served: none"
		})
	})

	t.Run("the controller never changes applyAction", func(t *testing.T) {
		var got v1alpha1.Incident
		if err := kube.Get(ctx, client.ObjectKeyFromObject(inc), &got); err != nil {
			t.Fatal(err)
		}
		want := action.DeepCopy()
		want.Resource = "gadgets"
		if d := cmp.Diff(want, got.Status.HowToFix.ApplyAction); d != "" {
			t.Errorf("applyAction (-want +got):\n%s", d)
		}
	})

	t.Run("no applyAction: the condition is removed", func(t *testing.T) {
		writeStatus(ctx, t, kube, inc, func(st *v1alpha1.IncidentStatus) { st.HowToFix.ApplyAction = nil })
		eventually(t, "ApplyActionStale removed", func() bool {
			var got v1alpha1.Incident
			if err := kube.Get(ctx, client.ObjectKeyFromObject(inc), &got); err != nil {
				t.Fatal(err)
			}
			for _, c := range got.Status.Conditions {
				if c.Type == v1alpha1.TypeApplyActionStale {
					return false
				}
			}
			return true
		})
	})
}

// writeStatus applies edit to the incident's status as the writer does, retrying on conflicts
// with the controller's own status writes.
func writeStatus(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident, edit func(*v1alpha1.IncidentStatus)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Incident
		if err := kube.Get(ctx, client.ObjectKeyFromObject(inc), &cur); err != nil {
			return err
		}
		edit(&cur.Status)
		return kube.Status().Update(ctx, &cur)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func updateCRD(ctx context.Context, t *testing.T, kube client.Client, edit func(*apiextensionsv1.CustomResourceDefinition)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := kube.Get(ctx, client.ObjectKey{Name: "widgets." + widgetsGroup}, &crd); err != nil {
			return err
		}
		edit(&crd)
		return kube.Update(ctx, &crd)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// setServed sets which versions of the widget CRD are served.
func setServed(ctx context.Context, t *testing.T, kube client.Client, served map[string]bool) {
	t.Helper()
	updateCRD(ctx, t, kube, func(crd *apiextensionsv1.CustomResourceDefinition) {
		for i, v := range crd.Spec.Versions {
			if s, ok := served[v.Name]; ok {
				crd.Spec.Versions[i].Served = s
			}
		}
	})
}

// addVersion adds a served, not stored, version to the widget CRD.
func addVersion(ctx context.Context, t *testing.T, kube client.Client, name string) {
	t.Helper()
	updateCRD(ctx, t, kube, func(crd *apiextensionsv1.CustomResourceDefinition) {
		v := crd.Spec.Versions[0].DeepCopy()
		v.Name, v.Served, v.Storage = name, true, false
		crd.Spec.Versions = append(crd.Spec.Versions, *v)
	})
}

func condition(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident) prv1.Condition {
	t.Helper()
	var got v1alpha1.Incident
	if err := kube.Get(ctx, client.ObjectKeyFromObject(inc), &got); err != nil {
		t.Fatal(err)
	}
	return got.GetCondition(v1alpha1.TypeApplyActionStale)
}

// waitFor waits until the incident's ApplyActionStale condition satisfies ok, and returns it.
func waitFor(ctx context.Context, t *testing.T, kube client.Client, inc *v1alpha1.Incident, what string, ok func(prv1.Condition) bool) prv1.Condition {
	t.Helper()
	var c prv1.Condition
	eventually(t, "ApplyActionStale "+what, func() bool {
		c = condition(ctx, t, kube, inc)
		return ok(c)
	})
	return c
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

//go:build integration

package incident

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
	"github.com/krateo-platformops/incident-provider/internal/controllers/common/option"
)

// TestControllerBeforeCRDEnvtest is the upgrade where the controller starts before its CRD
// declares status.state selectable (the installer's dependency gate holds the app back on a first
// install only). Measured, not assumed: the manager fails to start (its caches cannot sync), so
// the process exits and is restarted; once the CRD carries the field, the next start syncs.
func TestControllerBeforeCRDEnvtest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "helm", "incident-provider-crds", "templates", "observability.krateo.io_incidents.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions[0].SelectableFields) == 0 {
		t.Fatal("control: the committed CRD should declare selectableFields")
	}
	old := crd.DeepCopy()
	for i := range old.Spec.Versions {
		old.Spec.Versions[i].SelectableFields = nil
	}

	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{old}}}
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

	start := func() error {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
			Controller: config.Controller{SkipNameValidation: ptr.To(true), CacheSyncTimeout: 5 * time.Second},
			Cache:      cache.Options{ByObject: CacheByObject()}})
		if err != nil {
			return err
		}
		opts := Options{
			Controller: option.ControllerOptions{
				Options: controller.Options{Logger: logging.NewNopLogger(), MaxConcurrentReconciles: 1, PollInterval: poll,
					GlobalRateLimiter: ratelimiter.NewGlobal(100)},
				Timeout: 30 * time.Second,
			},
			Pods: CheckPods{Namespace: checksNamespace, Template: template, Timeout: time.Minute},
		}
		if err := Setup(mgr, opts); err != nil {
			return err
		}
		if err := SetupClosed(mgr, opts); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- mgr.Start(ctx) }()
		select {
		case err := <-done: // the manager gave up on its own: what main turns into exit 1
			return err
		case <-time.After(15 * time.Second): // still running well past the 5s sync timeout: it synced
			if !mgr.GetCache().WaitForCacheSync(context.Background()) {
				return context.DeadlineExceeded
			}
			cancel()
			<-done
			return nil
		}
	}

	t.Run("before the CRD declares the field: the manager fails to start", func(t *testing.T) {
		err := start()
		if err == nil {
			t.Fatal("the manager started against a CRD without selectableFields")
		}
		if !strings.Contains(err.Error(), "sync") {
			t.Errorf("error = %v, want a cache-sync failure", err)
		}
		t.Logf("start error (main exits 1 on it): %v", err)
	})

	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur apiextensionsv1.CustomResourceDefinition
		if err := kube.Get(context.Background(), client.ObjectKey{Name: crd.Name}, &cur); err != nil {
			return err
		}
		for i := range cur.Spec.Versions {
			cur.Spec.Versions[i].SelectableFields = crd.Spec.Versions[i].SelectableFields
		}
		return kube.Update(context.Background(), &cur)
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("once the CRD declares it: the next start syncs", func(t *testing.T) {
		var err error
		for range 5 { // the API server takes a moment to serve the new field label
			if err = start(); err == nil {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("the manager still does not start after the CRD gained selectableFields: %v", err)
	})
}

//go:build integration

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"

	"github.com/krateo-platformops/incident-provider/apis/incident/v1alpha1"
	"github.com/krateo-platformops/incident-provider/internal/controllers/common/option"
	"github.com/krateo-platformops/incident-provider/internal/controllers/incident"
)

// TestTheProcessManagerStartsEnvtest starts the manager the process runs, not one the test builds:
// from 0.15.0 to 0.17.1 it could not be created, while every test with its own manager passed.
// It then reconciles an Incident, so the controllers are the ones that run too.
func TestTheProcessManagerStartsEnvtest(t *testing.T) {
	env := &envtest.Environment{ErrorIfCRDPathMissing: true,
		CRDDirectoryPaths: []string{filepath.Join("helm", "incident-provider-crds", "templates")}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("cannot start envtest (set KUBEBUILDER_ASSETS, see make test-integration): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	const checks = "default"
	o := option.ControllerOptions{Timeout: 30 * time.Second, Options: controller.Options{Logger: logging.NewNopLogger(),
		MaxConcurrentReconciles: 1, PollInterval: time.Second, GlobalRateLimiter: ratelimiter.NewGlobal(100)}}
	pods := incident.CheckPods{Namespace: checks, Template: corev1.PodSpec{}, Timeout: time.Minute}
	mgr, err := newManager(cfg, runOptions{syncPeriod: time.Hour, metricsAddr: "0", probeAddr: "0", checksNamespace: checks,
		incident: incident.Options{Controller: o, Pods: pods, Checks: incident.Config{SettleWindow: time.Minute,
			CheckTimeout: time.Minute, AlertGoneGrace: time.Second, Retention: time.Hour}},
		apply: incident.ApplyOptions{Controller: o, Pods: pods, Applies: incident.ApplyConfig{Timeout: time.Minute,
			TTL: time.Hour, CredentialsNamespace: checks}}})
	if err != nil {
		t.Fatalf("the process' manager cannot be created: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("the manager stopped with %v", err)
		}
	})
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the caches did not sync")
	}

	inc := &v1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"},
		Spec: v1alpha1.IncidentSpec{AlertRef: v1alpha1.AlertRef{Name: "a", Namespace: "default"}, Prompt: "p", Trigger: "alert"}}
	c := mgr.GetClient()
	if err := c.Create(ctx, inc); err != nil {
		t.Fatal(err)
	}
	inc.Status.State = v1alpha1.StateOpen
	if err := c.Status().Update(ctx, inc); err != nil {
		t.Fatal(err)
	}
	inc.Spec.Closed = true
	if err := c.Update(ctx, inc); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := &v1alpha1.Incident{}
		if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{Namespace: "default", Name: "probe"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Status.State == v1alpha1.StateClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the incident closed in spec is still %q", got.Status.State)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

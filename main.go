// Command incident-provider runs the checks of every Incident and moves it through its
// lifecycle, and runs each IncidentApply's apply script as the user who created it.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"

	"github.com/krateo-platformops/incident-provider/apis"
	"github.com/krateo-platformops/incident-provider/internal/controllers"
	"github.com/krateo-platformops/incident-provider/internal/controllers/checkreader"
	"github.com/krateo-platformops/incident-provider/internal/controllers/common/option"
	"github.com/krateo-platformops/incident-provider/internal/controllers/incident"
)

const (
	serviceName  = "incident-provider"
	envVarPrefix = "INCIDENT_PROVIDER"
)

func envKey(name string) string { return envVarPrefix + "_" + name }

func main() {
	debug := flag.Bool("debug", env.Bool(envKey("DEBUG"), false), "Run with debug logging.")
	syncPeriod := flag.Duration("sync", env.Duration(envKey("SYNC_PERIOD"), time.Hour), "Controller manager sync period such as 300ms, 1.5h, or 2h45m.")
	pollInterval := flag.Duration("poll", env.Duration(envKey("POLL_INTERVAL"), time.Minute), "Time from the end of one check of an incident to the start of the next.")
	settleWindow := flag.Duration("settle-window", env.Duration(envKey("SETTLE_WINDOW"), 5*time.Minute), "How long, from entering Verifying, a verify exit 1 is retried before the incident goes back to Open.")
	noVerdictRuns := flag.Int("no-verdict-runs", env.Int(envKey("NO_VERDICT_RUNS"), 6), "Consecutive precondition or verify runs without exit 0 or 1 after which the script stops and ChecksUnknown is set; 0 = no run bound.")
	noVerdictFor := flag.Duration("no-verdict-for", env.Duration(envKey("NO_VERDICT_FOR"), 24*time.Hour), "How long precondition or verify runs may give no exit 0 or 1 before the script stops and ChecksUnknown is set; 0 = no time bound.")
	alertGoneGrace := flag.Duration("alert-gone-grace", env.Duration(envKey("ALERT_GONE_GRACE"), 10*time.Minute), "How long an Open or Verifying incident's Alert must stay NotFound before the incident is closed with resolution.by alert-gone.")
	checkTimeout := flag.Duration("check-timeout", env.Duration(envKey("CHECK_TIMEOUT"), time.Minute), "Deadline of one check run: the check pod's activeDeadlineSeconds.")
	checksNamespace := flag.String("checks-namespace", env.String(envKey("CHECKS_NAMESPACE"), ""), "Namespace the check pods run in.")
	applyTimeout := flag.Duration("apply-timeout", env.Duration(envKey("APPLY_TIMEOUT"), 2*time.Minute), "Deadline of one IncidentApply run: the apply pod's activeDeadlineSeconds.")
	applyTTL := flag.Duration("apply-ttl", env.Duration(envKey("APPLY_TTL"), 30*24*time.Hour), "How long a finished IncidentApply is kept before it is deleted.")
	credentialsNamespace := flag.String("credentials-namespace", env.String(envKey("CREDENTIALS_NAMESPACE"), ""), "Namespace of the <username>-clientconfig Secrets authn writes at login.")
	checkPodTemplate := flag.String("check-pod-template", env.String(envKey("CHECK_POD_TEMPLATE"), "/etc/incident-provider/check-pod.yaml"), "File holding the check pod spec template.")
	maxReconcileRate := flag.Int("max-reconcile-rate", env.Int(envKey("MAX_RECONCILE_RATE"), 5), "The number of concurrent reconciles.")
	leaderElection := flag.Bool("leader-election", env.Bool(envKey("LEADER_ELECTION"), false), "Use leader election for the controller manager.")
	globalReconcileRate := flag.Int("global-reconcile-rate", env.Int(envKey("GLOBAL_RECONCILE_RATE"), 20), "Reconciles per second across all incidents, in bursts of up to ten times that.")
	timeout := flag.Duration("timeout", env.Duration(envKey("TIMEOUT"), time.Minute), "The timeout for each reconcile.")
	checkReaderRole := flag.String("check-reader-role", env.String(envKey("CHECK_READER_ROLE"), ""), "ClusterRole of the check pods whose rules grant reads on every resource but Secrets; empty = not managed.")
	checkReaderRefresh := flag.Duration("check-reader-refresh", env.Duration(envKey("CHECK_READER_REFRESH"), 5*time.Minute), "How often the check reader role is rewritten from discovery.")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	handler := logging.NewOTelJSONHandler(level, os.Stderr, logging.ServiceNameAttr(serviceName)...)
	log := logging.NewSlogLogger(*slog.New(handler))
	ctrl.SetLogger(logr.FromSlogHandler(handler))

	controllerOptions := option.ControllerOptions{
		Options: controller.Options{
			Logger:                  log,
			MaxConcurrentReconciles: *maxReconcileRate,
			PollInterval:            *pollInterval,
			// A token bucket: provider-runtime consults it on every reconcile, not only after an
			// error, so a failure back-off here would delay every check. Errors back off per
			// incident in the controller-runtime queue.
			GlobalRateLimiter: ratelimiter.NewGlobal(*globalReconcileRate),
		},
		Timeout: *timeout,
	}
	if err := run(log, runOptions{
		syncPeriod:      *syncPeriod,
		leaderElection:  *leaderElection,
		checksNamespace: *checksNamespace,
		podTemplate:     *checkPodTemplate,
		checkReader:     checkreader.Options{Role: *checkReaderRole, Interval: *checkReaderRefresh, Log: log},
		incident: incident.Options{
			Controller: controllerOptions,
			Checks: incident.Config{
				SettleWindow:   *settleWindow,
				CheckTimeout:   *checkTimeout,
				NoVerdictRuns:  int32(min(max(*noVerdictRuns, 0), math.MaxInt32)),
				NoVerdictFor:   *noVerdictFor,
				AlertGoneGrace: *alertGoneGrace,
			},
		},
		apply: incident.ApplyOptions{
			Controller: controllerOptions,
			Applies: incident.ApplyConfig{
				Timeout:              *applyTimeout,
				TTL:                  *applyTTL,
				CredentialsNamespace: *credentialsNamespace,
			},
		},
	}); err != nil {
		log.Error(err, "incident-provider stopped")
		os.Exit(1)
	}
}

type runOptions struct {
	syncPeriod      time.Duration
	leaderElection  bool
	checksNamespace string
	podTemplate     string
	checkReader     checkreader.Options
	incident        incident.Options
	apply           incident.ApplyOptions
}

func run(log logging.Logger, o runOptions) error {
	if o.checksNamespace == "" {
		return fmt.Errorf("the checks namespace is required (--checks-namespace or %s)", envKey("CHECKS_NAMESPACE"))
	}
	if o.apply.Applies.CredentialsNamespace == "" {
		return fmt.Errorf("the credentials namespace is required (--credentials-namespace or %s)", envKey("CREDENTIALS_NAMESPACE"))
	}
	template, err := incident.LoadPodTemplate(o.podTemplate)
	if err != nil {
		return err
	}
	o.incident.Pods = incident.CheckPods{
		Namespace: o.checksNamespace,
		Template:  template,
		Timeout:   o.incident.Checks.CheckTimeout,
	}
	o.apply.Pods = o.incident.Pods

	log.Info("Starting incident-provider",
		"poll-interval", o.incident.Controller.PollInterval.String(),
		"settle-window", o.incident.Checks.SettleWindow.String(),
		"check-timeout", o.incident.Checks.CheckTimeout.String(),
		"no-verdict-runs", o.incident.Checks.NoVerdictRuns,
		"no-verdict-for", o.incident.Checks.NoVerdictFor.String(),
		"alert-gone-grace", o.incident.Checks.AlertGoneGrace.String(),
		"checks-namespace", o.checksNamespace,
		"apply-timeout", o.apply.Applies.Timeout.String(),
		"apply-ttl", o.apply.Applies.TTL.String(),
		"credentials-namespace", o.apply.Applies.CredentialsNamespace,
		"check-reader-role", o.checkReader.Role,
		"leader-election", o.leaderElection)

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("cannot get API server rest config: %w", err)
	}

	// Check and apply pods and their ConfigMaps are cached only in the checks namespace. Secrets
	// are never cached: the credentials are read past the cache, one at a time.
	checkObjects := cache.ByObject{
		Namespaces: map[string]cache.Config{o.checksNamespace: {}},
		Label:      labels.SelectorFromSet(labels.Set{incident.LabelComponent: incident.ComponentCheck}),
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		LeaderElection:   o.leaderElection,
		LeaderElectionID: "incident-provider.observability.krateo.io",
		Cache: cache.Options{
			SyncPeriod: &o.syncPeriod,
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}:       checkObjects,
				&corev1.ConfigMap{}: checkObjects,
			},
		},
		Metrics:                metricsserver.Options{BindAddress: ":8080"},
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		return fmt.Errorf("cannot create controller manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		return fmt.Errorf("cannot add APIs to scheme: %w", err)
	}
	if err := controllers.Setup(mgr, o.incident, o.apply); err != nil {
		return fmt.Errorf("cannot set up controllers: %w", err)
	}
	if o.checkReader.Role != "" {
		if err := checkreader.Setup(mgr, o.checkReader); err != nil {
			return fmt.Errorf("cannot set up the check reader role: %w", err)
		}
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

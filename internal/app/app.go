// Package app assembles previewd: the controller-runtime manager, the
// PreviewEnvironment reconciler, the webhook receiver, the orphan collector
// and the SCM reporter. `previewd serve`, the integration tests and the
// offline demo all build the process through New, so they exercise the same
// wiring.
package app

import (
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/config"
	"github.com/archit2501/previewd/internal/controller"
	"github.com/archit2501/previewd/internal/gc"
	"github.com/archit2501/previewd/internal/intake"
	"github.com/archit2501/previewd/internal/metrics"
	"github.com/archit2501/previewd/internal/render"
	"github.com/archit2501/previewd/internal/scm"
	"github.com/archit2501/previewd/internal/webhook"
)

// Overrides replace production dependencies (tests and the demo).
type Overrides struct {
	// Reporter replaces the GitHub/log reporter.
	Reporter scm.Reporter
	// Clock replaces the real clock.
	Clock clock.Clock
	// Catalog replaces loading templates from cfg.TemplatesDir.
	Catalog *render.Catalog
	// SkipNameValidation allows building several managers in one process.
	SkipNameValidation bool
}

// App is a built, not yet started, previewd process.
type App struct {
	Manager    ctrl.Manager
	Webhook    *webhook.Server
	Reconciler *controller.Reconciler
	Collector  *gc.Collector
	Reporter   scm.Reporter
}

// Scheme returns a scheme with the core types and the previewd API.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// New builds the manager and registers every component with it.
func New(restCfg *rest.Config, cfg config.Config, log logr.Logger, o Overrides) (*App, error) {
	scheme, err := Scheme()
	if err != nil {
		return nil, err
	}
	catalog := o.Catalog
	if catalog == nil {
		catalog, err = render.LoadCatalog(os.DirFS(cfg.TemplatesDir), ".")
		if err != nil {
			return nil, err
		}
	}
	for _, r := range cfg.Rules {
		if _, ok := catalog.Get(r.Template); !ok {
			return nil, fmt.Errorf("repository rule %q uses unknown template set %q (have %v)", r.Pattern, r.Template, catalog.Names())
		}
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}
	reporter := selectReporter(cfg, o, log)

	skip := o.SkipNameValidation
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Logger:                  log,
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress:  cfg.ProbeAddr,
		LeaderElection:          cfg.LeaderElect,
		LeaderElectionID:        "previewd.previewd.dev",
		LeaderElectionNamespace: cfg.LeaderElectNS,
		Cache:                   controller.CacheOptions(),
		Controller:              ctrlConfig(skip),
	})
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	rec := &controller.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Catalog:   catalog,
		Reporter:  reporter,
		Clock:     clk,
		Events:    mgr.GetEventRecorder("previewd"),
		Phases:    metrics.NewPhaseTracker(metrics.Environments),
		Options: controller.Options{
			PollInterval:            cfg.PollInterval,
			Retention:               cfg.Retention,
			RequiredLabel:           cfg.RequiredLabel,
			SkipDrafts:              cfg.SkipDrafts,
			MaxConcurrentReconciles: cfg.MaxConcurrent,
		},
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("setup controller: %w", err)
	}

	handler := &webhook.Handler{
		Secrets:      cfg.WebhookSecrets,
		Sink:         &intake.Sink{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Rules: cfg.Rules, Log: log.WithName("intake")},
		Deliveries:   webhook.NewDeliveryCache(cfg.DeliveryCache, cfg.DeliveryTTL, clk.Now),
		MaxBodyBytes: cfg.MaxBodyBytes,
		AllowForks:   cfg.AllowForks,
		Log:          log.WithName("webhook"),
	}
	srv := webhook.NewServer(cfg.WebhookAddr, cfg.WebhookPath, handler, log.WithName("webhook"))
	if err := mgr.Add(srv); err != nil {
		return nil, err
	}
	collector := &gc.Collector{Client: mgr.GetClient(), Interval: cfg.GCInterval, Grace: cfg.GCGrace, Clock: clk, Log: log.WithName("gc")}
	if err := mgr.Add(collector); err != nil {
		return nil, err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, err
	}
	return &App{Manager: mgr, Webhook: srv, Reconciler: rec, Collector: collector, Reporter: reporter}, nil
}

// DefaultConfig is a complete configuration with production defaults, for
// callers that build a Config programmatically (tests, demo).
func DefaultConfig() config.Config {
	return config.Config{
		MetricsAddr: "0", ProbeAddr: "0", WebhookAddr: "127.0.0.1:0", WebhookPath: "/webhook",
		TemplatesDir: "templates", DefaultTemplate: "default",
		PollInterval: 10 * time.Second, Retention: 24 * time.Hour,
		GCInterval: 5 * time.Minute, GCGrace: 10 * time.Minute,
		MaxConcurrent: 4, MaxBodyBytes: webhook.DefaultMaxBodyBytes,
		DeliveryCache: 10000, DeliveryTTL: time.Hour, StatusContext: "previewd",
	}
}

// selectReporter picks the override, the GitHub client (token configured)
// or the log-only reporter.
func selectReporter(cfg config.Config, o Overrides, log logr.Logger) scm.Reporter {
	switch {
	case o.Reporter != nil:
		return o.Reporter
	case cfg.GitHubToken != "":
		return &scm.GitHub{BaseURL: cfg.GitHubAPI, Token: cfg.GitHubToken, Context: cfg.StatusContext}
	default:
		return scm.LogReporter{Log: log.WithName("scm")}
	}
}

func ctrlConfig(skipNameValidation bool) ctrlcfg.Controller {
	return ctrlcfg.Controller{SkipNameValidation: &skipNameValidation}
}

// Package config parses previewd's command-line flags and environment into
// a validated Config.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SlateGitOrg/previewd/internal/intake"
	"github.com/SlateGitOrg/previewd/internal/scm"
	"github.com/SlateGitOrg/previewd/internal/webhook"
)

// Environment variables read by previewd. Secrets come from the
// environment or files, never from flags, so they do not show up in
// process listings.
const (
	EnvWebhookSecrets = "PREVIEWD_WEBHOOK_SECRETS" //nolint:gosec // variable name, not a credential
	EnvGitHubToken    = "GITHUB_TOKEN"             //nolint:gosec // variable name, not a credential
)

// Config is the full runtime configuration of `previewd serve`.
type Config struct {
	MetricsAddr     string
	ProbeAddr       string
	WebhookAddr     string
	WebhookPath     string
	LeaderElect     bool
	LeaderElectNS   string
	TemplatesDir    string
	DefaultTemplate string
	Rules           []intake.Rule
	RequiredLabel   string
	SkipDrafts      bool
	AllowForks      bool
	PollInterval    time.Duration
	Retention       time.Duration
	GCInterval      time.Duration
	GCGrace         time.Duration
	MaxConcurrent   int
	MaxBodyBytes    int64
	DeliveryCache   int
	DeliveryTTL     time.Duration
	WebhookSecrets  [][]byte
	GitHubAPI       string
	GitHubToken     string
	StatusContext   string
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// Parse parses args (without the program/subcommand name). getenv and
// readFile are injectable for tests.
func Parse(args []string, getenv func(string) string, readFile func(string) ([]byte, error), output io.Writer) (Config, error) {
	var c Config
	var repos listFlag
	var secretFile, tokenFile string
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&c.MetricsAddr, "metrics-bind-address", ":8080", "address for the Prometheus /metrics endpoint (\"0\" disables)")
	fs.StringVar(&c.ProbeAddr, "health-probe-bind-address", ":8081", "address for /healthz and /readyz")
	fs.StringVar(&c.WebhookAddr, "webhook-bind-address", ":8090", "address the SCM webhook receiver listens on")
	fs.StringVar(&c.WebhookPath, "webhook-path", "/webhook", "HTTP path of the webhook receiver")
	fs.BoolVar(&c.LeaderElect, "leader-elect", false, "enable leader election (required with more than one replica)")
	fs.StringVar(&c.LeaderElectNS, "leader-election-namespace", "", "namespace for the leader-election lease (default: in-cluster namespace)")
	fs.StringVar(&c.TemplatesDir, "templates-dir", "templates", "directory containing template sets (one subdirectory each)")
	fs.StringVar(&c.DefaultTemplate, "default-template", "default", "template set used by repository rules that do not name one")
	fs.Var(&repos, "repos", "repository rules, comma-separated or repeated: owner/name, owner/* or */*, optionally =template")
	fs.StringVar(&c.RequiredLabel, "required-label", "", "only PRs carrying this label get a preview (empty: every open PR)")
	fs.BoolVar(&c.SkipDrafts, "skip-drafts", false, "do not run previews for draft PRs")
	fs.BoolVar(&c.AllowForks, "allow-forks", false, "accept PRs whose head branch lives in a fork")
	fs.DurationVar(&c.PollInterval, "poll-interval", 10*time.Second, "requeue interval while waiting on the cluster")
	fs.DurationVar(&c.Retention, "retention", 24*time.Hour, "keep Closed/Expired records this long (0 keeps them)")
	fs.DurationVar(&c.GCInterval, "gc-interval", 5*time.Minute, "orphan namespace collection interval")
	fs.DurationVar(&c.GCGrace, "gc-grace", 10*time.Minute, "minimum namespace age before it can be collected as an orphan")
	fs.IntVar(&c.MaxConcurrent, "max-concurrent-reconciles", 4, "parallel reconciles")
	fs.Int64Var(&c.MaxBodyBytes, "max-body-bytes", webhook.DefaultMaxBodyBytes, "maximum webhook request body")
	fs.IntVar(&c.DeliveryCache, "delivery-cache-size", 10000, "delivery IDs remembered for duplicate detection")
	fs.DurationVar(&c.DeliveryTTL, "delivery-cache-ttl", time.Hour, "how long delivery IDs are remembered")
	fs.StringVar(&secretFile, "webhook-secret-file", "", "file with one webhook secret per line (alternative to $"+EnvWebhookSecrets+")")
	fs.StringVar(&c.GitHubAPI, "github-api-url", scm.DefaultGitHubAPI, "GitHub REST API root (GHES: https://HOST/api/v3)")
	fs.StringVar(&tokenFile, "github-token-file", "", "file containing the GitHub token (alternative to $"+EnvGitHubToken+")")
	fs.StringVar(&c.StatusContext, "status-context", "previewd", "commit status context name")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	var errs []error
	rules, err := intake.ParseRules(repos, c.DefaultTemplate)
	if err != nil {
		errs = append(errs, fmt.Errorf("--repos: %w", err))
	}
	c.Rules = rules

	var rawSecrets []string
	if secretFile != "" {
		b, err := readFile(secretFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("--webhook-secret-file: %w", err))
		}
		rawSecrets = strings.Split(string(b), "\n")
	} else {
		rawSecrets = strings.Split(getenv(EnvWebhookSecrets), ",")
	}
	for _, s := range rawSecrets {
		if s = strings.TrimSpace(s); s != "" {
			if len(s) < 16 {
				errs = append(errs, errors.New("webhook secrets must be at least 16 characters"))
				continue
			}
			c.WebhookSecrets = append(c.WebhookSecrets, []byte(s))
		}
	}
	if len(c.WebhookSecrets) == 0 && secretFile == "" {
		errs = append(errs, fmt.Errorf("no webhook secret: set $%s or --webhook-secret-file", EnvWebhookSecrets))
	} else if len(c.WebhookSecrets) == 0 {
		errs = append(errs, errors.New("--webhook-secret-file contains no secrets"))
	}

	if tokenFile != "" {
		b, err := readFile(tokenFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("--github-token-file: %w", err))
		}
		c.GitHubToken = strings.TrimSpace(string(b))
	} else {
		c.GitHubToken = strings.TrimSpace(getenv(EnvGitHubToken))
	}

	for name, d := range map[string]time.Duration{
		"--poll-interval": c.PollInterval, "--gc-interval": c.GCInterval, "--delivery-cache-ttl": c.DeliveryTTL,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	if c.Retention < 0 || c.GCGrace < 0 {
		errs = append(errs, errors.New("--retention and --gc-grace must not be negative"))
	}
	if c.MaxConcurrent < 1 || c.MaxBodyBytes < 1 || c.DeliveryCache < 1 {
		errs = append(errs, errors.New("--max-concurrent-reconciles, --max-body-bytes and --delivery-cache-size must be positive"))
	}
	if !strings.HasPrefix(c.WebhookPath, "/") {
		errs = append(errs, errors.New("--webhook-path must start with /"))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// FromOS parses os.Args-style arguments against the real environment.
func FromOS(args []string, output io.Writer) (Config, error) {
	return Parse(args, os.Getenv, os.ReadFile, output)
}

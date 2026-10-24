// Package cli implements the previewd command line: `serve` runs the
// controller, `render` previews a template set offline, `sign` computes a
// webhook signature for testing deliveries by hand.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/app"
	"github.com/archit2501/previewd/internal/config"
	"github.com/archit2501/previewd/internal/naming"
	"github.com/archit2501/previewd/internal/render"
	"github.com/archit2501/previewd/internal/webhook"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "dev"

// IO bundles the process streams and environment for testability.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
}

const usage = `previewd — ephemeral preview environments for pull requests

Usage:
  previewd serve  [flags]   run the controller and webhook receiver
  previewd render [flags]   render a template set for a PR and print the YAML
  previewd sign   [flags]   print the X-Hub-Signature-256 for a payload on stdin
  previewd version

Run "previewd <command> -h" for flags.
`

// Run executes the command line and returns the process exit code.
func Run(args []string, stdio IO) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdio.Stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve(args[1:], stdio)
	case "render":
		err = renderCmd(args[1:], stdio)
	case "sign":
		err = sign(args[1:], stdio)
	case "version":
		_, _ = fmt.Fprintln(stdio.Stdout, "previewd", Version)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdio.Stdout, usage)
	default:
		_, _ = fmt.Fprintf(stdio.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stdio.Stderr, "error:", err)
		return 1
	}
	return 0
}

func serve(args []string, stdio IO) error {
	cfg, err := config.Parse(args, stdio.Getenv, os.ReadFile, stdio.Stderr)
	if err != nil {
		return err
	}
	log := zap.New(zap.WriteTo(stdio.Stderr))
	ctrl.SetLogger(log)
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	a, err := app.New(restCfg, cfg, log, app.Overrides{})
	if err != nil {
		return err
	}
	log.Info("starting previewd", "version", Version, "templates", cfg.TemplatesDir, "webhook", cfg.WebhookAddr+cfg.WebhookPath)
	return a.Manager.Start(ctrl.SetupSignalHandler())
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func renderCmd(args []string, stdio IO) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stdio.Stderr)
	dir := fs.String("templates-dir", "templates", "directory containing template sets")
	tpl := fs.String("template", "default", "template set name")
	repo := fs.String("repo", "", "repository as owner/name (required)")
	number := fs.Int("pr", 0, "pull request number (required)")
	sha := fs.String("sha", "", "head commit SHA (required)")
	branch := fs.String("branch", "", "head branch name")
	title := fs.String("title", "", "pull request title")
	expires := fs.String("expires-at", "", "expiry timestamp (RFC 3339); default now + template TTL")
	var labels stringList
	fs.Var(&labels, "label", "pull request label (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	owner, name, ok := strings.Cut(*repo, "/")
	if !ok || owner == "" || name == "" || *number < 1 || *sha == "" {
		return errors.New("render: --repo owner/name, --pr and --sha are required")
	}
	catalog, err := render.LoadCatalog(os.DirFS(*dir), ".")
	if err != nil {
		return err
	}
	ts, found := catalog.Get(*tpl)
	if !found {
		return fmt.Errorf("template set %q not found (have %v)", *tpl, catalog.Names())
	}
	at := time.Now().Add(ts.TTL()).UTC().Truncate(time.Second)
	if *expires != "" {
		if at, err = time.Parse(time.RFC3339, *expires); err != nil {
			return fmt.Errorf("--expires-at: %w", err)
		}
	}
	b, err := ts.Render(render.Input{
		Instance: naming.Instance(owner, name, *number),
		Repo:     v1alpha1.RepositoryRef{Owner: owner, Name: name},
		PullRequest: v1alpha1.PullRequestRef{
			Number: *number, HeadSHA: *sha, HeadRef: *branch, Title: *title, Labels: labels,
		},
		ExpiresAt: at,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdio.Stdout, "# previewd render: template=%s namespace=%s objects=%d hash=%s url=%s\n",
		ts.Name, b.Namespace.GetName(), len(b.Objects)+1, b.Hash, b.Values.URL)
	for _, o := range append([]*unstructured.Unstructured{b.Namespace}, b.Objects...) {
		y, err := yaml.Marshal(o.Object)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdio.Stdout, "---\n%s", y)
	}
	return nil
}

func sign(args []string, stdio IO) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	fs.SetOutput(stdio.Stderr)
	secretFile := fs.String("secret-file", "", "file containing the webhook secret (default: first entry of $"+config.EnvWebhookSecrets+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var secret string
	if *secretFile != "" {
		b, err := os.ReadFile(*secretFile)
		if err != nil {
			return err
		}
		secret = strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	} else {
		secret = strings.TrimSpace(strings.Split(stdio.Getenv(config.EnvWebhookSecrets), ",")[0])
	}
	if secret == "" {
		return errors.New("sign: no secret (set $" + config.EnvWebhookSecrets + " or --secret-file)")
	}
	body, err := io.ReadAll(stdio.Stdin)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdio.Stdout, webhook.Sign([]byte(secret), body))
	return nil
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
	"github.com/SlateGitOrg/previewd/internal/intake"
	"github.com/SlateGitOrg/previewd/internal/scm"
	"github.com/SlateGitOrg/previewd/internal/webhook"
)

var (
	testCfg    *rest.Config
	envtestErr string
)

func TestMain(m *testing.M) {
	logf.SetLogger(logr.Discard())
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		envtestErr = "KUBEBUILDER_ASSETS is not set (run `make test`)"
		if os.Getenv("PREVIEWD_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "envtest required but", envtestErr)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}
	testCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

const secret = "integration-test-secret-0123"

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestEndToEnd drives the assembled process the way GitHub would: signed
// webhooks in over HTTP, watches and reconciles in the manager, statuses out
// through the SCM client, metrics on /metrics.
func TestEndToEnd(t *testing.T) {
	if testCfg == nil {
		t.Skip(envtestErr)
	}
	cfg := DefaultConfig()
	cfg.TemplatesDir = filepath.Join("..", "..", "templates")
	cfg.WebhookSecrets = [][]byte{[]byte(secret)}
	cfg.Rules, _ = intake.ParseRules([]string{"acme/*"}, "default")
	cfg.PollInterval = 200 * time.Millisecond
	cfg.MetricsAddr = freeAddr(t)
	rec := &scm.Recorder{}
	// The fixtures are dated 2026-10-01; pin the clock just after them so
	// the 72h TTL has not elapsed.
	clk := clocktesting.NewFakeClock(time.Date(2026, 10, 1, 9, 5, 0, 0, time.UTC))
	a, err := New(testCfg, cfg, logr.Discard(), Overrides{Reporter: rec, Clock: clk, SkipNameValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Manager.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	select {
	case <-a.Webhook.Ready():
	case <-time.After(20 * time.Second):
		t.Fatal("webhook server did not start")
	}
	url := "http://" + a.Webhook.BoundAddr().String() + "/webhook"
	send := func(file, delivery string) webhook.Response {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "webhooks", file))
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set(webhook.EventHeader, "pull_request")
		req.Header.Set(webhook.DeliveryHeader, delivery)
		req.Header.Set(webhook.SignatureHeader, webhook.Sign([]byte(secret), body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out webhook.Response
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	c, err := client.New(testCfg, client.Options{Scheme: a.Manager.GetScheme()})
	if err != nil {
		t.Fatal(err)
	}
	get := func() *v1alpha1.PreviewEnvironment {
		var pe v1alpha1.PreviewEnvironment
		if err := c.Get(ctx, types.NamespacedName{Name: "acme-shop-pr-42"}, &pe); err != nil {
			return nil
		}
		return &pe
	}

	if r := send("01-opened.json", "e2e-1"); r.Status != "created" {
		t.Fatalf("opened: %+v", r)
	}
	eventually(t, "namespace and Provisioning phase", func() bool {
		pe := get()
		return pe != nil && pe.Status.Phase == v1alpha1.PhaseProvisioning && pe.Status.AppliedHash != ""
	})

	// Play the deployment controller: the Deployment watch must trigger the
	// transition to Ready without waiting for a poll.
	var d appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "pr-shop-42", Name: "web"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	if err := c.Status().Update(ctx, &d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Ready phase", func() bool {
		pe := get()
		return pe != nil && pe.Status.Phase == v1alpha1.PhaseReady
	})

	if r := send("02-synchronize.json", "e2e-2"); r.Status != "updated" {
		t.Fatalf("synchronize: %+v", r)
	}
	if r := send("02-synchronize.json", "e2e-2"); r.Status != "duplicate" {
		t.Fatalf("redelivery: %+v", r)
	}
	if r := send("03-stale-synchronize.json", "e2e-3"); r.Status != "stale" {
		t.Fatalf("stale: %+v", r)
	}
	eventually(t, "re-apply of new head", func() bool {
		pe := get()
		return pe != nil && pe.Status.AppliedSHA == "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d"
	})

	if r := send("06-closed.json", "e2e-6"); r.Status != "updated" {
		t.Fatalf("closed: %+v", r)
	}
	eventually(t, "namespace deletion", func() bool {
		var ns corev1.Namespace
		err := c.Get(ctx, types.NamespacedName{Name: "pr-shop-42"}, &ns)
		return err == nil && !ns.DeletionTimestamp.IsZero()
	})

	var states []string
	for _, s := range rec.Statuses() {
		states = append(states, string(s.State)+"@"+s.SHA[:7])
	}
	if got := strings.Join(states, ","); got != "pending@4f2a9c1,ready@4f2a9c1,pending@9e8d7c6" {
		t.Fatalf("statuses %s", got)
	}

	resp, err := http.Get("http://" + cfg.MetricsAddr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`previewd_webhook_requests_total{event="pull_request",outcome="created"} 1`,
		`previewd_webhook_requests_total{event="pull_request",outcome="stale"} 1`,
		`previewd_phase_transitions_total{from="Provisioning",to="Ready"}`,
		`previewd_scm_status_reports_total{result="ok",state="ready"}`,
		`controller_runtime_reconcile_total{controller="previewenvironment"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestNewRejectsUnknownTemplateInRules(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TemplatesDir = filepath.Join("..", "..", "templates")
	cfg.Rules = []intake.Rule{{Pattern: "a/*", Template: "nope"}}
	_, err := New(&rest.Config{Host: "http://127.0.0.1:1"}, cfg, logr.Discard(), Overrides{SkipNameValidation: true})
	if err == nil || !strings.Contains(err.Error(), "unknown template set") {
		t.Fatalf("got %v", err)
	}
	cfg.TemplatesDir = "does-not-exist"
	if _, err := New(&rest.Config{Host: "http://127.0.0.1:1"}, cfg, logr.Discard(), Overrides{}); err == nil {
		t.Fatal("expected template load error")
	}
}

func TestReporterSelection(t *testing.T) {
	cfg := DefaultConfig()
	if _, ok := selectReporter(cfg, Overrides{}, logr.Discard()).(scm.LogReporter); !ok {
		t.Fatal("no token should log only")
	}
	cfg.GitHubToken = "t"
	if g, ok := selectReporter(cfg, Overrides{}, logr.Discard()).(*scm.GitHub); !ok || g.Token != "t" || g.Context != "previewd" {
		t.Fatal("token should select the GitHub reporter")
	}
	rec := &scm.Recorder{}
	if selectReporter(cfg, Overrides{Reporter: rec}, logr.Discard()) != rec {
		t.Fatal("override ignored")
	}
}

// TestDeployManifestsAreValid server-side dry-runs the shipped deployment
// manifests and the sample against the envtest API server, so schema errors
// in config/ fail the build even though no real cluster is involved.
func TestDeployManifestsAreValid(t *testing.T) {
	if testCfg == nil {
		t.Skip(envtestErr)
	}
	s, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(testCfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "previewd-system"}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	files := []string{"rbac/role.yaml", "manager/manager.yaml", "samples/previewenvironment.yaml"}
	count := 0
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range strings.Split(string(raw), "\n---\n") {
			var obj unstructured.Unstructured
			if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if len(obj.Object) == 0 {
				continue
			}
			if obj.GetKind() == "Namespace" {
				continue // created above
			}
			if err := c.Create(ctx, &obj, client.DryRunAll); err != nil {
				t.Errorf("%s: %s %s rejected: %v", f, obj.GetKind(), obj.GetName(), err)
			}
			count++
		}
	}
	if count < 7 {
		t.Fatalf("validated only %d objects", count)
	}
}

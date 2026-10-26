package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
	"github.com/SlateGitOrg/previewd/internal/app"
	"github.com/SlateGitOrg/previewd/internal/intake"
	"github.com/SlateGitOrg/previewd/internal/scm"
	"github.com/SlateGitOrg/previewd/internal/webhook"
)

// runScale opens n pull requests at once and measures webhook latency, time
// from delivery to Ready, and time from close to Closed. It is a throughput
// smoke test against a local API server, not a production benchmark: envtest
// has no kubelet, so "Ready" means the simulator marked Deployments
// available.
func runScale(n int, fixtures, templates, crds string) error {
	logf.SetLogger(logr.Discard())
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return fmt.Errorf("KUBEBUILDER_ASSETS is not set; run `make bench-scale`")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		return err
	}
	defer func() { _ = env.Stop() }()

	cfg := app.DefaultConfig()
	cfg.TemplatesDir = templates
	cfg.WebhookSecrets = [][]byte{[]byte(secret)}
	cfg.Rules, _ = intake.ParseRules([]string{"acme/*"}, "default")
	cfg.PollInterval = 200 * time.Millisecond
	cfg.Retention = 0
	clk := clocktesting.NewFakeClock(time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC))
	a, err := app.New(restCfg, cfg, logr.Discard(), app.Overrides{Clock: clk, Reporter: &scm.Recorder{}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Manager.Start(ctx) }()
	<-a.Webhook.Ready()
	c, err := client.New(restCfg, client.Options{Scheme: a.Manager.GetScheme()})
	if err != nil {
		return err
	}
	go simulateCluster(ctx, c)
	url := "http://" + a.Webhook.BoundAddr().String() + "/webhook"

	raw, err := os.ReadFile(filepath.Join(fixtures, "01-opened.json"))
	if err != nil {
		return err
	}
	payload := func(number int, action, state string) []byte {
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		p["action"], p["number"] = action, number
		pr := p["pull_request"].(map[string]any)
		pr["number"], pr["state"] = number, state
		pr["updated_at"] = clk.Now().UTC().Format(time.RFC3339)
		b, _ := json.Marshal(p)
		return b
	}
	send := func(number int, action, state string) (time.Duration, error) {
		body := payload(number, action, state)
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set(webhook.EventHeader, "pull_request")
		req.Header.Set(webhook.DeliveryHeader, fmt.Sprintf("%s-%d", action, number))
		req.Header.Set(webhook.SignatureHeader, webhook.Sign([]byte(secret), body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return 0, fmt.Errorf("PR %d %s: HTTP %d", number, action, resp.StatusCode)
		}
		return time.Since(start), nil
	}

	// Warm-up delivery: the first request pays for API discovery and client
	// setup, which is not what this run measures.
	warm, err := send(999, "opened", "open")
	if err != nil {
		return err
	}
	fmt.Printf("warm-up delivery took %v (excluded)\n", warm.Round(time.Millisecond))
	n0 := n
	n++ // the warm-up environment also counts towards convergence below
	fmt.Printf("scale run: %d pull requests, template=default (11 objects each), envtest %s\n", n0, restCfg.Host)
	webhookLat, readyLat, err := phase(ctx, c, n0, n, func(i int) (time.Duration, error) { return send(1000+i, "opened", "open") },
		func(pe *v1alpha1.PreviewEnvironment) bool { return pe.Status.Phase == v1alpha1.PhaseReady })
	if err != nil {
		return err
	}
	report("open: webhook response", webhookLat)
	report("open: delivery -> Ready", readyLat)
	var nss corev1.NamespaceList
	if err := c.List(ctx, &nss, client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue}); err != nil {
		return err
	}
	fmt.Printf("  preview namespaces live: %d\n", len(nss.Items))

	clk.Step(time.Minute)
	webhookLat, closeLat, err := phase(ctx, c, n0, n0, func(i int) (time.Duration, error) { return send(1000+i, "closed", "closed") },
		func(pe *v1alpha1.PreviewEnvironment) bool { return pe.Status.Phase == v1alpha1.PhaseClosed })
	if err != nil {
		return err
	}
	report("close: webhook response", webhookLat)
	report("close: delivery -> Closed", closeLat)
	return nil
}

// phase fires n deliveries concurrently and waits until every environment
// satisfies done, returning per-PR webhook and convergence latencies.
func phase(ctx context.Context, c client.Client, n, want int, fire func(int) (time.Duration, error), done func(*v1alpha1.PreviewEnvironment) bool) ([]time.Duration, []time.Duration, error) {
	start := time.Now()
	hook := make([]time.Duration, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hook[i], errs[i] = fire(i)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	reached := map[string]time.Duration{}
	deadline := time.Now().Add(2 * time.Minute)
	for len(reached) < want {
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("only %d/%d environments converged", len(reached), want)
		}
		var list v1alpha1.PreviewEnvironmentList
		if err := c.List(ctx, &list); err != nil {
			return nil, nil, err
		}
		for i := range list.Items {
			pe := &list.Items[i]
			if _, ok := reached[pe.Name]; !ok && done(pe) {
				reached[pe.Name] = time.Since(start)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	conv := make([]time.Duration, 0, n)
	for name, d := range reached {
		if name != "acme-shop-pr-999" {
			conv = append(conv, d)
		}
	}
	return hook, conv, nil
}

func report(label string, ds []time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	pct := func(p float64) time.Duration { return ds[int(p*float64(len(ds)-1))] }
	fmt.Printf("  %-28s n=%d p50=%v p95=%v max=%v\n", label, len(ds),
		pct(0.50).Round(time.Millisecond), pct(0.95).Round(time.Millisecond), ds[len(ds)-1].Round(time.Millisecond))
}

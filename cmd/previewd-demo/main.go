// Command previewd-demo runs previewd end to end against a local, offline
// Kubernetes API server (envtest: real kube-apiserver + etcd, no nodes) and
// a local fake GitHub status API, replaying signed webhook fixtures through
// the full lifecycle: open, push, duplicate and stale deliveries, TTL
// expiry, revival, close, retention and orphan collection.
//
// envtest runs no controllers, so a small simulator plays the parts of the
// deployment controller (marks Deployments available) and the namespace
// controller (finalizes terminating namespaces). Everything previewd does is
// real: it talks to the API server over HTTP exactly as it would in a
// cluster.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/app"
	"github.com/archit2501/previewd/internal/gc"
	"github.com/archit2501/previewd/internal/intake"
	"github.com/archit2501/previewd/internal/webhook"
)

const (
	secret   = "demo-webhook-secret-not-for-production" //nolint:gosec // throwaway secret for the offline demo
	instance = "acme-shop-pr-42"
	nsName   = "pr-shop-42"
	sha1     = "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c"
	sha2     = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d"
	sha3     = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
)

type demo struct {
	ctx      context.Context
	c        client.Client
	url      string
	fixtures string
	clock    *clocktesting.FakeClock
	deliver  int
}

func main() {
	fixtures := flag.String("fixtures", filepath.Join("testdata", "webhooks"), "webhook fixture directory")
	templates := flag.String("templates-dir", "templates", "template sets directory")
	crds := flag.String("crd-dir", filepath.Join("config", "crd"), "CRD directory")
	flag.Parse()
	if err := run(*fixtures, *templates, *crds); err != nil {
		fmt.Fprintln(os.Stderr, "demo failed:", err)
		os.Exit(1)
	}
}

func step(title string)               { fmt.Printf("\n== %s\n", title) }
func info(format string, args ...any) { fmt.Printf("   "+format+"\n", args...) }

func run(fixtures, templates, crds string) error {
	logf.SetLogger(logr.Discard())
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return fmt.Errorf("KUBEBUILDER_ASSETS is not set; run `make demo`")
	}
	step("starting local kube-apiserver + etcd (envtest)")
	env := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		return err
	}
	defer func() { _ = env.Stop() }()
	info("API server at %s, CRD previewenvironments.previewd.dev installed", restCfg.Host)

	gh := newFakeGitHub()
	defer gh.Close()
	info("fake GitHub status API at %s", gh.URL)

	cfg := app.DefaultConfig()
	cfg.TemplatesDir = templates
	cfg.WebhookSecrets = [][]byte{[]byte(secret)}
	cfg.Rules, _ = intake.ParseRules([]string{"acme/*=default"}, "default")
	cfg.PollInterval = 200 * time.Millisecond
	cfg.Retention = time.Hour
	cfg.GitHubAPI = gh.URL
	cfg.GitHubToken = "demo-token"
	cfg.MetricsAddr = freeAddr()
	// The fixtures were "sent" on 2026-10-01 between 09:00 and 09:20; run the
	// controller's clock from just after them so TTLs are meaningful and fast-forwardable.
	clk := clocktesting.NewFakeClock(time.Date(2026, 10, 1, 9, 25, 0, 0, time.UTC))
	a, err := app.New(restCfg, cfg, logr.Discard(), app.Overrides{Clock: clk})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := a.Manager.Start(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "manager:", err)
		}
	}()
	<-a.Webhook.Ready()
	c, err := client.New(restCfg, client.Options{Scheme: a.Manager.GetScheme()})
	if err != nil {
		return err
	}
	go simulateCluster(ctx, c)
	d := &demo{ctx: ctx, c: c, url: "http://" + a.Webhook.BoundAddr().String() + "/webhook", fixtures: fixtures, clock: clk}
	info("previewd running: webhook %s, metrics http://%s/metrics", d.url, cfg.MetricsAddr)

	step("1. GitHub pings the webhook, then an attacker posts an unsigned payload")
	d.sendFile("ping", "00-ping.json", true)
	d.sendFile("pull_request", "01-opened.json", false)

	step("2. PR #42 opened (signed delivery)")
	d.sendFile("pull_request", "01-opened.json", true)
	if err := d.waitPhase(v1alpha1.PhaseReady); err != nil {
		return err
	}
	d.showNamespace()

	step("3. new commit pushed; GitHub redelivers it; an older delivery arrives late")
	d.sendFile("pull_request", "02-synchronize.json", true)
	d.resend("pull_request", "02-synchronize.json")
	d.sendFile("pull_request", "03-stale-synchronize.json", true)
	if err := d.waitFor("Ready on "+sha2[:7], func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseReady && pe.Status.AppliedSHA == sha2
	}); err != nil {
		return err
	}

	step("4. events previewd ignores: an assignment, and a PR from a fork")
	d.sendFile("pull_request", "04-assigned.json", true)
	d.sendFile("pull_request", "05-fork-opened.json", true)

	step("5. nobody touches the PR for 72h: TTL expiry")
	d.fastForward(72 * time.Hour)
	if err := d.waitPhase(v1alpha1.PhaseExpired); err != nil {
		return err
	}

	step("6. a new push revives the expired preview")
	d.sendEvent("synchronize", sha3, "open")
	if err := d.waitFor("Ready on "+sha3[:7], func(pe *v1alpha1.PreviewEnvironment) bool {
		return pe.Status.Phase == v1alpha1.PhaseReady && pe.Status.AppliedSHA == sha3
	}); err != nil {
		return err
	}

	step("7. PR merged and closed")
	d.sendEvent("closed", sha3, "closed")
	if err := d.waitPhase(v1alpha1.PhaseClosed); err != nil {
		return err
	}

	step("8. retention (1h) elapses: the record deletes itself")
	d.fastForward(2 * time.Hour)
	if err := d.waitGone(); err != nil {
		return err
	}

	step("9. orphan collection: a preview namespace whose record was force-deleted")
	orphan := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pr-legacy-7", Labels: map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue, v1alpha1.LabelInstance: "acme-legacy-pr-7",
	}}}
	if err := c.Create(ctx, orphan); err != nil {
		return err
	}
	sweeper := &gc.Collector{Client: c, Grace: 0, Clock: clock.RealClock{}, Log: logr.Discard()}
	deleted, err := sweeper.Sweep(ctx)
	if err != nil {
		return err
	}
	info("collector deleted: %v", deleted)

	step("10. commit statuses GitHub received (POST /repos/{owner}/{repo}/statuses/{sha})")
	for _, s := range gh.Statuses() {
		info("%-40s %-8s %s", s.path, s.State, s.Description)
	}

	step("11. Prometheus metrics (previewd_* excerpt from /metrics)")
	if err := printMetrics(cfg.MetricsAddr); err != nil {
		return err
	}
	fmt.Println("\ndemo complete")
	return nil
}

func freeAddr() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func (d *demo) post(event string, body []byte, signed bool, delivery string) {
	req, _ := http.NewRequestWithContext(d.ctx, http.MethodPost, d.url, bytes.NewReader(body))
	req.Header.Set(webhook.EventHeader, event)
	req.Header.Set(webhook.DeliveryHeader, delivery)
	if signed {
		req.Header.Set(webhook.SignatureHeader, webhook.Sign([]byte(secret), body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		info("POST failed: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	sig := "signed"
	if !signed {
		sig = "UNSIGNED"
	}
	info("%-8s delivery=%-8s %-12s -> HTTP %d %s", sig, delivery, event, resp.StatusCode, strings.TrimSpace(string(out)))
}

func (d *demo) sendFile(event, file string, signed bool) {
	body, err := os.ReadFile(filepath.Join(d.fixtures, file))
	if err != nil {
		panic(err)
	}
	d.deliver++
	d.post(event, body, signed, fmt.Sprintf("d-%02d", d.deliver))
}

func (d *demo) resend(event, file string) {
	body, err := os.ReadFile(filepath.Join(d.fixtures, file))
	if err != nil {
		panic(err)
	}
	d.post(event, body, true, fmt.Sprintf("d-%02d", d.deliver))
}

// sendEvent builds a fresh delivery from the opened fixture with the given
// action, head SHA and state, stamped with the controller's current time.
func (d *demo) sendEvent(action, sha, state string) {
	raw, err := os.ReadFile(filepath.Join(d.fixtures, "01-opened.json"))
	if err != nil {
		panic(err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		panic(err)
	}
	p["action"] = action
	pr := p["pull_request"].(map[string]any)
	pr["state"] = state
	pr["updated_at"] = d.clock.Now().UTC().Format(time.RFC3339)
	pr["head"].(map[string]any)["sha"] = sha
	body, _ := json.Marshal(p)
	d.deliver++
	d.post("pull_request", body, true, fmt.Sprintf("d-%02d", d.deliver))
}

func (d *demo) get() (*v1alpha1.PreviewEnvironment, error) {
	var pe v1alpha1.PreviewEnvironment
	err := d.c.Get(d.ctx, types.NamespacedName{Name: instance}, &pe)
	return &pe, err
}

func (d *demo) waitFor(what string, cond func(*v1alpha1.PreviewEnvironment) bool) error {
	start := time.Now()
	var last v1alpha1.Phase
	for time.Since(start) < 30*time.Second {
		pe, err := d.get()
		if err == nil {
			if pe.Status.Phase != last {
				last = pe.Status.Phase
				info("phase -> %-12s %s", last, pe.Status.Message)
			}
			if cond(pe) {
				info("reached %s in %dms (url=%s, objects=%d, sha=%s)", what,
					time.Since(start).Milliseconds(), pe.Status.URL, pe.Status.Resources, shortSHA(pe.Status.AppliedSHA))
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s (last phase %s)", what, last)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	if s == "" {
		return "-"
	}
	return s
}

func (d *demo) waitPhase(p v1alpha1.Phase) error {
	return d.waitFor(string(p), func(pe *v1alpha1.PreviewEnvironment) bool { return pe.Status.Phase == p })
}

func (d *demo) waitGone() error {
	for range 1500 {
		if _, err := d.get(); apierrors.IsNotFound(err) {
			info("PreviewEnvironment %s deleted", instance)
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("PreviewEnvironment was not deleted")
}

// fastForward advances the controller's clock and touches the record so it
// is reconciled now; in a real deployment the RequeueAfter the reconciler
// returned would fire at exactly that moment.
func (d *demo) fastForward(by time.Duration) {
	d.clock.Step(by)
	info("(fast-forwarding the controller clock by %s to %s)", by, d.clock.Now().UTC().Format(time.RFC3339))
	pe, err := d.get()
	if err != nil {
		return
	}
	patch := client.MergeFrom(pe.DeepCopy())
	if pe.Annotations == nil {
		pe.Annotations = map[string]string{}
	}
	pe.Annotations["demo.previewd.dev/clock"] = d.clock.Now().UTC().Format("20060102T150405")
	_ = d.c.Patch(d.ctx, pe, patch)
}

func (d *demo) showNamespace() {
	var ns corev1.Namespace
	if err := d.c.Get(d.ctx, types.NamespacedName{Name: nsName}, &ns); err != nil {
		info("namespace: %v", err)
		return
	}
	keys := make([]string, 0, len(ns.Labels))
	for k := range ns.Labels {
		if strings.HasPrefix(k, "previewd.dev/") || strings.HasPrefix(k, "pod-security") {
			keys = append(keys, k+"="+ns.Labels[k])
		}
	}
	sort.Strings(keys)
	info("namespace %s labels: %s", ns.Name, strings.Join(keys, " "))
	var lists = []struct {
		kind string
		list client.ObjectList
	}{
		{"ResourceQuota", &corev1.ResourceQuotaList{}}, {"LimitRange", &corev1.LimitRangeList{}},
		{"ConfigMap", &corev1.ConfigMapList{}}, {"Service", &corev1.ServiceList{}},
		{"Deployment", &appsv1.DeploymentList{}}, {"NetworkPolicy", &networkingv1.NetworkPolicyList{}},
		{"Ingress", &networkingv1.IngressList{}},
	}
	var names []string
	for _, l := range lists {
		_ = d.c.List(d.ctx, l.list, client.InNamespace(nsName), client.MatchingLabels{v1alpha1.LabelInstance: instance})
		items, _ := meta.ExtractList(l.list)
		for _, it := range items {
			if m, err := meta.Accessor(it); err == nil {
				names = append(names, l.kind+"/"+m.GetName())
			}
		}
	}
	info("objects: %s", strings.Join(names, ", "))
}

// simulateCluster plays the controllers envtest does not run.
func simulateCluster(ctx context.Context, c client.Client) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	managed := client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var deps appsv1.DeploymentList
		if c.List(ctx, &deps, managed) == nil {
			for i := range deps.Items {
				dep := &deps.Items[i]
				if dep.Status.ObservedGeneration == dep.Generation && dep.Status.AvailableReplicas > 0 {
					continue
				}
				dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
				_ = c.Status().Update(ctx, dep)
			}
		}
		var nss corev1.NamespaceList
		if c.List(ctx, &nss, managed) == nil {
			for i := range nss.Items {
				ns := &nss.Items[i]
				if ns.DeletionTimestamp.IsZero() {
					continue
				}
				ns.Spec.Finalizers = nil
				_ = c.SubResource("finalize").Update(ctx, ns)
			}
		}
	}
}

type fakeGitHub struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []ghStatus
}

type ghStatus struct {
	path        string
	State       string `json:"state"`
	Description string `json:"description"`
}

func newFakeGitHub() *fakeGitHub {
	f := &fakeGitHub{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s ghStatus
		_ = json.NewDecoder(r.Body).Decode(&s)
		parts := strings.Split(r.URL.Path, "/")
		s.path = strings.Join(parts[:len(parts)-1], "/") + "/" + shortSHA(parts[len(parts)-1])
		f.mu.Lock()
		f.statuses = append(f.statuses, s)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	return f
}

func (f *fakeGitHub) Statuses() []ghStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghStatus(nil), f.statuses...)
}

func printMetrics(addr string) error {
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "previewd_") && !strings.Contains(line, "_bucket") {
			info("%s", line)
		}
	}
	return sc.Err()
}

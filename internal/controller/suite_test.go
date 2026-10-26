package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
	"github.com/SlateGitOrg/previewd/internal/metrics"
	"github.com/SlateGitOrg/previewd/internal/render"
	"github.com/SlateGitOrg/previewd/internal/scm"
)

// The envtest suite runs a real kube-apiserver and etcd (no controllers, no
// kubelet). KUBEBUILDER_ASSETS must point at the binaries; `make test`
// resolves it with setup-envtest. Without it the integration tests skip,
// unless PREVIEWD_REQUIRE_ENVTEST=1 (set by `make check`) makes that fatal.
var (
	testCfg    *rest.Config
	testClient client.Client
	testScheme *runtime.Scheme
	envtestErr string
)

func TestMain(m *testing.M) {
	testScheme = runtime.NewScheme()
	must(clientgoscheme.AddToScheme(testScheme))
	must(v1alpha1.AddToScheme(testScheme))
	logf.SetLogger(logr.Discard())

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		envtestErr = "KUBEBUILDER_ASSETS is not set (run `make test` or `make envtest`)"
		if os.Getenv("PREVIEWD_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "envtest required but", envtestErr)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		Scheme:                testScheme,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}
	testCfg = cfg
	testClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	must(err)
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stopping envtest:", err)
	}
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if testCfg == nil {
		t.Skip(envtestErr)
	}
}

var epoch = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// harness drives the reconciler directly (no manager), with an uncached
// client, a fake clock and an in-memory SCM, so every step is deterministic.
type harness struct {
	t     *testing.T
	ctx   context.Context
	r     *Reconciler
	clock *clocktesting.FakeClock
	scm   *scm.Recorder
}

func repoCatalog(t *testing.T) *render.Catalog {
	t.Helper()
	c, err := render.LoadCatalog(os.DirFS(filepath.Join("..", "..")), "templates")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newHarness(t *testing.T, catalog *render.Catalog, opts Options) *harness {
	t.Helper()
	requireEnvtest(t)
	clk := clocktesting.NewFakeClock(epoch.Add(time.Minute))
	rec := &scm.Recorder{}
	if opts.PollInterval == 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Retention == 0 {
		opts.Retention = time.Hour
	}
	r := &Reconciler{
		Client:    testClient,
		APIReader: testClient,
		Catalog:   catalog,
		Reporter:  rec,
		Clock:     clk,
		Phases:    metrics.NewPhaseTracker(metrics.Environments),
		Options:   opts,
	}
	return &harness{t: t, ctx: context.Background(), r: r, clock: clk, scm: rec}
}

const (
	shaA = "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c"
	shaB = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d"
)

// newPE creates a PreviewEnvironment for repo/number.
func (h *harness) newPE(repo string, number int, template string, labels ...string) *v1alpha1.PreviewEnvironment {
	h.t.Helper()
	pe := &v1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("acme-%s-pr-%d", repo, number)},
		Spec: v1alpha1.PreviewEnvironmentSpec{
			Repository: v1alpha1.RepositoryRef{Owner: "acme", Name: repo},
			PullRequest: v1alpha1.PullRequestRef{
				Number: number, HeadSHA: shaA, HeadRef: "feature/x", BaseRef: "main",
				Author: "octocat", Title: "Test PR", Labels: labels,
			},
			State:            v1alpha1.PRStateOpen,
			Template:         template,
			LastActivityTime: metav1.NewTime(epoch),
		},
	}
	if err := testClient.Create(h.ctx, pe); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { h.forceDelete(pe.Name) })
	return pe
}

// forceDelete removes a PreviewEnvironment regardless of its finalizer so
// tests do not leak state into each other.
func (h *harness) forceDelete(name string) {
	var pe v1alpha1.PreviewEnvironment
	if err := testClient.Get(h.ctx, types.NamespacedName{Name: name}, &pe); err != nil {
		return
	}
	pe.Finalizers = nil
	_ = testClient.Update(h.ctx, &pe)
	_ = testClient.Delete(h.ctx, &pe)
}

func (h *harness) reconcile(name string) (time.Duration, error) {
	res, err := h.r.Reconcile(h.ctx, reqFor(name))
	return res.RequeueAfter, err
}

func (h *harness) mustReconcile(name string) time.Duration {
	h.t.Helper()
	d, err := h.reconcile(name)
	if err != nil {
		h.t.Fatalf("reconcile %s: %v", name, err)
	}
	return d
}

func (h *harness) get(name string) *v1alpha1.PreviewEnvironment {
	h.t.Helper()
	var pe v1alpha1.PreviewEnvironment
	if err := testClient.Get(h.ctx, types.NamespacedName{Name: name}, &pe); err != nil {
		h.t.Fatalf("get %s: %v", name, err)
	}
	return &pe
}

func (h *harness) gone(name string) bool {
	var pe v1alpha1.PreviewEnvironment
	err := testClient.Get(h.ctx, types.NamespacedName{Name: name}, &pe)
	return apierrors.IsNotFound(err)
}

func (h *harness) namespace(name string) (*corev1.Namespace, bool) {
	h.t.Helper()
	var ns corev1.Namespace
	err := testClient.Get(h.ctx, types.NamespacedName{Name: name}, &ns)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return &ns, true
}

// finalizeNamespace plays the role of the namespace controller, which
// envtest does not run: it clears spec.finalizers so a terminating namespace
// is actually removed.
func (h *harness) finalizeNamespace(name string) {
	h.t.Helper()
	ns, ok := h.namespace(name)
	if !ok {
		return
	}
	ns.Spec.Finalizers = nil
	if err := testClient.SubResource("finalize").Update(h.ctx, ns); err != nil {
		h.t.Fatalf("finalize namespace %s: %v", name, err)
	}
	if _, still := h.namespace(name); still {
		h.t.Fatalf("namespace %s still present after finalize", name)
	}
}

// markAvailable plays the role of the deployment controller.
func (h *harness) markAvailable(namespace, name string) {
	h.t.Helper()
	var d appsv1.Deployment
	if err := testClient.Get(h.ctx, types.NamespacedName{Namespace: namespace, Name: name}, &d); err != nil {
		h.t.Fatal(err)
	}
	want := replicas(d.Spec.Replicas)
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Generation, Replicas: want, UpdatedReplicas: want,
		ReadyReplicas: want, AvailableReplicas: want,
	}
	if err := testClient.Status().Update(h.ctx, &d); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) reports() []string {
	var out []string
	for _, s := range h.scm.Statuses() {
		out = append(out, string(s.State)+"@"+s.SHA[:7])
	}
	return out
}

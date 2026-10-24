package controller

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/metrics"
	"github.com/archit2501/previewd/internal/render"
	"github.com/archit2501/previewd/internal/scm"
)

// TestFailedPreviewsBackOffUnderManager runs the reconciler inside a real
// controller-runtime manager, with the watches SetupWithManager registers.
// A Failed preview writes status on every attempt; those status writes must
// not wake the reconciler again, or the 5s..5m backoff never takes effect
// and one broken preview patches the API server in a tight loop. The
// harness-driven tests call Reconcile directly and cannot see this.
func TestFailedPreviewsBackOffUnderManager(t *testing.T) {
	requireEnvtest(t)
	fsys := fstest.MapFS{
		"t/broken/templateset.yaml": {Data: []byte("namespacePrefix: mgr\n")},
		// Passes previewd's checks, rejected by the API server (no selector).
		"t/broken/manifests/bad.yaml": {Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: bad}\nspec:\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n      containers: [{name: c, image: i}]\n")},
	}
	catalog, err := render.LoadCatalog(fsys, "t")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, catalog, Options{})

	skip := true
	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  CacheOptions(),
		Controller:             ctrlcfg.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &scm.Recorder{}
	r := &Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Catalog:   catalog,
		Reporter:  rec,
		Clock:     clocktesting.NewFakeClock(epoch.Add(time.Minute)),
		Phases:    metrics.NewPhaseTracker(metrics.Environments),
		Options:   Options{PollInterval: 200 * time.Millisecond, Retention: time.Hour},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}

	renderFail := h.newPE("mgrmissing", 120, "does-not-exist")
	applyFail := h.newPE("mgrbroken", 121, "broken")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	waitFailed := func(name string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if pe := h.get(name); pe.Status.Phase == v1alpha1.PhaseFailed {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s never reached Failed: %+v", name, h.get(name).Status)
	}
	waitFailed(renderFail.Name)
	waitFailed(applyFail.Name)

	// Backoff after the first failure is 5s, so 3s later no further attempt
	// is due. Allow one extra attempt for watch events caused by the first
	// one (e.g. the namespace a partial apply created). Before the fix the
	// render-failure count was in the hundreds after 3s.
	time.Sleep(3 * time.Second)
	for _, name := range []string{renderFail.Name, applyFail.Name} {
		if n := h.get(name).Status.FailureCount; n < 1 || n > 2 {
			t.Errorf("%s: failureCount %d after 3s; backoff is not being honoured", name, n)
		}
	}
	// The backoff still fires: the 5s retry has happened 6.5s in, and the
	// next one (10s later) has not.
	time.Sleep(3500 * time.Millisecond)
	for _, name := range []string{renderFail.Name, applyFail.Name} {
		if n := h.get(name).Status.FailureCount; n < 2 || n > 3 {
			t.Errorf("%s: failureCount %d after 6.5s; want the 5s retry and no more", name, n)
		}
	}
	if got := len(rec.Statuses()); got != 2 {
		t.Errorf("SCM statuses %d, want one failure per preview", got)
	}
}

func TestSpecOrDeletionPredicate(t *testing.T) {
	p := specOrDeletion()
	pe := func(gen int64, deleting bool, phase v1alpha1.Phase) *v1alpha1.PreviewEnvironment {
		o := &v1alpha1.PreviewEnvironment{}
		o.Generation = gen
		o.Status.Phase = phase
		if deleting {
			now := metav1.Now()
			o.DeletionTimestamp = &now
		}
		return o
	}
	cases := []struct {
		name     string
		old, new *v1alpha1.PreviewEnvironment
		want     bool
	}{
		{"status only", pe(1, false, v1alpha1.PhasePending), pe(1, false, v1alpha1.PhaseFailed), false},
		{"spec change", pe(1, false, ""), pe(2, false, ""), true},
		{"deletion requested", pe(1, false, ""), pe(1, true, ""), true},
		{"status while deleting", pe(1, true, v1alpha1.PhaseTearingDown), pe(1, true, v1alpha1.PhaseClosed), false},
	}
	for _, c := range cases {
		if got := p.Update(event.UpdateEvent{ObjectOld: c.old, ObjectNew: c.new}); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if !p.Create(event.CreateEvent{Object: pe(1, false, "")}) || !p.Delete(event.DeleteEvent{Object: pe(1, true, "")}) {
		t.Error("create and delete events must pass")
	}
	if deletionRequested.Update(event.UpdateEvent{}) {
		t.Error("nil objects must not pass")
	}
}

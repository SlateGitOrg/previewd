package controller

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/render"
)

func reqFor(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func TestProvisionThenReady(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("shop", 101, "default")

	requeue := h.mustReconcile(pe.Name)
	if requeue != 2*time.Second {
		t.Fatalf("requeue %v", requeue)
	}
	got := h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseProvisioning || got.Status.Namespace != "pr-shop-101" {
		t.Fatalf("status %+v", got.Status)
	}
	if !strings.Contains(strings.Join(got.Finalizers, ","), v1alpha1.Finalizer) {
		t.Fatal("finalizer not added")
	}
	ns, ok := h.namespace("pr-shop-101")
	if !ok {
		t.Fatal("namespace not created")
	}
	if ns.Labels[v1alpha1.LabelInstance] != pe.Name || ns.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Fatalf("namespace labels %v", ns.Labels)
	}
	if refs := ns.OwnerReferences; len(refs) != 1 || refs[0].Name != pe.Name || refs[0].Controller == nil || !*refs[0].Controller {
		t.Fatalf("owner refs %+v", refs)
	}
	var netpols networkingv1.NetworkPolicyList
	if err := testClient.List(h.ctx, &netpols, client.InNamespace("pr-shop-101")); err != nil {
		t.Fatal(err)
	}
	if len(netpols.Items) != 4 {
		t.Fatalf("network policies %d", len(netpols.Items))
	}
	var quota corev1.ResourceQuota
	if err := testClient.Get(h.ctx, types.NamespacedName{Namespace: "pr-shop-101", Name: render.QuotaName}, &quota); err != nil {
		t.Fatal(err)
	}
	if quota.Spec.Hard.Pods().String() != "20" {
		t.Fatalf("quota %v", quota.Spec.Hard)
	}
	if strings.Join(h.reports(), ",") != "pending@4f2a9c1" {
		t.Fatalf("reports %v", h.reports())
	}

	// Workloads not yet available: stays Provisioning, no duplicate report.
	h.mustReconcile(pe.Name)
	if h.get(pe.Name).Status.Phase != v1alpha1.PhaseProvisioning || len(h.reports()) != 1 {
		t.Fatalf("phase %s reports %v", h.get(pe.Name).Status.Phase, h.reports())
	}

	h.markAvailable("pr-shop-101", "web")
	requeue = h.mustReconcile(pe.Name)
	got = h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase %s: %s", got.Status.Phase, got.Status.Message)
	}
	// Requeue exactly at TTL expiry: 72h after lastActivity, clock is +1m.
	if requeue != 72*time.Hour-time.Minute {
		t.Fatalf("requeue %v", requeue)
	}
	statuses := h.scm.Statuses()
	last := statuses[len(statuses)-1]
	if last.State != "ready" || last.URL != "https://pr-shop-101.preview.example.test" {
		t.Fatalf("last status %+v", last)
	}
	for _, c := range got.Status.Conditions {
		if c.Status != metav1.ConditionTrue {
			t.Fatalf("condition %s is %s: %s", c.Type, c.Status, c.Message)
		}
	}
}

// TestReconcileIsIdempotent: once converged, further reconciles change
// nothing — no writes to any object (resourceVersions are stable), no new
// SCM statuses, identical status.
func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("idem", 102, "default")
	h.mustReconcile(pe.Name)
	h.markAvailable("pr-idem-102", "web")
	h.mustReconcile(pe.Name)
	h.mustReconcile(pe.Name)

	snapshot := func() map[string]string {
		out := map[string]string{}
		pe := h.get(pe.Name)
		out["pe"] = pe.ResourceVersion
		ns, _ := h.namespace("pr-idem-102")
		out["ns"] = ns.ResourceVersion
		lists := []client.ObjectList{&corev1.ConfigMapList{}, &corev1.ServiceList{}, &appsv1.DeploymentList{},
			&networkingv1.NetworkPolicyList{}, &networkingv1.IngressList{}, &corev1.ResourceQuotaList{}, &corev1.LimitRangeList{}}
		for _, l := range lists {
			if err := testClient.List(h.ctx, l, client.InNamespace("pr-idem-102")); err != nil {
				t.Fatal(err)
			}
			switch v := l.(type) {
			case *corev1.ConfigMapList:
				for _, i := range v.Items {
					out["cm/"+i.Name] = i.ResourceVersion
				}
			case *corev1.ServiceList:
				for _, i := range v.Items {
					out["svc/"+i.Name] = i.ResourceVersion
				}
			case *appsv1.DeploymentList:
				for _, i := range v.Items {
					out["deploy/"+i.Name] = i.ResourceVersion
				}
			case *networkingv1.NetworkPolicyList:
				for _, i := range v.Items {
					out["np/"+i.Name] = i.ResourceVersion
				}
			case *networkingv1.IngressList:
				for _, i := range v.Items {
					out["ing/"+i.Name] = i.ResourceVersion
				}
			case *corev1.ResourceQuotaList:
				for _, i := range v.Items {
					out["quota/"+i.Name] = i.ResourceVersion
				}
			case *corev1.LimitRangeList:
				for _, i := range v.Items {
					out["lr/"+i.Name] = i.ResourceVersion
				}
			}
		}
		return out
	}
	before := snapshot()
	reportsBefore := len(h.reports())
	if len(before) < 10 {
		t.Fatalf("expected the full bundle, got %v", before)
	}
	for range 5 {
		h.mustReconcile(pe.Name)
	}
	after := snapshot()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %s -> %s", k, v, after[k])
		}
	}
	if len(h.reports()) != reportsBefore {
		t.Fatalf("idempotent reconciles posted statuses: %v", h.reports())
	}
	if h.get(pe.Name).Status.Phase != v1alpha1.PhaseReady {
		t.Fatal("phase drifted")
	}
}

func TestNewPushReappliesAndPrunes(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("site", 103, "static-site", "preview:worker")
	h.mustReconcile(pe.Name)
	var worker appsv1.Deployment
	if err := testClient.Get(h.ctx, types.NamespacedName{Namespace: "site-site-103", Name: "worker"}, &worker); err != nil {
		t.Fatalf("worker not created: %v", err)
	}

	// New push without the worker label.
	got := h.get(pe.Name)
	got.Spec.PullRequest.HeadSHA = shaB
	got.Spec.PullRequest.Labels = nil
	got.Spec.LastActivityTime = metav1.NewTime(epoch.Add(time.Minute))
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	err := testClient.Get(h.ctx, types.NamespacedName{Namespace: "site-site-103", Name: "worker"}, &worker)
	if !apierrors.IsNotFound(err) && (err != nil || worker.DeletionTimestamp.IsZero()) {
		t.Fatalf("worker not pruned: %v", err)
	}
	var site appsv1.Deployment
	if err := testClient.Get(h.ctx, types.NamespacedName{Namespace: "site-site-103", Name: "site"}, &site); err != nil {
		t.Fatal(err)
	}
	if site.Annotations[v1alpha1.AnnotationSHA] != shaB {
		t.Fatalf("site not re-applied for new SHA: %v", site.Annotations)
	}
	if strings.Join(h.reports(), ",") != "pending@4f2a9c1,pending@9e8d7c6" {
		t.Fatalf("reports %v", h.reports())
	}
	if h.get(pe.Name).Status.AppliedSHA != shaB {
		t.Fatal("appliedSHA not updated")
	}
}

func TestCloseTearsDownThenRetentionDeletesRecord(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("close", 104, "default")
	h.mustReconcile(pe.Name)

	got := h.get(pe.Name)
	got.Spec.State = v1alpha1.PRStateClosed
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	ns, ok := h.namespace("pr-close-104")
	if !ok || ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace not being deleted")
	}
	if h.get(pe.Name).Status.Phase != v1alpha1.PhaseTearingDown {
		t.Fatalf("phase %s", h.get(pe.Name).Status.Phase)
	}
	// Still terminating: waits quietly.
	h.mustReconcile(pe.Name)
	h.finalizeNamespace("pr-close-104")
	requeue := h.mustReconcile(pe.Name)
	got = h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseClosed || got.Status.TerminatedAt == nil || requeue != time.Hour {
		t.Fatalf("status %+v requeue %v", got.Status, requeue)
	}
	if strings.Join(h.reports(), ",") != "pending@4f2a9c1,inactive@4f2a9c1" {
		t.Fatalf("reports %v", h.reports())
	}

	// Retention elapses: the record deletes itself, then the finalizer is
	// released on the next reconcile.
	h.clock.Step(2 * time.Hour)
	h.mustReconcile(pe.Name)
	h.mustReconcile(pe.Name)
	if !h.gone(pe.Name) {
		t.Fatal("record not deleted after retention")
	}
	// Reconciling a deleted record is a no-op.
	h.mustReconcile(pe.Name)
}

func TestTTLExpiryAndRevivalOnPush(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("ttl", 105, "default")
	got := h.get(pe.Name)
	got.Spec.TTL = &metav1.Duration{Duration: 30 * time.Minute}
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	if exp := h.get(pe.Name).Status.ExpiresAt; exp == nil || !exp.Time.Equal(epoch.Add(30*time.Minute)) {
		t.Fatalf("expiresAt %v", exp)
	}

	h.clock.Step(time.Hour)
	h.mustReconcile(pe.Name)
	if h.get(pe.Name).Status.Phase != v1alpha1.PhaseTearingDown {
		t.Fatal("not tearing down after TTL")
	}
	h.finalizeNamespace("pr-ttl-105")
	h.mustReconcile(pe.Name)
	if h.get(pe.Name).Status.Phase != v1alpha1.PhaseExpired {
		t.Fatalf("phase %s", h.get(pe.Name).Status.Phase)
	}
	if r := h.reports(); r[len(r)-1] != "expired@4f2a9c1" {
		t.Fatalf("reports %v", r)
	}

	// A new push renews the TTL and brings the environment back.
	got = h.get(pe.Name)
	got.Spec.PullRequest.HeadSHA = shaB
	got.Spec.LastActivityTime = metav1.NewTime(h.clock.Now())
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	got = h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseProvisioning || got.Status.TerminatedAt != nil {
		t.Fatalf("status %+v", got.Status)
	}
	if _, ok := h.namespace("pr-ttl-105"); !ok {
		t.Fatal("namespace not recreated")
	}
}

func TestDeletionReleasesFinalizerOnceNamespaceTerminates(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("del", 106, "default")
	h.mustReconcile(pe.Name)
	if err := testClient.Delete(h.ctx, h.get(pe.Name)); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	if ns, ok := h.namespace("pr-del-106"); !ok || ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace not deleted")
	}
	if h.gone(pe.Name) {
		t.Fatal("finalizer released before namespace deletion was observed")
	}
	h.mustReconcile(pe.Name)
	if !h.gone(pe.Name) {
		t.Fatal("record not released")
	}
}

func TestForeignNamespaceIsNeverAdoptedOrDeleted(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pr-squat-107", Labels: map[string]string{"team": "payments"}}}
	if err := testClient.Create(h.ctx, foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(h.ctx, foreign) })
	pe := h.newPE("squat", 107, "default")
	h.mustReconcile(pe.Name)
	got := h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(got.Status.Message, "not managed by this preview") {
		t.Fatalf("status %+v", got.Status)
	}
	ns, _ := h.namespace("pr-squat-107")
	if ns.Labels[v1alpha1.LabelManagedBy] != "" {
		t.Fatal("foreign namespace was adopted")
	}
	got.Spec.State = v1alpha1.PRStateClosed
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	if ns, ok := h.namespace("pr-squat-107"); !ok || !ns.DeletionTimestamp.IsZero() {
		t.Fatal("foreign namespace was deleted")
	}
	if h.get(pe.Name).Status.Phase != v1alpha1.PhaseClosed {
		t.Fatalf("phase %s", h.get(pe.Name).Status.Phase)
	}
}

func TestMissingTemplateFailsWithBackoff(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("nott", 108, "does-not-exist")
	first := h.mustReconcile(pe.Name)
	second := h.mustReconcile(pe.Name)
	got := h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseFailed || got.Status.FailureCount != 2 || second <= first {
		t.Fatalf("status %+v backoff %v -> %v", got.Status, first, second)
	}
	if strings.Join(h.reports(), ",") != "failed@4f2a9c1" {
		t.Fatalf("reports %v", h.reports())
	}
	if _, ok := h.namespace("pr-nott-108"); ok {
		t.Fatal("failed render created a namespace")
	}
}

func TestAPIServerRejectionIsApplyFailed(t *testing.T) {
	fsys := fstest.MapFS{
		"t/broken/templateset.yaml": {Data: []byte("namespacePrefix: brk\n")},
		// Valid YAML that passes previewd's checks but that the API server
		// rejects: a Deployment without a selector.
		"t/broken/manifests/bad.yaml": {Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: bad}\nspec:\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n      containers: [{name: c, image: i}]\n")},
	}
	catalog, err := render.LoadCatalog(fsys, "t")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, catalog, Options{})
	pe := h.newPE("broken", 109, "broken")
	requeue := h.mustReconcile(pe.Name)
	got := h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(got.Status.Message, "Deployment/bad") || requeue != 5*time.Second {
		t.Fatalf("status %+v requeue %v", got.Status, requeue)
	}
	// The failed apply replaces the pending report: the SCM never sees a
	// pending status for an attempt that already failed.
	if strings.Join(h.reports(), ",") != "failed@4f2a9c1" {
		t.Fatalf("reports %v", h.reports())
	}
	// The next attempt fails again without re-posting the same failure,
	// while the failure count keeps growing for backoff.
	if requeue := h.mustReconcile(pe.Name); requeue != 10*time.Second {
		t.Fatalf("second backoff %v", requeue)
	}
	if len(h.reports()) != 1 || h.get(pe.Name).Status.FailureCount != 2 {
		t.Fatalf("reports %v count %d", h.reports(), h.get(pe.Name).Status.FailureCount)
	}
	for _, c := range h.get(pe.Name).Status.Conditions {
		if c.Type == v1alpha1.ConditionApplied && (c.Status != metav1.ConditionFalse || c.Reason != "ApplyFailed") {
			t.Fatalf("applied condition %+v", c)
		}
	}
}

func TestRequiredLabelGatesPreview(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{RequiredLabel: "preview", SkipDrafts: true})
	pe := h.newPE("gate", 110, "default")
	h.mustReconcile(pe.Name)
	got := h.get(pe.Name)
	if got.Status.Phase != v1alpha1.PhaseClosed || !strings.Contains(got.Status.Message, `label "preview"`) {
		t.Fatalf("status %+v", got.Status)
	}
	if _, ok := h.namespace("pr-gate-110"); ok {
		t.Fatal("gated preview created a namespace")
	}
	got.Spec.PullRequest.Labels = []string{"preview"}
	got.Spec.PullRequest.Draft = true
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	if msg := h.get(pe.Name).Status.Message; !strings.Contains(msg, "draft") {
		t.Fatalf("message %q", msg)
	}
	got = h.get(pe.Name)
	got.Spec.PullRequest.Draft = false
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	if h.get(pe.Name).Status.Phase != v1alpha1.PhaseProvisioning {
		t.Fatalf("phase %s", h.get(pe.Name).Status.Phase)
	}
}

func TestReopenWaitsForTerminatingNamespace(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	pe := h.newPE("reopen", 111, "default")
	h.mustReconcile(pe.Name)
	got := h.get(pe.Name)
	got.Spec.State = v1alpha1.PRStateClosed
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name) // namespace now terminating
	got = h.get(pe.Name)
	got.Spec.State = v1alpha1.PRStateOpen
	if err := testClient.Update(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile(pe.Name)
	if p := h.get(pe.Name).Status.Phase; p != v1alpha1.PhasePending {
		t.Fatalf("phase %s", p)
	}
	h.finalizeNamespace("pr-reopen-111")
	h.mustReconcile(pe.Name)
	if p := h.get(pe.Name).Status.Phase; p != v1alpha1.PhaseProvisioning {
		t.Fatalf("phase %s", p)
	}
	if ns, ok := h.namespace("pr-reopen-111"); !ok || !ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace not recreated")
	}
}

func TestReportFailureIsRetried(t *testing.T) {
	h := newHarness(t, repoCatalog(t), Options{})
	h.scm.FailNext(1, errors.New("github is down"))
	pe := h.newPE("flaky", 112, "default")
	requeue := h.mustReconcile(pe.Name)
	if h.get(pe.Name).Status.LastReported != nil || requeue != 2*time.Second {
		t.Fatalf("failed report recorded as sent (requeue %v)", requeue)
	}
	h.mustReconcile(pe.Name)
	if lr := h.get(pe.Name).Status.LastReported; lr == nil || lr.State != "pending" {
		t.Fatalf("report not retried: %+v", lr)
	}
	if len(h.reports()) != 1 {
		t.Fatalf("reports %v", h.reports())
	}
}

func TestReadinessHelpers(t *testing.T) {
	two := int32(2)
	d := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &two}}
	d.Generation = 3
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2}
	if DeploymentReady(d) {
		t.Fatal("stale generation counted as ready")
	}
	d.Status.ObservedGeneration = 3
	if !DeploymentReady(d) {
		t.Fatal("rolled-out deployment not ready")
	}
	d.Status.Replicas = 3 // old replica still around mid-rollout
	if DeploymentReady(d) {
		t.Fatal("mid-rollout counted as ready")
	}
	s := &appsv1.StatefulSet{}
	s.Status = appsv1.StatefulSetStatus{UpdatedReplicas: 1, ReadyReplicas: 0}
	if StatefulSetReady(s) {
		t.Fatal("unready statefulset")
	}
	s.Status.ReadyReplicas = 1
	if !StatefulSetReady(s) {
		t.Fatal("ready statefulset")
	}
	if minDuration(0, time.Second) != time.Second || minDuration(3*time.Second, time.Second) != time.Second || minDuration(time.Second, 3*time.Second) != time.Second {
		t.Fatal("minDuration")
	}
	if phaseOrNone("") != "<none>" {
		t.Fatal("phaseOrNone")
	}
	if byInstanceLabel(t.Context(), &corev1.Namespace{}) != nil {
		t.Fatal("unlabelled object enqueued")
	}
	if ManagedSelector().String() != "app.kubernetes.io/managed-by=previewd" {
		t.Fatal(ManagedSelector().String())
	}
}

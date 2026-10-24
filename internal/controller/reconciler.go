// Package controller wires the lifecycle state machine to Kubernetes with
// controller-runtime: it observes PreviewEnvironments, their namespaces and
// workloads, asks lifecycle.Decide what to do, performs the side effects and
// records the outcome in status.
package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/lifecycle"
	"github.com/archit2501/previewd/internal/metrics"
	"github.com/archit2501/previewd/internal/naming"
	"github.com/archit2501/previewd/internal/render"
	"github.com/archit2501/previewd/internal/scm"
)

// FieldOwner is the server-side-apply field manager previewd applies as.
const FieldOwner = "previewd"

// Options tune the reconciler.
type Options struct {
	// PollInterval is how soon to look again while waiting on the cluster.
	PollInterval time.Duration
	// Retention keeps Closed/Expired records this long; zero keeps them.
	Retention time.Duration
	// RequiredLabel, if set, must be on the PR for a preview to exist.
	RequiredLabel string
	// SkipDrafts tears down (or never creates) previews of draft PRs.
	SkipDrafts bool
	// MaxConcurrentReconciles bounds parallel reconciles (default 4).
	MaxConcurrentReconciles int
}

// Reconciler reconciles PreviewEnvironments.
type Reconciler struct {
	// Client is the (cached) manager client.
	Client client.Client
	// APIReader bypasses the cache. Namespace lookups use it so a
	// namespace previewd does not manage is never mistaken for absent.
	APIReader client.Reader
	Catalog   *render.Catalog
	Reporter  scm.Reporter
	Clock     clock.PassiveClock
	Events    events.EventRecorder
	Phases    *metrics.PhaseTracker
	Options   Options
}

// observed is what one reconcile learned about the world, beyond the
// lifecycle.Observation it feeds to Decide.
type observed struct {
	bundle    *render.Bundle
	namespace string
	expiresAt time.Time
	foreignNS bool
	// expiryLabel is the namespace's current expiry label value.
	expiryLabel string
}

// RBAC for the controller. Namespaces are created dynamically, so the
// namespaced kinds a template set may contain need cluster-wide verbs; see
// docs/THREAT_MODEL.md for the consequences and mitigations.
//
// +kubebuilder:rbac:groups=previewd.dev,resources=previewenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=previewd.dev,resources=previewenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=previewd.dev,resources=previewenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas;limitranges;configmaps;secrets;services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	// Read the PreviewEnvironment uncached. A reconcile triggered by an
	// owned-object event can otherwise run against a cached copy that does
	// not yet include the status written by the previous reconcile, and
	// would re-post an SCM status that was already sent.
	var pe v1alpha1.PreviewEnvironment
	if err := r.APIReader.Get(ctx, req.NamespacedName, &pe); err != nil {
		if apierrors.IsNotFound(err) {
			r.Phases.Forget(req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if pe.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&pe, v1alpha1.Finalizer) {
		patch := client.MergeFrom(pe.DeepCopy())
		controllerutil.AddFinalizer(&pe, v1alpha1.Finalizer)
		if err := r.Client.Patch(ctx, &pe, patch); err != nil {
			return reconcile.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	obs, extra, err := r.observe(ctx, &pe)
	if err != nil {
		return reconcile.Result{}, err
	}
	d := lifecycle.Decide(obs)
	logger.V(1).Info("decided", "phase", d.Phase, "reason", d.Reason, "apply", d.Apply,
		"deleteNamespace", d.DeleteNamespace, "release", d.ReleaseFinalizer, "deleteSelf", d.DeleteSelf)

	original := pe.DeepCopy()
	status := &pe.Status
	status.Namespace = extra.namespace
	if !extra.expiresAt.IsZero() {
		status.ExpiresAt = &metav1.Time{Time: extra.expiresAt}
	}
	if extra.bundle != nil {
		status.URL = extra.bundle.Values.URL
	}

	if d.Apply {
		if err := r.apply(ctx, &pe, extra.bundle); err != nil {
			logger.Error(err, "apply failed")
			d = applyFailed(obs, err)
		} else {
			status.AppliedHash = extra.bundle.Hash
			status.AppliedSHA = pe.Spec.PullRequest.HeadSHA
			status.Resources = clampInt32(len(extra.bundle.Objects) + 1)
		}
	}
	if (d.Phase == v1alpha1.PhaseProvisioning || d.Phase == v1alpha1.PhaseReady) &&
		(obs.Namespace == lifecycle.NamespaceActive || d.Apply) {
		if err := r.ensureExpiryLabel(ctx, extra.namespace, extra.expiresAt, extra.expiryLabel); err != nil {
			return reconcile.Result{}, err
		}
	}
	if d.DeleteNamespace && !extra.foreignNS {
		if err := r.deleteNamespace(ctx, extra.namespace); err != nil {
			return reconcile.Result{}, err
		}
	}
	if d.Report != nil {
		if err := r.report(ctx, &pe, *d.Report, d.Message); err != nil {
			logger.Error(err, "status report failed; will retry", "state", *d.Report)
			d.RequeueAfter = minDuration(d.RequeueAfter, obs.PollInterval)
		} else {
			status.LastReported = &v1alpha1.ReportedStatus{SHA: pe.Spec.PullRequest.HeadSHA, State: string(*d.Report)}
		}
	}

	r.updateStatus(&pe, d, obs, extra)
	// A converged environment produces an identical status; skipping the
	// empty patch makes a quiet reconcile cost reads only.
	if !equality.Semantic.DeepEqual(original.Status, pe.Status) {
		if err := r.Client.Status().Patch(ctx, &pe, client.MergeFrom(original)); err != nil {
			if apierrors.IsNotFound(err) {
				return reconcile.Result{}, nil
			}
			return reconcile.Result{}, fmt.Errorf("update status: %w", err)
		}
	}
	r.recordTransition(&pe, original.Status.Phase, d)

	if d.ReleaseFinalizer {
		patch := client.MergeFrom(pe.DeepCopy())
		controllerutil.RemoveFinalizer(&pe, v1alpha1.Finalizer)
		if err := r.Client.Patch(ctx, &pe, patch); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
		r.Phases.Forget(pe.Name)
		return reconcile.Result{}, nil
	}
	if d.DeleteSelf {
		if err := r.Client.Delete(ctx, &pe); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("delete expired record: %w", err)
		}
		return reconcile.Result{}, nil
	}
	return reconcile.Result{RequeueAfter: d.RequeueAfter}, nil
}

// observe builds the lifecycle observation for pe.
func (r *Reconciler) observe(ctx context.Context, pe *v1alpha1.PreviewEnvironment) (lifecycle.Observation, observed, error) {
	now := r.Clock.Now()
	wanted, why := r.wanted(pe)
	obs := lifecycle.Observation{
		Now:             now,
		Deleting:        !pe.DeletionTimestamp.IsZero(),
		Wanted:          wanted,
		NotWantedReason: why,
		AppliedHash:     pe.Status.AppliedHash,
		HeadSHA:         pe.Spec.PullRequest.HeadSHA,
		FailureCount:    pe.Status.FailureCount,
		Retention:       r.Options.Retention,
		PollInterval:    r.pollInterval(),
	}
	if lr := pe.Status.LastReported; lr != nil {
		obs.LastReported = &lifecycle.Reported{SHA: lr.SHA, State: lifecycle.ReportState(lr.State)}
	}
	if pe.Status.TerminatedAt != nil {
		t := pe.Status.TerminatedAt.Time
		obs.TerminatedAt = &t
	}

	var extra observed
	ts, ok := r.Catalog.Get(pe.Spec.Template)
	ttl := render.DefaultTTL
	if ok {
		ttl = ts.TTL()
	}
	if pe.Spec.TTL != nil && pe.Spec.TTL.Duration > 0 {
		ttl = pe.Spec.TTL.Duration
	}
	extra.expiresAt = pe.Spec.LastActivityTime.Add(ttl)
	obs.ExpiresAt = extra.expiresAt

	extra.namespace = pe.Status.Namespace
	if ok {
		extra.namespace = naming.Namespace(ts.Config.NamespacePrefix, pe.Spec.Repository.Name, pe.Spec.PullRequest.Number)
	}
	switch {
	case !ok:
		obs.RenderError = fmt.Sprintf("template set %q not found (available: %s)", pe.Spec.Template, strings.Join(r.Catalog.Names(), ", "))
	case obs.Wanted && !obs.Deleting && !obs.Expired():
		b, err := ts.Render(render.InputFrom(pe))
		if err != nil {
			obs.RenderError = err.Error()
		} else {
			extra.bundle = b
			obs.DesiredHash = b.Hash
		}
	}

	nsState, foreign, expiryLabel, err := r.namespaceState(ctx, extra.namespace, pe.Name)
	if err != nil {
		return obs, extra, err
	}
	extra.expiryLabel = expiryLabel
	obs.Namespace = nsState
	extra.foreignNS = foreign
	if foreign {
		// Never adopt or delete a namespace someone else owns. For teardown
		// it is as good as absent; for provisioning it is a hard error.
		obs.Namespace = lifecycle.NamespaceAbsent
		if obs.Wanted && !obs.Deleting && !obs.Expired() {
			obs.RenderError = fmt.Sprintf("namespace %q already exists and is not managed by this preview", extra.namespace)
		}
	}

	if extra.bundle != nil && obs.Namespace == lifecycle.NamespaceActive && obs.AppliedHash == obs.DesiredHash {
		ready, err := r.workloadsReady(ctx, extra.namespace, extra.bundle)
		if err != nil {
			return obs, extra, err
		}
		obs.WorkloadsReady = ready
	}
	return obs, extra, nil
}

func (r *Reconciler) pollInterval() time.Duration {
	if r.Options.PollInterval > 0 {
		return r.Options.PollInterval
	}
	return 10 * time.Second
}

// wanted applies the gating policy to the PR facts in the spec.
func (r *Reconciler) wanted(pe *v1alpha1.PreviewEnvironment) (bool, string) {
	if pe.Spec.State != v1alpha1.PRStateOpen {
		return false, "pull request closed"
	}
	if r.Options.SkipDrafts && pe.Spec.PullRequest.Draft {
		return false, "pull request is a draft"
	}
	if l := r.Options.RequiredLabel; l != "" {
		for _, have := range pe.Spec.PullRequest.Labels {
			if have == l {
				return true, ""
			}
		}
		return false, fmt.Sprintf("label %q is not set", l)
	}
	return true, ""
}

// namespaceState reads the namespace straight from the API server. foreign
// is true when it exists but belongs to something other than instance.
// The returned label is the namespace's current expiry label.
func (r *Reconciler) namespaceState(ctx context.Context, name, instance string) (lifecycle.NamespaceState, bool, string, error) {
	if name == "" {
		return lifecycle.NamespaceAbsent, false, "", nil
	}
	var ns corev1.Namespace
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return lifecycle.NamespaceAbsent, false, "", nil
		}
		return lifecycle.NamespaceAbsent, false, "", fmt.Errorf("get namespace %s: %w", name, err)
	}
	if ns.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue || ns.Labels[v1alpha1.LabelInstance] != instance {
		return lifecycle.NamespaceActive, true, "", nil
	}
	if !ns.DeletionTimestamp.IsZero() {
		return lifecycle.NamespaceTerminating, false, "", nil
	}
	return lifecycle.NamespaceActive, false, ns.Labels[v1alpha1.LabelExpiresAt], nil
}

// ensureExpiryLabel keeps the namespace's expiry label (unix seconds) in
// step with the TTL. It is applied by its own field manager, outside the
// bundle, so a TTL bump never re-applies the environment or flips the SCM
// status back to pending.
func (r *Reconciler) ensureExpiryLabel(ctx context.Context, namespace string, expiresAt time.Time, current string) error {
	want := strconv.FormatInt(expiresAt.Unix(), 10)
	if current == want {
		return nil
	}
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(namespace)
	ns.SetLabels(map[string]string{v1alpha1.LabelExpiresAt: want})
	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(ns), client.FieldOwner(FieldOwner+"-ttl"), client.ForceOwnership); err != nil {
		return fmt.Errorf("label namespace %s with expiry: %w", namespace, err)
	}
	return nil
}

// workloadsReady reports whether every Deployment and StatefulSet in the
// bundle has rolled out the current spec.
func (r *Reconciler) workloadsReady(ctx context.Context, namespace string, b *render.Bundle) (bool, error) {
	for _, w := range b.Workloads() {
		key := types.NamespacedName{Namespace: namespace, Name: w.Name}
		switch w.GVK.Kind {
		case "Deployment":
			var d appsv1.Deployment
			if err := r.Client.Get(ctx, key, &d); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			if !DeploymentReady(&d) {
				return false, nil
			}
		case "StatefulSet":
			var s appsv1.StatefulSet
			if err := r.Client.Get(ctx, key, &s); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			if !StatefulSetReady(&s) {
				return false, nil
			}
		}
	}
	return true, nil
}

func replicas(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

// DeploymentReady mirrors `kubectl rollout status`: the controller has seen
// the latest generation and every desired replica is updated and available.
func DeploymentReady(d *appsv1.Deployment) bool {
	want := replicas(d.Spec.Replicas)
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas >= want && d.Status.AvailableReplicas >= want &&
		d.Status.Replicas == d.Status.UpdatedReplicas
}

// StatefulSetReady reports whether every desired replica is updated and ready.
func StatefulSetReady(s *appsv1.StatefulSet) bool {
	want := replicas(s.Spec.Replicas)
	return s.Status.ObservedGeneration >= s.Generation &&
		s.Status.UpdatedReplicas >= want && s.Status.ReadyReplicas >= want
}

// apply server-side-applies the namespace and every object in the bundle,
// then prunes previously applied objects that the bundle no longer contains.
func (r *Reconciler) apply(ctx context.Context, pe *v1alpha1.PreviewEnvironment, b *render.Bundle) error {
	start := time.Now()
	defer func() { metrics.ApplyDuration.Observe(time.Since(start).Seconds()) }()

	ns := b.Namespace.DeepCopy()
	ns.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "PreviewEnvironment",
		Name:               pe.Name,
		UID:                pe.UID,
		Controller:         ptr(true),
		BlockOwnerDeletion: ptr(true),
	}})
	opts := []client.ApplyOption{client.FieldOwner(FieldOwner), client.ForceOwnership}
	if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(ns), opts...); err != nil {
		return fmt.Errorf("apply namespace %s: %w", ns.GetName(), err)
	}
	for _, o := range b.Objects {
		if err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(o.DeepCopy()), opts...); err != nil {
			return fmt.Errorf("apply %s/%s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	metrics.AppliedObjects.WithLabelValues("apply").Add(float64(len(b.Objects) + 1))
	pruned, err := r.prune(ctx, pe.Name, b)
	metrics.AppliedObjects.WithLabelValues("prune").Add(float64(pruned))
	return err
}

// prune deletes objects carrying this instance's label that the current
// bundle no longer renders, e.g. a worker Deployment whose enabling label
// was removed from the PR.
func (r *Reconciler) prune(ctx context.Context, instance string, b *render.Bundle) (int, error) {
	keep := b.Keys()
	selector := labels.SelectorFromSet(labels.Set{v1alpha1.LabelInstance: instance})
	namespace := b.Namespace.GetName()
	pruned := 0
	var errs []error
	for _, gvk := range r.Catalog.PruneKinds() {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err := r.Client.List(ctx, list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector})
		if meta.IsNoMatchError(err) {
			continue // kind not served by this cluster
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s: %w", gvk.Kind, err))
			continue
		}
		for i := range list.Items {
			item := &list.Items[i]
			if keep[render.ObjectKey{GVK: gvk, Name: item.GetName()}] {
				continue
			}
			if err := r.Client.Delete(ctx, item, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("prune %s/%s: %w", gvk.Kind, item.GetName(), err))
				continue
			}
			pruned++
		}
	}
	return pruned, errors.Join(errs...)
}

func (r *Reconciler) deleteNamespace(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := r.Client.Delete(ctx, ns, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", name, err)
	}
	return nil
}

func (r *Reconciler) report(ctx context.Context, pe *v1alpha1.PreviewEnvironment, state lifecycle.ReportState, message string) error {
	s := scm.Status{Repo: pe.Spec.Repository, SHA: pe.Spec.PullRequest.HeadSHA, State: state}
	switch state {
	case lifecycle.ReportReady, lifecycle.ReportPending:
		s.URL = pe.Status.URL
	case lifecycle.ReportFailed:
		s.Description = "Preview failed: " + message
	}
	err := r.Reporter.Report(ctx, s)
	result := "ok"
	if err != nil {
		result = "error"
	}
	metrics.StatusReports.WithLabelValues(string(state), result).Inc()
	return err
}

// applyFailed turns a failed apply into a Failed decision with backoff.
func applyFailed(o lifecycle.Observation, err error) lifecycle.Decision {
	n := o.FailureCount + 1
	failed := lifecycle.ReportFailed
	d := lifecycle.Decision{
		Phase:        v1alpha1.PhaseFailed,
		Reason:       "ApplyFailed",
		Message:      err.Error(),
		FailureCount: n,
		RequeueAfter: lifecycle.Backoff(n),
	}
	if o.LastReported == nil || o.LastReported.SHA != o.HeadSHA || o.LastReported.State != failed {
		d.Report = &failed
	}
	return d
}

func (r *Reconciler) updateStatus(pe *v1alpha1.PreviewEnvironment, d lifecycle.Decision, obs lifecycle.Observation, extra observed) {
	s := &pe.Status
	s.Phase = d.Phase
	s.Message = d.Message
	s.FailureCount = d.FailureCount
	s.ObservedGeneration = pe.Generation
	switch {
	case d.MarkTerminated:
		s.TerminatedAt = &metav1.Time{Time: obs.Now}
	case d.Phase != v1alpha1.PhaseClosed && d.Phase != v1alpha1.PhaseExpired:
		s.TerminatedAt = nil
	}
	if d.Phase == v1alpha1.PhaseClosed || d.Phase == v1alpha1.PhaseExpired {
		s.AppliedHash, s.Resources = "", 0
	}
	gen := pe.Generation
	set := func(t string, ok bool, reason, msg string) {
		st := metav1.ConditionFalse
		if ok {
			st = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: t, Status: st, Reason: reason, Message: msg, ObservedGeneration: gen})
	}
	switch {
	case obs.RenderError != "":
		set(v1alpha1.ConditionRendered, false, "RenderFailed", obs.RenderError)
	case extra.bundle != nil:
		set(v1alpha1.ConditionRendered, true, "Rendered", fmt.Sprintf("bundle %s with %d objects", extra.bundle.Hash, len(extra.bundle.Objects)+1))
	}
	applied := extra.bundle != nil && s.AppliedHash == extra.bundle.Hash
	switch {
	case d.Reason == "ApplyFailed":
		set(v1alpha1.ConditionApplied, false, "ApplyFailed", d.Message)
	case applied:
		set(v1alpha1.ConditionApplied, true, "Applied", "bundle "+s.AppliedHash+" applied")
	default:
		set(v1alpha1.ConditionApplied, false, d.Reason, d.Message)
	}
	set(v1alpha1.ConditionReady, d.Phase == v1alpha1.PhaseReady, d.Reason, d.Message)
}

func (r *Reconciler) recordTransition(pe *v1alpha1.PreviewEnvironment, from v1alpha1.Phase, d lifecycle.Decision) {
	r.Phases.Set(pe.Name, string(d.Phase))
	if from == d.Phase {
		return
	}
	metrics.PhaseTransitions.WithLabelValues(phaseOrNone(from), string(d.Phase)).Inc()
	if d.Phase == v1alpha1.PhaseReady {
		// Skip negative samples caused by clock skew between the SCM and
		// the cluster rather than corrupting the histogram.
		if since := r.Clock.Since(pe.Spec.LastActivityTime.Time); since >= 0 {
			metrics.TimeToReady.Observe(since.Seconds())
		}
	}
	eventType := corev1.EventTypeNormal
	if d.Phase == v1alpha1.PhaseFailed {
		eventType = corev1.EventTypeWarning
	}
	if r.Events != nil {
		r.Events.Eventf(pe, nil, eventType, d.Reason, "Reconcile", "%s -> %s: %s", phaseOrNone(from), d.Phase, d.Message)
	}
}

func phaseOrNone(p v1alpha1.Phase) string {
	if p == "" {
		return "<none>"
	}
	return string(p)
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 || b < a {
		return b
	}
	return a
}

func ptr[T any](v T) *T { return &v }

func clampInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < 0:
		return 0
	default:
		return int32(n)
	}
}

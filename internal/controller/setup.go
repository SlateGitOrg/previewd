package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/archit2501/previewd/api/v1alpha1"
)

// ManagedSelector selects every object previewd created.
func ManagedSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
}

// CacheOptions restricts the informers for namespaces and workloads to
// objects previewd manages, so memory scales with the number of previews
// rather than the size of the cluster.
func CacheOptions() cache.Options {
	sel := ManagedSelector()
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Namespace{}:   {Label: sel},
		&appsv1.Deployment{}:  {Label: sel},
		&appsv1.StatefulSet{}: {Label: sel},
	}}
}

// byInstanceLabel maps a managed object to the PreviewEnvironment named in
// its instance label.
func byInstanceLabel(_ context.Context, o client.Object) []reconcile.Request {
	name := o.GetLabels()[v1alpha1.LabelInstance]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

// SetupWithManager registers the reconciler. It watches PreviewEnvironments,
// the namespaces they own, and the workloads inside them, so readiness and
// out-of-band deletions trigger a reconcile without polling.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	workers := r.Options.MaxConcurrentReconciles
	if workers < 1 {
		workers = 4
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PreviewEnvironment{}).
		Owns(&corev1.Namespace{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(byInstanceLabel)).
		Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(byInstanceLabel)).
		WithOptions(controller.Options{MaxConcurrentReconciles: workers}).
		Named("previewenvironment").
		Complete(r)
}

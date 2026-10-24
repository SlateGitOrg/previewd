// Package gc deletes orphaned preview namespaces: namespaces previewd
// created whose PreviewEnvironment no longer exists. Normal teardown goes
// through the reconciler and its finalizer; this collector is the backstop
// for records that were force-deleted (finalizer removed by hand) or lost.
package gc

import (
	"context"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/metrics"
)

// SelectOrphans returns the names of namespaces that are managed by
// previewd, older than grace, not already terminating, and whose instance
// label names no live PreviewEnvironment. The grace period avoids racing a
// reconcile that has created the namespace but whose PreviewEnvironment is
// not yet in the cache.
func SelectOrphans(namespaces []corev1.Namespace, live map[string]bool, now time.Time, grace time.Duration) []string {
	var out []string
	for _, ns := range namespaces {
		if ns.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue {
			continue
		}
		if !ns.DeletionTimestamp.IsZero() {
			continue
		}
		if now.Sub(ns.CreationTimestamp.Time) < grace {
			continue
		}
		if live[ns.Labels[v1alpha1.LabelInstance]] {
			continue
		}
		out = append(out, ns.Name)
	}
	sort.Strings(out)
	return out
}

// Collector periodically deletes orphaned namespaces.
type Collector struct {
	Client   client.Client
	Interval time.Duration
	Grace    time.Duration
	Clock    clock.PassiveClock
	Log      logr.Logger
}

// NeedLeaderElection makes only the leader collect.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start runs sweeps until ctx is cancelled. It implements manager.Runnable.
func (c *Collector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := c.Sweep(ctx); err != nil {
			c.Log.Error(err, "orphan sweep failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Sweep runs one collection pass and returns the namespaces it deleted.
func (c *Collector) Sweep(ctx context.Context) ([]string, error) {
	var nsList corev1.NamespaceList
	if err := c.Client.List(ctx, &nsList, client.MatchingLabels{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue}); err != nil {
		return nil, err
	}
	var peList v1alpha1.PreviewEnvironmentList
	if err := c.Client.List(ctx, &peList); err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(peList.Items))
	for _, pe := range peList.Items {
		live[pe.Name] = true
	}
	var deleted []string
	for _, name := range SelectOrphans(nsList.Items, live, c.Clock.Now(), c.Grace) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := c.Client.Delete(ctx, ns, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return deleted, err
		}
		c.Log.Info("deleted orphaned preview namespace", "namespace", name)
		metrics.GarbageCollected.Inc()
		deleted = append(deleted, name)
	}
	return deleted, nil
}

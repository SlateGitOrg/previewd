// Package metrics defines previewd's Prometheus metrics. They are
// registered with controller-runtime's registry, so they are served on the
// manager's /metrics endpoint next to the built-in controller metrics
// (reconcile counts, durations, work-queue depth).
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// WebhookRequests counts webhook deliveries by GitHub event type and
	// outcome (created, updated, unchanged, stale, ignored, duplicate,
	// unauthorized, bad_request, error, pong).
	WebhookRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previewd_webhook_requests_total",
		Help: "Webhook deliveries by event type and outcome.",
	}, []string{"event", "outcome"})

	// PhaseTransitions counts lifecycle phase changes.
	PhaseTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previewd_phase_transitions_total",
		Help: "PreviewEnvironment phase transitions.",
	}, []string{"from", "to"})

	// Environments is the number of PreviewEnvironments per phase, as of
	// the last reconcile of each.
	Environments = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "previewd_environments",
		Help: "PreviewEnvironments by phase.",
	}, []string{"phase"})

	// ApplyDuration measures how long applying a bundle takes.
	ApplyDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "previewd_apply_duration_seconds",
		Help:    "Time to server-side apply and prune one bundle.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
	})

	// AppliedObjects counts objects applied or pruned.
	AppliedObjects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previewd_objects_total",
		Help: "Objects applied or pruned.",
	}, []string{"op"})

	// TimeToReady observes the time from the triggering PR activity to the
	// Ready phase.
	TimeToReady = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "previewd_time_to_ready_seconds",
		Help:    "Seconds from the last PR activity to the environment becoming Ready.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12),
	})

	// StatusReports counts SCM status posts by state and result.
	StatusReports = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "previewd_scm_status_reports_total",
		Help: "SCM commit-status posts by state and result.",
	}, []string{"state", "result"})

	// GarbageCollected counts namespaces removed by the orphan collector.
	GarbageCollected = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "previewd_gc_orphans_deleted_total",
		Help: "Orphaned preview namespaces deleted by the garbage collector.",
	})
)

// All returns every previewd collector.
func All() []prometheus.Collector {
	return []prometheus.Collector{
		WebhookRequests, PhaseTransitions, Environments, ApplyDuration,
		AppliedObjects, TimeToReady, StatusReports, GarbageCollected,
	}
}

func init() {
	ctrlmetrics.Registry.MustRegister(All()...)
}

// PhaseTracker keeps the Environments gauge consistent: it remembers the
// last phase recorded for each environment so a phase change moves exactly
// one unit between gauge series.
type PhaseTracker struct {
	mu     sync.Mutex
	phases map[string]string
	gauge  *prometheus.GaugeVec
}

// NewPhaseTracker returns a tracker that updates gauge.
func NewPhaseTracker(gauge *prometheus.GaugeVec) *PhaseTracker {
	return &PhaseTracker{phases: map[string]string{}, gauge: gauge}
}

// Set records that instance is in phase and returns the previous phase
// ("" if unknown).
func (t *PhaseTracker) Set(instance, phase string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, known := t.phases[instance]
	if known && prev == phase {
		return prev
	}
	if known {
		t.gauge.WithLabelValues(prev).Dec()
	}
	t.gauge.WithLabelValues(phase).Inc()
	t.phases[instance] = phase
	return prev
}

// Forget removes instance from the gauge.
func (t *PhaseTracker) Forget(instance string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.phases[instance]; ok {
		t.gauge.WithLabelValues(prev).Dec()
		delete(t.phases, instance)
	}
}

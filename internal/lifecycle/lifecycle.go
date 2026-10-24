// Package lifecycle is the level-triggered state machine at the heart of
// previewd.
//
// Decide is a pure function from an Observation (everything the reconciler
// read from the cluster and the clock) to a Decision (the phase to record and
// the side effects to perform). It never looks at *how* the world got into
// its current state — only at what the state is now — so it is safe to call
// any number of times, after crashes, missed events or duplicate deliveries.
// Running the decided effects and observing again converges to a fixed point
// in which Decide asks for no further mutations; the property tests in this
// package check exactly that.
package lifecycle

import (
	"time"

	"github.com/archit2501/previewd/api/v1alpha1"
)

// NamespaceState is the observed state of the preview namespace.
type NamespaceState int

const (
	// NamespaceAbsent means the namespace does not exist.
	NamespaceAbsent NamespaceState = iota
	// NamespaceActive means the namespace exists and is not being deleted.
	NamespaceActive
	// NamespaceTerminating means deletion was requested and is in progress.
	NamespaceTerminating
)

func (s NamespaceState) String() string {
	switch s {
	case NamespaceActive:
		return "Active"
	case NamespaceTerminating:
		return "Terminating"
	default:
		return "Absent"
	}
}

// ReportState is the SCM-neutral status previewd reports for a head commit.
type ReportState string

// Report states. The SCM client maps them onto its own vocabulary.
const (
	ReportPending  ReportState = "pending"
	ReportReady    ReportState = "ready"
	ReportFailed   ReportState = "failed"
	ReportExpired  ReportState = "expired"
	ReportInactive ReportState = "inactive"
)

// Reported is the last status that was successfully posted.
type Reported struct {
	SHA   string
	State ReportState
}

// Observation is everything Decide needs to know. The reconciler fills it
// from the API server, the rendered bundle and the clock.
type Observation struct {
	Now time.Time

	// Deleting is true when the PreviewEnvironment has a deletion timestamp.
	Deleting bool
	// Wanted is true when the PR is open and passes the gating rules
	// (required label, draft policy). NotWantedReason explains a false value.
	Wanted          bool
	NotWantedReason string
	// ExpiresAt is lastActivityTime + TTL.
	ExpiresAt time.Time

	Namespace NamespaceState

	// RenderError is non-empty when the template failed to render or
	// validate. DesiredHash is the bundle hash when rendering succeeded.
	RenderError string
	DesiredHash string
	AppliedHash string
	// WorkloadsReady reports whether every workload in the applied bundle
	// is available. It is only meaningful when AppliedHash == DesiredHash.
	WorkloadsReady bool

	HeadSHA      string
	LastReported *Reported
	TerminatedAt *time.Time
	FailureCount int32

	// Retention is how long a Closed/Expired environment record is kept
	// before the PreviewEnvironment object itself is deleted. Zero keeps it
	// forever.
	Retention time.Duration
	// PollInterval is how soon to look again while waiting on the cluster.
	PollInterval time.Duration
}

// Decision is the outcome of Decide.
type Decision struct {
	Phase   v1alpha1.Phase
	Reason  string
	Message string

	// Apply asks for namespace creation, server-side apply of the bundle and
	// pruning of objects that are no longer rendered.
	Apply bool
	// DeleteNamespace asks for the namespace to be deleted.
	DeleteNamespace bool
	// Report, when non-nil, is a status to post for HeadSHA. It is only set
	// when it differs from LastReported, so repeated reconciles are quiet.
	Report *ReportState
	// ReleaseFinalizer asks for the teardown finalizer to be removed.
	ReleaseFinalizer bool
	// DeleteSelf asks for the PreviewEnvironment to be deleted because its
	// retention period has elapsed.
	DeleteSelf bool
	// MarkTerminated asks for status.terminatedAt to be set to Now.
	MarkTerminated bool
	// FailureCount is the value to store in status.failureCount.
	FailureCount int32
	// RequeueAfter is when to reconcile again even without a watch event.
	// Zero means "only on the next event".
	RequeueAfter time.Duration
}

// Mutates reports whether the decision changes anything in the cluster or
// the SCM. A converged environment yields a decision that does not mutate.
func (d Decision) Mutates() bool {
	return d.Apply || d.DeleteNamespace || d.Report != nil || d.ReleaseFinalizer ||
		d.DeleteSelf || d.MarkTerminated
}

// Backoff limits for failed renders and applies.
const (
	BaseBackoff = 5 * time.Second
	MaxBackoff  = 5 * time.Minute
)

// Backoff returns the retry delay after n consecutive failures: 5s, 10s,
// 20s, ... capped at MaxBackoff.
func Backoff(n int32) time.Duration {
	if n < 1 {
		return BaseBackoff
	}
	d := BaseBackoff
	for i := int32(1); i < n; i++ {
		d *= 2
		if d >= MaxBackoff {
			return MaxBackoff
		}
	}
	return d
}

func (o Observation) poll() time.Duration {
	if o.PollInterval <= 0 {
		return 10 * time.Second
	}
	return o.PollInterval
}

// report returns a pointer to s when s is not already the last reported
// state for the current head commit.
func (o Observation) report(s ReportState) *ReportState {
	if o.HeadSHA == "" {
		return nil
	}
	if o.LastReported != nil && o.LastReported.SHA == o.HeadSHA && o.LastReported.State == s {
		return nil
	}
	return &s
}

// Expired reports whether the TTL has elapsed.
func (o Observation) Expired() bool {
	return !o.ExpiresAt.IsZero() && !o.Now.Before(o.ExpiresAt)
}

// Decide maps an observation to a decision. It is pure and total.
func Decide(o Observation) Decision {
	if o.Deleting {
		return decideDeleting(o)
	}
	if !o.Wanted || o.Expired() {
		return decideTeardown(o)
	}
	return decideRunning(o)
}

func decideDeleting(o Observation) Decision {
	d := Decision{Phase: v1alpha1.PhaseTearingDown, FailureCount: o.FailureCount}
	if o.Namespace == NamespaceActive {
		d.DeleteNamespace = true
		d.Reason = "DeletingNamespace"
		d.Message = "PreviewEnvironment is being deleted; deleting its namespace"
		d.RequeueAfter = o.poll()
		return d
	}
	// Once the namespace is terminating, the Kubernetes namespace controller
	// owns cleanup of everything inside it; holding our own object hostage
	// until the namespace is fully gone adds nothing.
	d.ReleaseFinalizer = true
	d.Reason = "NamespaceReleased"
	d.Message = "namespace is " + o.Namespace.String() + "; releasing finalizer"
	return d
}

func decideTeardown(o Observation) Decision {
	terminal, report, reason, msg := v1alpha1.PhaseClosed, ReportInactive, "Closed", o.NotWantedReason
	if o.Wanted {
		terminal, report, reason = v1alpha1.PhaseExpired, ReportExpired, "Expired"
		msg = "TTL elapsed at " + o.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if msg == "" {
		msg = "pull request closed"
	}
	d := Decision{FailureCount: 0}
	switch o.Namespace {
	case NamespaceActive:
		d.Phase = v1alpha1.PhaseTearingDown
		d.DeleteNamespace = true
		d.Reason = reason
		d.Message = msg + "; deleting namespace"
		d.RequeueAfter = o.poll()
		return d
	case NamespaceTerminating:
		d.Phase = v1alpha1.PhaseTearingDown
		d.Reason = reason
		d.Message = msg + "; waiting for namespace deletion"
		d.RequeueAfter = o.poll()
		return d
	}
	d.Phase = terminal
	d.Reason = reason
	d.Message = msg
	d.Report = o.report(report)
	if o.Retention <= 0 {
		return d
	}
	if o.TerminatedAt == nil {
		d.MarkTerminated = true
		d.RequeueAfter = o.Retention
		return d
	}
	until := o.TerminatedAt.Add(o.Retention).Sub(o.Now)
	if until <= 0 {
		d.DeleteSelf = true
		d.Reason = "RetentionElapsed"
		d.Message = "retention elapsed; deleting record"
		return d
	}
	d.RequeueAfter = until
	return d
}

func decideRunning(o Observation) Decision {
	var untilExpiry time.Duration // zero means "no TTL"
	if !o.ExpiresAt.IsZero() {
		untilExpiry = o.ExpiresAt.Sub(o.Now)
	}
	if o.Namespace == NamespaceTerminating {
		// A reopened PR whose previous namespace is still being torn down.
		return Decision{
			Phase:        v1alpha1.PhasePending,
			Reason:       "WaitingForNamespace",
			Message:      "previous namespace is still terminating",
			RequeueAfter: minPositive(o.poll(), untilExpiry),
			FailureCount: o.FailureCount,
		}
	}
	if o.RenderError != "" {
		n := o.FailureCount + 1
		return Decision{
			Phase:        v1alpha1.PhaseFailed,
			Reason:       "RenderFailed",
			Message:      o.RenderError,
			Report:       o.report(ReportFailed),
			FailureCount: n,
			RequeueAfter: minPositive(Backoff(n), untilExpiry),
		}
	}
	if o.Namespace == NamespaceAbsent || o.AppliedHash != o.DesiredHash {
		return Decision{
			Phase:        v1alpha1.PhaseProvisioning,
			Reason:       "Applying",
			Message:      "applying rendered bundle",
			Apply:        true,
			Report:       o.report(ReportPending),
			FailureCount: 0,
			RequeueAfter: minPositive(o.poll(), untilExpiry),
		}
	}
	if !o.WorkloadsReady {
		return Decision{
			Phase:        v1alpha1.PhaseProvisioning,
			Reason:       "WaitingForWorkloads",
			Message:      "bundle applied; waiting for workloads to become available",
			Report:       o.report(ReportPending),
			RequeueAfter: minPositive(o.poll(), untilExpiry),
		}
	}
	return Decision{
		Phase:   v1alpha1.PhaseReady,
		Reason:  "Ready",
		Message: "all workloads available",
		Report:  o.report(ReportReady),
		// Wake up exactly when the TTL elapses so expiry needs no sweeper.
		RequeueAfter: untilExpiry,
		FailureCount: 0,
	}
}

// minPositive returns the smaller of a and b, treating non-positive b as
// "no deadline".
func minPositive(a, b time.Duration) time.Duration {
	if b > 0 && b < a {
		return b
	}
	return a
}

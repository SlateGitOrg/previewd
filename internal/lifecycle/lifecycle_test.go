package lifecycle

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func base() Observation {
	return Observation{
		Now:          t0,
		Wanted:       true,
		ExpiresAt:    t0.Add(time.Hour),
		Namespace:    NamespaceActive,
		DesiredHash:  "h1",
		AppliedHash:  "h1",
		HeadSHA:      "abc1234",
		PollInterval: 5 * time.Second,
		Retention:    time.Hour,
	}
}

func TestDecideTable(t *testing.T) {
	ready := ReportReady
	cases := []struct {
		name   string
		mut    func(*Observation)
		phase  v1alpha1.Phase
		check  func(*testing.T, Decision)
		quiet  bool // decision must not mutate
		reason string
	}{
		{
			name:   "fresh PR applies and reports pending",
			mut:    func(o *Observation) { o.Namespace = NamespaceAbsent; o.AppliedHash = "" },
			phase:  v1alpha1.PhaseProvisioning,
			reason: "Applying",
			check: func(t *testing.T, d Decision) {
				if !d.Apply || d.Report == nil || *d.Report != ReportPending {
					t.Fatalf("want apply+pending, got %+v", d)
				}
				if d.RequeueAfter != 5*time.Second {
					t.Fatalf("requeue %v", d.RequeueAfter)
				}
			},
		},
		{
			name:   "new push re-applies",
			mut:    func(o *Observation) { o.DesiredHash = "h2" },
			phase:  v1alpha1.PhaseProvisioning,
			reason: "Applying",
			check: func(t *testing.T, d Decision) {
				if !d.Apply {
					t.Fatal("expected apply")
				}
			},
		},
		{
			name:   "applied but workloads not ready waits",
			mut:    func(o *Observation) { o.LastReported = &Reported{SHA: "abc1234", State: ReportPending} },
			phase:  v1alpha1.PhaseProvisioning,
			reason: "WaitingForWorkloads",
			quiet:  true,
		},
		{
			name:   "ready reports success once and requeues at expiry",
			mut:    func(o *Observation) { o.WorkloadsReady = true },
			phase:  v1alpha1.PhaseReady,
			reason: "Ready",
			check: func(t *testing.T, d Decision) {
				if d.Report == nil || *d.Report != ReportReady {
					t.Fatalf("report %v", d.Report)
				}
				if d.RequeueAfter != time.Hour {
					t.Fatalf("requeue %v", d.RequeueAfter)
				}
			},
		},
		{
			name: "ready and already reported is quiet",
			mut: func(o *Observation) {
				o.WorkloadsReady = true
				o.LastReported = &Reported{SHA: "abc1234", State: ready}
			},
			phase: v1alpha1.PhaseReady,
			quiet: true,
		},
		{
			name: "ready but reported for an older commit reports again",
			mut: func(o *Observation) {
				o.WorkloadsReady = true
				o.LastReported = &Reported{SHA: "0000000", State: ready}
			},
			phase: v1alpha1.PhaseReady,
			check: func(t *testing.T, d Decision) {
				if d.Report == nil {
					t.Fatal("expected a report for the new SHA")
				}
			},
		},
		{
			name: "render failure backs off and counts",
			mut: func(o *Observation) {
				o.RenderError = "boom"
				o.FailureCount = 2
			},
			phase:  v1alpha1.PhaseFailed,
			reason: "RenderFailed",
			check: func(t *testing.T, d Decision) {
				if d.FailureCount != 3 || d.RequeueAfter != 20*time.Second {
					t.Fatalf("got count=%d requeue=%v", d.FailureCount, d.RequeueAfter)
				}
				if d.Apply {
					t.Fatal("must not apply a failed render")
				}
			},
		},
		{
			name:   "closed PR deletes namespace",
			mut:    func(o *Observation) { o.Wanted = false; o.NotWantedReason = "pull request closed" },
			phase:  v1alpha1.PhaseTearingDown,
			reason: "Closed",
			check: func(t *testing.T, d Decision) {
				if !d.DeleteNamespace || d.Report != nil {
					t.Fatalf("got %+v", d)
				}
			},
		},
		{
			name: "closed PR waits for terminating namespace",
			mut: func(o *Observation) {
				o.Wanted = false
				o.Namespace = NamespaceTerminating
			},
			phase: v1alpha1.PhaseTearingDown,
			quiet: true,
		},
		{
			name: "closed PR with namespace gone becomes Closed and marks termination",
			mut: func(o *Observation) {
				o.Wanted = false
				o.Namespace = NamespaceAbsent
			},
			phase: v1alpha1.PhaseClosed,
			check: func(t *testing.T, d Decision) {
				if !d.MarkTerminated || d.RequeueAfter != time.Hour {
					t.Fatalf("got %+v", d)
				}
				if d.Report == nil || *d.Report != ReportInactive {
					t.Fatalf("report %v", d.Report)
				}
				if d.Message != "pull request closed" {
					t.Fatalf("message %q", d.Message)
				}
			},
		},
		{
			name:   "TTL elapsed tears down",
			mut:    func(o *Observation) { o.Now = o.ExpiresAt },
			phase:  v1alpha1.PhaseTearingDown,
			reason: "Expired",
		},
		{
			name: "expired and gone reports expired",
			mut: func(o *Observation) {
				o.Now = o.ExpiresAt.Add(time.Minute)
				o.Namespace = NamespaceAbsent
			},
			phase: v1alpha1.PhaseExpired,
			check: func(t *testing.T, d Decision) {
				if d.Report == nil || *d.Report != ReportExpired {
					t.Fatalf("report %v", d.Report)
				}
			},
		},
		{
			name: "retention elapsed deletes the record",
			mut: func(o *Observation) {
				o.Wanted = false
				o.Namespace = NamespaceAbsent
				ts := t0.Add(-2 * time.Hour)
				o.TerminatedAt = &ts
				o.LastReported = &Reported{SHA: "abc1234", State: ReportInactive}
			},
			phase:  v1alpha1.PhaseClosed,
			reason: "RetentionElapsed",
			check: func(t *testing.T, d Decision) {
				if !d.DeleteSelf {
					t.Fatal("expected self-delete")
				}
			},
		},
		{
			name: "retention pending requeues for the remainder",
			mut: func(o *Observation) {
				o.Wanted = false
				o.Namespace = NamespaceAbsent
				ts := t0.Add(-20 * time.Minute)
				o.TerminatedAt = &ts
				o.LastReported = &Reported{SHA: "abc1234", State: ReportInactive}
			},
			phase: v1alpha1.PhaseClosed,
			quiet: true,
			check: func(t *testing.T, d Decision) {
				if d.RequeueAfter != 40*time.Minute {
					t.Fatalf("requeue %v", d.RequeueAfter)
				}
			},
		},
		{
			name: "zero retention keeps the record forever",
			mut: func(o *Observation) {
				o.Wanted = false
				o.Namespace = NamespaceAbsent
				o.Retention = 0
				o.LastReported = &Reported{SHA: "abc1234", State: ReportInactive}
			},
			phase: v1alpha1.PhaseClosed,
			quiet: true,
		},
		{
			name:   "reopen while old namespace terminates waits",
			mut:    func(o *Observation) { o.Namespace = NamespaceTerminating },
			phase:  v1alpha1.PhasePending,
			reason: "WaitingForNamespace",
			quiet:  true,
		},
		{
			name:   "deletion with active namespace deletes it",
			mut:    func(o *Observation) { o.Deleting = true },
			phase:  v1alpha1.PhaseTearingDown,
			reason: "DeletingNamespace",
			check: func(t *testing.T, d Decision) {
				if !d.DeleteNamespace || d.ReleaseFinalizer {
					t.Fatalf("got %+v", d)
				}
			},
		},
		{
			name:   "deletion with terminating namespace releases finalizer",
			mut:    func(o *Observation) { o.Deleting = true; o.Namespace = NamespaceTerminating },
			phase:  v1alpha1.PhaseTearingDown,
			reason: "NamespaceReleased",
			check: func(t *testing.T, d Decision) {
				if !d.ReleaseFinalizer || d.DeleteNamespace {
					t.Fatalf("got %+v", d)
				}
			},
		},
		{
			name: "no head SHA never reports",
			mut: func(o *Observation) {
				o.HeadSHA = ""
				o.WorkloadsReady = true
			},
			phase: v1alpha1.PhaseReady,
			quiet: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := base()
			tc.mut(&o)
			d := Decide(o)
			if d.Phase != tc.phase {
				t.Fatalf("phase = %s, want %s (%+v)", d.Phase, tc.phase, d)
			}
			if tc.reason != "" && d.Reason != tc.reason {
				t.Fatalf("reason = %s, want %s", d.Reason, tc.reason)
			}
			if tc.quiet && d.Mutates() {
				t.Fatalf("expected a quiet decision, got %+v", d)
			}
			if tc.check != nil {
				tc.check(t, d)
			}
			if again := Decide(o); again != d && !sameDecision(again, d) {
				t.Fatalf("Decide is not deterministic: %+v vs %+v", d, again)
			}
		})
	}
}

func sameDecision(a, b Decision) bool {
	ra, rb := "", ""
	if a.Report != nil {
		ra = string(*a.Report)
	}
	if b.Report != nil {
		rb = string(*b.Report)
	}
	a.Report, b.Report = nil, nil
	return a == b && ra == rb
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}
	for n, w := range want {
		if got := Backoff(int32(n)); got != w {
			t.Errorf("Backoff(%d)=%v want %v", n, got, w)
		}
	}
	if Backoff(50) != MaxBackoff {
		t.Fatal("backoff not capped")
	}
}

func TestNoTTLMeansNoExpiryRequeue(t *testing.T) {
	o := base()
	o.ExpiresAt = time.Time{}
	o.WorkloadsReady = true
	d := Decide(o)
	if d.Phase != v1alpha1.PhaseReady || d.RequeueAfter != 0 {
		t.Fatalf("got %+v", d)
	}
	o.DesiredHash = "other"
	if d := Decide(o); d.RequeueAfter != o.PollInterval {
		t.Fatalf("got %v", d.RequeueAfter)
	}
}

func TestDefaultPollInterval(t *testing.T) {
	o := base()
	o.PollInterval = 0
	o.DesiredHash = "new"
	if d := Decide(o); d.RequeueAfter != 10*time.Second {
		t.Fatalf("got %v", d.RequeueAfter)
	}
}

func TestRequeueNeverOverrunsExpiry(t *testing.T) {
	o := base()
	o.ExpiresAt = o.Now.Add(2 * time.Second)
	o.DesiredHash = "new"
	if d := Decide(o); d.RequeueAfter != 2*time.Second {
		t.Fatalf("got %v", d.RequeueAfter)
	}
}

func TestNamespaceStateString(t *testing.T) {
	if NamespaceAbsent.String() != "Absent" || NamespaceActive.String() != "Active" || NamespaceTerminating.String() != "Terminating" {
		t.Fatal("unexpected strings")
	}
}

// world is a tiny simulation of the cluster and SCM used by the property
// tests: it applies a Decision's effects the way the reconciler would and
// lets the "cluster" make progress between reconciles.
type world struct {
	o                Observation
	readyIn          int // reconciles until workloads become available
	terminatingFor   int // reconciles until a terminating namespace is gone
	released, gone   bool
	reportsPosted    []Reported
	readyDelay, nsGC int
}

func (w *world) step() Decision {
	d := Decide(w.o)
	if d.Apply {
		if w.o.AppliedHash != w.o.DesiredHash || w.o.Namespace != NamespaceActive {
			w.o.WorkloadsReady = false
			w.readyIn = w.readyDelay
		}
		w.o.Namespace = NamespaceActive
		w.o.AppliedHash = w.o.DesiredHash
	}
	if d.DeleteNamespace {
		w.o.Namespace = NamespaceTerminating
		w.terminatingFor = w.nsGC
	}
	if d.Report != nil {
		r := Reported{SHA: w.o.HeadSHA, State: *d.Report}
		w.reportsPosted = append(w.reportsPosted, r)
		w.o.LastReported = &r
	}
	if d.MarkTerminated {
		ts := w.o.Now
		w.o.TerminatedAt = &ts
	}
	w.o.FailureCount = d.FailureCount
	w.released = w.released || d.ReleaseFinalizer
	w.gone = w.gone || d.DeleteSelf

	// The cluster makes progress between reconciles.
	if w.o.Namespace == NamespaceTerminating {
		if w.terminatingFor <= 0 {
			w.o.Namespace = NamespaceAbsent
		}
		w.terminatingFor--
	}
	if w.o.Namespace == NamespaceActive && w.o.AppliedHash == w.o.DesiredHash && !w.o.WorkloadsReady {
		if w.readyIn <= 0 {
			w.o.WorkloadsReady = true
		}
		w.readyIn--
	}
	return d
}

func randomWorld(r *rand.Rand) *world {
	nsStates := []NamespaceState{NamespaceAbsent, NamespaceActive, NamespaceTerminating}
	o := Observation{
		Now:          t0,
		Deleting:     r.IntN(5) == 0,
		Wanted:       r.IntN(3) != 0,
		Namespace:    nsStates[r.IntN(3)],
		DesiredHash:  []string{"h1", "h2"}[r.IntN(2)],
		AppliedHash:  []string{"", "h1", "h2"}[r.IntN(3)],
		HeadSHA:      "abc1234",
		PollInterval: time.Second,
		FailureCount: int32(r.IntN(4)),
	}
	switch r.IntN(3) {
	case 0:
		o.ExpiresAt = t0.Add(-time.Minute) // already expired
	case 1:
		o.ExpiresAt = t0.Add(time.Hour)
	}
	if r.IntN(4) == 0 {
		o.RenderError = "template error"
	}
	if r.IntN(2) == 0 {
		o.WorkloadsReady = true
	}
	if r.IntN(2) == 0 {
		o.LastReported = &Reported{SHA: "abc1234", State: ReportPending}
	}
	return &world{o: o, readyDelay: r.IntN(4), nsGC: r.IntN(4)}
}

// TestConvergenceProperty: from any starting state, repeated reconciles
// reach a fixed point (no further mutations) within a bounded number of
// steps, the fixed point is the phase the inputs call for, and no status is
// ever posted twice in a row.
func TestConvergenceProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(27, 2026))
	for i := range 5000 {
		w := randomWorld(r)
		start := w.o
		var last Decision
		converged := false
		for range 30 {
			last = Decide(w.o)
			// The simulated cluster has nothing left to do on its own.
			stable := w.o.Namespace != NamespaceTerminating &&
				(w.o.WorkloadsReady || w.o.Namespace != NamespaceActive || w.o.AppliedHash != w.o.DesiredHash)
			if !last.Mutates() && stable {
				converged = true
				break
			}
			w.step()
			if w.released || w.gone {
				converged = true
				break
			}
		}
		if !converged {
			t.Fatalf("case %d did not converge\nstart=%+v\nend=%+v\nlast=%+v", i, start, w.o, last)
		}
		for j := 1; j < len(w.reportsPosted); j++ {
			if w.reportsPosted[j] == w.reportsPosted[j-1] {
				t.Fatalf("case %d posted %v twice in a row", i, w.reportsPosted[j])
			}
		}
		if w.released || w.gone {
			if !start.Deleting && w.released {
				t.Fatalf("case %d released finalizer without deletion", i)
			}
			continue
		}
		want := expectedPhase(start)
		if last.Phase != want {
			t.Fatalf("case %d converged to %s, want %s\nstart=%+v\nend=%+v", i, last.Phase, want, start, w.o)
		}
	}
}

func expectedPhase(o Observation) v1alpha1.Phase {
	switch {
	case !o.Wanted:
		return v1alpha1.PhaseClosed
	case o.Expired():
		return v1alpha1.PhaseExpired
	case o.RenderError != "":
		return v1alpha1.PhaseFailed
	default:
		return v1alpha1.PhaseReady
	}
}

// TestTeardownNeverApplies: whatever the state, a closed, expired or deleted
// environment is never (re)provisioned — the safety half of the machine.
func TestTeardownNeverApplies(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		w := randomWorld(r)
		if w.o.Wanted && !w.o.Expired() && !w.o.Deleting {
			continue
		}
		for range 20 {
			if d := w.step(); d.Apply {
				t.Fatalf("applied during teardown: %+v", w.o)
			}
		}
	}
}

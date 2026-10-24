package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPhaseTracker(t *testing.T) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "t"}, []string{"phase"})
	tr := NewPhaseTracker(g)
	if prev := tr.Set("a", "Pending"); prev != "" {
		t.Fatalf("prev %q", prev)
	}
	tr.Set("b", "Pending")
	if prev := tr.Set("a", "Ready"); prev != "Pending" {
		t.Fatalf("prev %q", prev)
	}
	tr.Set("a", "Ready") // no-op
	if v := testutil.ToFloat64(g.WithLabelValues("Pending")); v != 1 {
		t.Fatalf("pending %v", v)
	}
	if v := testutil.ToFloat64(g.WithLabelValues("Ready")); v != 1 {
		t.Fatalf("ready %v", v)
	}
	tr.Forget("a")
	tr.Forget("never-seen")
	if v := testutil.ToFloat64(g.WithLabelValues("Ready")); v != 0 {
		t.Fatalf("ready after forget %v", v)
	}
}

func TestAllRegistered(t *testing.T) {
	if len(All()) != 8 {
		t.Fatalf("collectors %d", len(All()))
	}
}

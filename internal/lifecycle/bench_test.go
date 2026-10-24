package lifecycle

import "testing"

func BenchmarkDecide(b *testing.B) {
	o := base()
	o.WorkloadsReady = true
	b.ReportAllocs()
	for b.Loop() {
		_ = Decide(o)
	}
}

package render

import "testing"

func BenchmarkRenderDefault(b *testing.B) {
	c, err := LoadCatalog(osDirFS(), "templates")
	if err != nil {
		b.Fatal(err)
	}
	ts, _ := c.Get("default")
	in := sampleInput()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ts.Render(in); err != nil {
			b.Fatal(err)
		}
	}
}

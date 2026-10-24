package webhook

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkVerify measures signature verification of a 25 KiB payload,
// roughly the size of a real pull_request delivery.
func BenchmarkVerify(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 25<<10)
	sig := Sign(secret, body)
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if err := Verify([][]byte{secret}, body, sig); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParsePullRequest(b *testing.B) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "webhooks", "01-opened.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParsePullRequest(body, "d", false); err != nil {
			b.Fatal(err)
		}
	}
}

package naming

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Acme/Shop":           "acme-shop",
		"feature/__Login--UI": "feature-login-ui",
		"--x--":               "x",
		"":                    "x",
		"日本語":                 "x",
		"v1.2.3":              "v1-2-3",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNamespaceShort(t *testing.T) {
	if got := Namespace("pr", "Shop", 42); got != "pr-shop-42" {
		t.Fatalf("got %q", got)
	}
}

func TestNamespaceLongIsTruncatedValidAndDistinct(t *testing.T) {
	long1 := strings.Repeat("very-long-repository-name-", 5) + "a"
	long2 := strings.Repeat("very-long-repository-name-", 5) + "b"
	a := Namespace("pr", long1, 123456)
	b := Namespace("pr", long2, 123456)
	for _, ns := range []string{a, b} {
		if len(ns) > MaxLabelLength {
			t.Fatalf("%q too long: %d", ns, len(ns))
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			t.Fatalf("%q invalid: %v", ns, errs)
		}
		if !strings.HasSuffix(ns, "-123456") || !strings.HasPrefix(ns, "pr-") {
			t.Fatalf("%q lost its prefix or PR number", ns)
		}
	}
	if a == b {
		t.Fatalf("distinct repos collided: %q", a)
	}
}

func TestInstanceIsValidLabel(t *testing.T) {
	got := Instance("Acme", "Shop", 7)
	if got != "acme-shop-pr-7" {
		t.Fatalf("got %q", got)
	}
	long := Instance(strings.Repeat("o", 80), strings.Repeat("r", 80), 99)
	if errs := validation.IsDNS1123Label(long); len(errs) > 0 {
		t.Fatalf("%q invalid: %v", long, errs)
	}
}

func TestFitDegeneratesToHash(t *testing.T) {
	got := fit(strings.Repeat("p", 40), "middle", strings.Repeat("s", 40), 20)
	if len(got) != 20 || got[0] != 'h' {
		t.Fatalf("got %q", got)
	}
}

func TestShortHashClamps(t *testing.T) {
	if len(ShortHash("x", 1000)) != 64 {
		t.Fatal("expected full sha256 hex")
	}
}

func TestLabelValue(t *testing.T) {
	cases := map[string]string{
		"acme/shop":   "acme_shop",
		"-weird-":     "weird",
		"ok.v1_2-3":   "ok.v1_2-3",
		"with space!": "with_space",
	}
	for in, want := range cases {
		if got := LabelValue(in); got != want {
			t.Errorf("LabelValue(%q) = %q, want %q", in, got, want)
		}
	}
	long := LabelValue(strings.Repeat("a", 100))
	if errs := validation.IsValidLabelValue(long); len(errs) > 0 {
		t.Fatalf("%q invalid: %v", long, errs)
	}
}

func FuzzNamespaceIsAlwaysValid(f *testing.F) {
	f.Add("pr", "shop", 1)
	f.Add("PR!!", strings.Repeat("ü", 70), 999999)
	f.Fuzz(func(t *testing.T, prefix, repo string, n int) {
		if n < 1 {
			n = -n + 1
		}
		ns := Namespace(prefix, repo, n)
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			t.Fatalf("Namespace(%q,%q,%d)=%q invalid: %v", prefix, repo, n, ns, errs)
		}
		if Namespace(prefix, repo, n) != ns {
			t.Fatal("not deterministic")
		}
	})
}

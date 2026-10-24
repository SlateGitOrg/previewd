package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/archit2501/previewd/internal/webhook"
)

func run(t *testing.T, stdin string, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, IO{
		Stdin:  strings.NewReader(stdin),
		Stdout: &out,
		Stderr: &errb,
		Getenv: func(k string) string { return env[k] },
	})
	return code, out.String(), errb.String()
}

var templates = filepath.Join("..", "..", "templates")

func TestRender(t *testing.T) {
	code, out, errOut := run(t, "", nil, "render", "--templates-dir", templates, "--repo", "acme/shop",
		"--pr", "42", "--sha", "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c", "--title", "hi",
		"--label", "preview")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if !strings.HasPrefix(out, "# previewd render: template=default namespace=pr-shop-42 objects=11") {
		t.Fatalf("header: %s", strings.SplitN(out, "\n", 2)[0])
	}
	if n := strings.Count(out, "\n---\n"); n != 11 {
		t.Fatalf("documents: %d", n)
	}
	if code, _, errOut := run(t, "", nil, "render", "--templates-dir", templates, "--template", "static-site",
		"--repo", "acme/site", "--pr", "1", "--sha", "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c"); code != 0 {
		t.Fatalf("static-site: %s", errOut)
	}
}

func TestRenderErrors(t *testing.T) {
	sha := "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c"
	cases := map[string][]string{
		"missing repo":     {"render", "--pr", "1", "--sha", sha},
		"unknown template": {"render", "--templates-dir", templates, "--template", "nope", "--repo", "a/b", "--pr", "1", "--sha", sha},
		"bad dir":          {"render", "--templates-dir", "nope", "--repo", "a/b", "--pr", "1", "--sha", sha},
		"bad flag":         {"render", "--nope"},
	}
	for name, args := range cases {
		if code, _, _ := run(t, "", nil, args...); code == 0 {
			t.Errorf("%s: expected failure", name)
		}
	}
	// A template that fails validation surfaces the render error.
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "bad", "manifests"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "bad", "templateset.yaml"), []byte("{}"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "bad", "manifests", "x.yaml"), []byte("apiVersion: v1\nkind: Namespace\nmetadata: {name: x}\n"), 0o644))
	code, _, errOut := run(t, "", nil, "render", "--templates-dir", dir, "--template", "bad", "--repo", "a/b", "--pr", "1", "--sha", sha)
	if code != 1 || !strings.Contains(errOut, "not allowed") {
		t.Fatalf("code %d: %s", code, errOut)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSign(t *testing.T) {
	body := `{"zen":"x"}`
	code, out, _ := run(t, body, map[string]string{"PREVIEWD_WEBHOOK_SECRETS": "first-secret-123456,second"}, "sign")
	if code != 0 || strings.TrimSpace(out) != webhook.Sign([]byte("first-secret-123456"), []byte(body)) {
		t.Fatalf("code %d out %q", code, out)
	}
	f := filepath.Join(t.TempDir(), "secret")
	must(t, os.WriteFile(f, []byte("file-secret-123456\nother\n"), 0o600))
	code, out, _ = run(t, body, nil, "sign", "--secret-file", f)
	if code != 0 || strings.TrimSpace(out) != webhook.Sign([]byte("file-secret-123456"), []byte(body)) {
		t.Fatalf("code %d out %q", code, out)
	}
	if code, _, errOut := run(t, body, nil, "sign"); code != 1 || !strings.Contains(errOut, "no secret") {
		t.Fatalf("code %d %s", code, errOut)
	}
	if code, _, _ := run(t, body, nil, "sign", "--secret-file", "/does/not/exist"); code != 1 {
		t.Fatal("missing secret file")
	}
}

func TestDispatch(t *testing.T) {
	if code, _, errOut := run(t, "", nil); code != 2 || !strings.Contains(errOut, "Usage") {
		t.Fatal("no args")
	}
	if code, _, _ := run(t, "", nil, "bogus"); code != 2 {
		t.Fatal("unknown command")
	}
	if code, out, _ := run(t, "", nil, "version"); code != 0 || !strings.Contains(out, "previewd dev") {
		t.Fatal("version")
	}
	if code, out, _ := run(t, "", nil, "help"); code != 0 || !strings.Contains(out, "serve") {
		t.Fatal("help")
	}
	if code, _, _ := run(t, "", nil, "render", "-h"); code != 0 {
		t.Fatal("-h should exit 0")
	}
	// serve validates configuration before touching the cluster.
	if code, _, errOut := run(t, "", nil, "serve"); code != 1 || !strings.Contains(errOut, "--repos") {
		t.Fatalf("serve without config: %d %s", code, errOut)
	}
}

package render

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/archit2501/previewd/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite golden files")

func sampleInput() Input {
	return Input{
		Instance: "acme-shop-pr-42",
		Repo:     v1alpha1.RepositoryRef{Owner: "acme", Name: "shop"},
		PullRequest: v1alpha1.PullRequestRef{
			Number:  42,
			HeadSHA: "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c",
			HeadRef: "feature/Checkout-v2",
			BaseRef: "main",
			Author:  "octocat",
			Title:   "Checkout v2: \"one-click\" flow",
			Labels:  []string{"preview"},
		},
	}
}

func loadRepoCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadCatalog(os.DirFS(filepath.Join("..", "..")), "templates")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func bundleYAML(t *testing.T, b *Bundle) string {
	t.Helper()
	var sb strings.Builder
	for _, o := range append([]*unstructured.Unstructured{b.Namespace}, b.Objects...) {
		y, err := yaml.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString("---\n")
		sb.Write(y)
	}
	return sb.String()
}

// TestGoldenBundles renders every bundled template set and compares the
// output with testdata/golden/<set>.yaml. Run `go test ./internal/render
// -update` after an intentional template change.
func TestGoldenBundles(t *testing.T) {
	c := loadRepoCatalog(t)
	if got := strings.Join(c.Names(), ","); got != "default,static-site" {
		t.Fatalf("catalog names = %s", got)
	}
	for _, name := range c.Names() {
		t.Run(name, func(t *testing.T) {
			ts, _ := c.Get(name)
			b, err := ts.Render(sampleInput())
			if err != nil {
				t.Fatal(err)
			}
			got := "# hash: " + b.Hash + "\n" + bundleYAML(t, b)
			path := filepath.Join("testdata", "golden", name+".yaml")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s; run go test ./internal/render -update and review the diff", name)
			}
		})
	}
}

func TestRenderDefaultShape(t *testing.T) {
	ts, _ := loadRepoCatalog(t).Get("default")
	b, err := ts.Render(sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	if b.Namespace.GetName() != "pr-shop-42" {
		t.Fatalf("namespace %s", b.Namespace.GetName())
	}
	if b.Values.URL != "https://pr-shop-42.preview.example.test" {
		t.Fatalf("url %s", b.Values.URL)
	}
	var kinds []string
	for _, o := range b.Objects {
		kinds = append(kinds, o.GetKind()+"/"+o.GetName())
		if o.GetNamespace() != "pr-shop-42" {
			t.Fatalf("%s not pinned to namespace", o.GetName())
		}
		if o.GetLabels()[v1alpha1.LabelInstance] != "acme-shop-pr-42" {
			t.Fatalf("%s missing instance label", o.GetName())
		}
	}
	want := "ResourceQuota/previewd-quota,LimitRange/previewd-limits,NetworkPolicy/previewd-default-deny," +
		"NetworkPolicy/previewd-allow-same-namespace,NetworkPolicy/previewd-allow-ingress-namespaces," +
		"NetworkPolicy/previewd-allow-egress,ConfigMap/preview-info,Service/web,Deployment/web,Ingress/web"
	if got := strings.Join(kinds, ","); got != want {
		t.Fatalf("objects:\n got %s\nwant %s", got, want)
	}
	w := b.Workloads()
	if len(w) != 1 || w[0].Name != "web" {
		t.Fatalf("workloads %v", w)
	}
	if len(b.Keys()) != len(b.Objects) {
		t.Fatal("keys not unique")
	}
	ns := b.Namespace.GetLabels()
	if _, has := ns[v1alpha1.LabelExpiresAt]; has || ns["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Fatalf("namespace labels %v", ns)
	}
	port, found, _ := unstructured.NestedSlice(b.Objects[8].Object, "spec", "template", "spec", "containers")
	if !found || port[0].(map[string]any)["ports"].([]any)[0].(map[string]any)["containerPort"] != int64(8080) {
		t.Fatalf("containerPort not decoded as int64: %v", port)
	}
}

func TestHashIsDeterministicAndSensitive(t *testing.T) {
	ts, _ := loadRepoCatalog(t).Get("default")
	a, _ := ts.Render(sampleInput())
	b, _ := ts.Render(sampleInput())
	if a.Hash != b.Hash {
		t.Fatal("same input, different hash")
	}
	in := sampleInput()
	in.PullRequest.HeadSHA = "0000000000000000000000000000000000000001"
	c, _ := ts.Render(in)
	if c.Hash == a.Hash {
		t.Fatal("new head SHA did not change the hash")
	}
}

func TestLabelGatedWorkerTemplate(t *testing.T) {
	ts, _ := loadRepoCatalog(t).Get("static-site")
	in := sampleInput()
	b, err := ts.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Workloads()) != 1 {
		t.Fatalf("workloads %v", b.Workloads())
	}
	in.PullRequest.Labels = append(in.PullRequest.Labels, "preview:worker")
	b2, err := ts.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Workloads()) != 2 || b2.Hash == b.Hash {
		t.Fatalf("label did not add worker: %v", b2.Workloads())
	}
	if b2.Values.Host != "pr-42-shop.sites.example.test" || b2.Namespace.GetName() != "site-shop-42" {
		t.Fatalf("host %s ns %s", b2.Values.Host, b2.Namespace.GetName())
	}
}

// TestTitleInjectionIsNeutralised: a PR title crafted to break out of its
// YAML scalar and smuggle in extra keys must stay a plain string.
func TestTitleInjectionIsNeutralised(t *testing.T) {
	ts, _ := loadRepoCatalog(t).Get("default")
	in := sampleInput()
	in.PullRequest.Title = "x\"\n  INJECTED: yes\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\n"
	b, err := ts.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range b.Objects {
		if o.GetKind() == "ConfigMap" {
			data, _, _ := unstructured.NestedStringMap(o.Object, "data")
			if _, bad := data["INJECTED"]; bad {
				t.Fatal("title injected a key")
			}
			if data["PR_TITLE"] != in.PullRequest.Title {
				t.Fatalf("title mangled: %q", data["PR_TITLE"])
			}
		}
		if o.GetKind() == "ClusterRoleBinding" {
			t.Fatal("injected a ClusterRoleBinding")
		}
	}
}

// TestUnquotedInjectionIsCaughtByValidation: a careless template that does
// not quote a PR field is still stopped by the kind allowlist.
func TestUnquotedInjectionIsCaughtByValidation(t *testing.T) {
	fsys := fstest.MapFS{
		"sets/careless/templateset.yaml": {Data: []byte("namespacePrefix: pr\n")},
		"sets/careless/manifests/cm.yaml": {Data: []byte(
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\ndata:\n  title: {{ .PR.Title }}\n")},
	}
	ts, err := LoadTemplateSet(fsys, "sets/careless")
	if err != nil {
		t.Fatal(err)
	}
	in := sampleInput()
	in.PullRequest.Title = "x\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: pwn"
	_, err = ts.Render(in)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected allowlist rejection, got %v", err)
	}
}

func manifestSet(t *testing.T, cfg, manifest string) *TemplateSet {
	t.Helper()
	fsys := fstest.MapFS{
		"s/templateset.yaml": {Data: []byte(cfg)},
		"s/manifests/a.yaml": {Data: []byte(manifest)},
		"s/manifests/README": {Data: []byte("ignored: not *.yaml")},
		"s/manifests/z.yaml": {Data: []byte("---\n# empty document\n---\nnull\n---\n{}\n")},
		"s/manifests/c.yaml": {Data: []byte("{{- if false }}x{{ end }}")},
	}
	ts, err := LoadTemplateSet(fsys, "s")
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestValidationRejections(t *testing.T) {
	deploy := func(podSpec string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec:\n  selector: {matchLabels: {a: b}}\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n" + podSpec
	}
	cases := map[string]struct{ cfg, manifest, want string }{
		"cluster role":     {"{}", "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata: {name: r}\n", "not allowed"},
		"other namespace":  {"{}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: kube-system}\n", "may only write"},
		"no name":          {"{}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n", "no metadata.name"},
		"bad name":         {"{}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: Bad_Name}\n", "invalid"},
		"generateName":     {"{}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, generateName: c-}\n", "generateName"},
		"missing kind":     {"{}", "apiVersion: v1\nmetadata: {name: c}\n", "Kind"},
		"not a mapping":    {"{}", "- a\n- b\n", "not a mapping"},
		"bad yaml":         {"{}", "a: [\n", "invalid YAML"},
		"load balancer":    {"{}", "apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {type: LoadBalancer}\n", "LoadBalancer"},
		"host network":     {"{}", deploy("      hostNetwork: true\n      containers: [{name: c, image: i}]\n"), "hostNetwork"},
		"host path":        {"{}", deploy("      volumes: [{name: v, hostPath: {path: /}}]\n      containers: [{name: c, image: i}]\n"), "hostPath"},
		"privileged":       {"{}", deploy("      containers: [{name: c, image: i, securityContext: {privileged: true}}]\n"), "privileged"},
		"capability":       {"{}", deploy("      containers: [{name: c, image: i, securityContext: {capabilities: {add: [SYS_ADMIN]}}}]\n"), "SYS_ADMIN"},
		"host port":        {"{}", deploy("      containers: [{name: c, image: i, ports: [{containerPort: 80, hostPort: 80}]}]\n"), "hostPort"},
		"duplicate object": {"{}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n", "duplicate"},
		"missing value":    {"{}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: {{ .Vars.nope }}}\n", "map has no entry"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ts := manifestSet(t, tc.cfg, tc.manifest)
			_, err := ts.Render(sampleInput())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestAllowedServiceTypeAndCronJobPolicy(t *testing.T) {
	ts := manifestSet(t, "allowedServiceTypes: [NodePort]\nallowedKinds: [v1/Service, batch/v1/CronJob]\n",
		"apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {type: NodePort}\n---\n"+
			"apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: cj}\nspec:\n  schedule: '* * * * *'\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          hostPID: true\n          containers: [{name: c, image: i}]\n")
	_, err := ts.Render(sampleInput())
	if err == nil || !strings.Contains(err.Error(), "hostPID") {
		t.Fatalf("expected hostPID rejection, got %v", err)
	}
}

func TestRenderRequiresIdentity(t *testing.T) {
	ts := manifestSet(t, "{}", "")
	if _, err := ts.Render(Input{}); err == nil {
		t.Fatal("expected error")
	}
	b, err := ts.Render(sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	// Empty documents are skipped; only the default guard rails remain.
	if len(b.Objects) != 2 {
		t.Fatalf("objects %d", len(b.Objects))
	}
	if b.Values.URL != "" {
		t.Fatal("no host template means no URL")
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"missing config":   {map[string]string{"s/manifests/a.yaml": ""}, "templateset.yaml"},
		"unknown field":    {map[string]string{"s/templateset.yaml": "nope: 1\n"}, "unknown field"},
		"bad quantity":     {map[string]string{"s/templateset.yaml": "quota: {pods: lots}\n"}, "invalid quantity"},
		"bad limit":        {map[string]string{"s/templateset.yaml": "limitRange: {default: {cpu: x}, defaultRequest: {memory: y}}\n"}, "limitRange.default"},
		"bad scheme":       {map[string]string{"s/templateset.yaml": "urlScheme: ftp\n"}, "urlScheme"},
		"bad psa":          {map[string]string{"s/templateset.yaml": "podSecurity: yolo\n"}, "podSecurity"},
		"negative ttl":     {map[string]string{"s/templateset.yaml": "ttl: -1h\n"}, "negative"},
		"bad ttl":          {map[string]string{"s/templateset.yaml": "ttl: soon\n"}, "invalid duration"},
		"bad kind":         {map[string]string{"s/templateset.yaml": "allowedKinds: [Deployment]\n"}, "invalid kind"},
		"forbidden kind":   {map[string]string{"s/templateset.yaml": "allowedKinds: [rbac.authorization.k8s.io/v1/Role]\n"}, "may not be allowed"},
		"cluster kind":     {map[string]string{"s/templateset.yaml": "allowedKinds: [v1/Namespace]\n"}, "may not be allowed"},
		"bad service type": {map[string]string{"s/templateset.yaml": "allowedServiceTypes: [Magic]\n"}, "unknown service type"},
		"bad host":         {map[string]string{"s/templateset.yaml": "host: '{{ .Nope'\n"}, "host"},
		"bad manifest":     {map[string]string{"s/templateset.yaml": "{}", "s/manifests/a.yaml": "{{ end }}"}, "a.yaml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for p, body := range tc.files {
				fsys[p] = &fstest.MapFile{Data: []byte(body)}
			}
			_, err := LoadTemplateSet(fsys, "s")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestHostTemplateError(t *testing.T) {
	fsys := fstest.MapFS{"s/templateset.yaml": {Data: []byte("host: '{{ .Vars.missing }}'\n")}}
	ts, err := LoadTemplateSet(fsys, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Render(sampleInput()); err == nil || !strings.Contains(err.Error(), "host template") {
		t.Fatalf("got %v", err)
	}
}

func TestCatalog(t *testing.T) {
	if _, err := LoadCatalog(fstest.MapFS{}, "missing"); err == nil {
		t.Fatal("expected error for missing root")
	}
	empty := fstest.MapFS{"root/notaset/readme.txt": {Data: []byte("x")}, "root/file.txt": {Data: []byte("x")}}
	if _, err := LoadCatalog(empty, "root"); err == nil || !strings.Contains(err.Error(), "no template sets") {
		t.Fatalf("got %v", err)
	}
	broken := fstest.MapFS{"root/bad/templateset.yaml": {Data: []byte("urlScheme: gopher\n")}}
	if _, err := LoadCatalog(broken, "root"); err == nil {
		t.Fatal("expected error for broken set")
	}
	ts := manifestSet(t, "allowedKinds: [v1/ConfigMap]\n", "")
	c := NewCatalog(ts)
	if _, ok := c.Get("s"); !ok {
		t.Fatal("missing set")
	}
	if _, ok := c.Get("nope"); ok {
		t.Fatal("unexpected set")
	}
	if got := c.PruneKinds(); len(got) != 1 || got[0].Kind != "ConfigMap" {
		t.Fatalf("prune kinds %v", got)
	}
	repo := loadRepoCatalog(t)
	if n := len(repo.PruneKinds()); n != len(DefaultAllowedKinds) {
		t.Fatalf("prune kinds union = %d", n)
	}
}

func TestDurationAndTTL(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`""`)); err != nil || d.Duration != 0 {
		t.Fatal("empty duration")
	}
	if err := d.UnmarshalJSON([]byte(`"90m"`)); err != nil || d.Duration != 90*time.Minute {
		t.Fatal("90m")
	}
	b, _ := d.MarshalJSON()
	if string(b) != `"1h30m0s"` {
		t.Fatalf("marshal %s", b)
	}
	ts := manifestSet(t, "{}", "")
	if ts.TTL() != DefaultTTL {
		t.Fatal("default ttl")
	}
	ts2, _ := loadRepoCatalog(t).Get("static-site")
	if ts2.TTL() != 24*time.Hour {
		t.Fatal("static-site ttl")
	}
}

func TestParseKind(t *testing.T) {
	if g, err := ParseKind("v1/Service"); err != nil || g.Group != "" || g.Kind != "Service" {
		t.Fatal(g, err)
	}
	if g, err := ParseKind("apps/v1/Deployment"); err != nil || g.Group != "apps" {
		t.Fatal(g, err)
	}
	for _, bad := range []string{"", "Service", "a//b", "/v1/X", "a/b/c/d"} {
		if _, err := ParseKind(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestFuncMap(t *testing.T) {
	fm := funcMap()
	q := fm["quote"].(func(any) (string, error))
	if s, _ := q("a\"b\n"); s != `"a\"b\n"` {
		t.Fatalf("quote %s", s)
	}
	if fm["dnsLabel"].(func(string) string)(strings.Repeat("Ab", 50)) != strings.Repeat("ab", 31)+"a" {
		t.Fatal("dnsLabel")
	}
	if fm["trunc"].(func(int, string) string)(3, "abcdef") != "abc" || fm["trunc"].(func(int, string) string)(10, "ab") != "ab" {
		t.Fatal("trunc")
	}
	if fm["default"].(func(string, string) string)("d", "") != "d" || fm["default"].(func(string, string) string)("d", "v") != "v" {
		t.Fatal("default")
	}
	if fm["replace"].(func(string, string, string) string)("/", "-", "a/b") != "a-b" {
		t.Fatal("replace")
	}
	if fm["join"].(func(string, []string) string)(",", []string{"a", "b"}) != "a,b" {
		t.Fatal("join")
	}
	if fm["has"].(func(string, []string) bool)("z", []string{"a"}) {
		t.Fatal("has")
	}
}

func TestInputFrom(t *testing.T) {
	pe := &v1alpha1.PreviewEnvironment{}
	pe.Name = "n"
	pe.Spec.Repository = v1alpha1.RepositoryRef{Owner: "o", Name: "r"}
	pe.Spec.PullRequest.Number = 3
	in := InputFrom(pe)
	if in.Instance != "n" || in.Repo.Name != "r" || in.PullRequest.Number != 3 {
		t.Fatalf("%+v", in)
	}
}

func osDirFS() fs.FS { return os.DirFS(filepath.Join("..", "..")) }

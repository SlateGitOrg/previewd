// Package render turns a PullRequest plus a template set into a validated
// bundle of Kubernetes objects: the namespace, its guard rails
// (ResourceQuota, LimitRange, NetworkPolicies) and the application
// manifests rendered from Go templates.
//
// Rendering is deterministic: the same inputs produce byte-identical
// objects and therefore the same bundle hash, which the reconciler uses to
// decide whether anything needs applying.
package render

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"text/template"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// ConfigFile is the name of the template-set configuration file.
const ConfigFile = "templateset.yaml"

// ManifestDir is the directory (inside a template set) holding manifest
// templates. Every *.yaml file in it is rendered, in lexical order.
const ManifestDir = "manifests"

// DefaultAllowedKinds is used when a template set does not list its own.
// RBAC, cluster-scoped and node-level kinds are deliberately absent.
var DefaultAllowedKinds = []string{
	"v1/ConfigMap",
	"v1/Secret",
	"v1/Service",
	"v1/ServiceAccount",
	"v1/PersistentVolumeClaim",
	"apps/v1/Deployment",
	"apps/v1/StatefulSet",
	"batch/v1/Job",
	"networking.k8s.io/v1/Ingress",
}

// Config is the on-disk configuration of a template set.
type Config struct {
	// NamespacePrefix is prepended to generated namespace names.
	NamespacePrefix string `json:"namespacePrefix,omitempty"`
	// TTL is the default time-to-live after the last PR activity.
	TTL Duration `json:"ttl,omitempty"`
	// Host is a Go template for the preview hostname, e.g.
	// "{{ .Namespace }}.preview.example.test". Empty disables URLs.
	Host string `json:"host,omitempty"`
	// URLScheme is "https" (default) or "http".
	URLScheme string `json:"urlScheme,omitempty"`
	// PodSecurity is the Pod Security Admission level enforced on the
	// namespace: "restricted", "baseline" (default) or "privileged".
	PodSecurity string `json:"podSecurity,omitempty"`
	// Quota is the ResourceQuota hard limits, e.g. {"pods": "10"}.
	Quota map[string]string `json:"quota,omitempty"`
	// LimitRange holds container defaults so quota-limited namespaces
	// accept pods that do not declare resources.
	LimitRange *LimitRangeConfig `json:"limitRange,omitempty"`
	// NetworkPolicy configures the generated network isolation.
	NetworkPolicy NetworkPolicyConfig `json:"networkPolicy,omitempty"`
	// AllowedKinds lists "group/version/Kind" (or "v1/Kind" for core)
	// that manifests may create. Empty means DefaultAllowedKinds.
	AllowedKinds []string `json:"allowedKinds,omitempty"`
	// AllowedServiceTypes lists Service types manifests may use. Empty
	// means ClusterIP only, so previews never allocate cloud load balancers.
	AllowedServiceTypes []string `json:"allowedServiceTypes,omitempty"`
	// Vars are static values exposed to templates as .Vars.
	Vars map[string]string `json:"vars,omitempty"`
}

// LimitRangeConfig maps resource names to quantities.
type LimitRangeConfig struct {
	Default        map[string]string `json:"default,omitempty"`
	DefaultRequest map[string]string `json:"defaultRequest,omitempty"`
}

// NetworkPolicyConfig configures generated NetworkPolicies. Ingress is
// always default-deny with same-namespace traffic allowed.
type NetworkPolicyConfig struct {
	// IngressFromNamespaces may reach pods in the preview (e.g. the
	// ingress controller's namespace).
	IngressFromNamespaces []string `json:"ingressFromNamespaces,omitempty"`
	// RestrictEgress turns on default-deny egress, allowing only DNS,
	// same-namespace traffic and EgressCIDRs.
	RestrictEgress bool `json:"restrictEgress,omitempty"`
	// EgressCIDRs are additional allowed egress destinations.
	EgressCIDRs []string `json:"egressCIDRs,omitempty"`
}

// Duration is a time.Duration that (un)marshals as a Go duration string.
type Duration struct{ time.Duration }

// UnmarshalJSON parses "72h"-style strings.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}

// TemplateSet is a loaded, validated template set ready to render.
type TemplateSet struct {
	Name         string
	Config       Config
	host         *template.Template
	manifests    []*template.Template
	allowedKinds map[schema.GroupVersionKind]bool
	serviceTypes map[string]bool
}

// DefaultTTL is used when neither the template set nor the
// PreviewEnvironment sets a TTL.
const DefaultTTL = 72 * time.Hour

// TTL returns the configured TTL or DefaultTTL.
func (t *TemplateSet) TTL() time.Duration {
	if t.Config.TTL.Duration > 0 {
		return t.Config.TTL.Duration
	}
	return DefaultTTL
}

// AllowedKinds returns the allowed GVKs in a stable order. The reconciler
// uses it to know which kinds to prune.
func (t *TemplateSet) AllowedKinds() []schema.GroupVersionKind {
	out := make([]schema.GroupVersionKind, 0, len(t.allowedKinds))
	for k := range t.allowedKinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// ParseKind parses "group/version/Kind" or "version/Kind" (core group).
func ParseKind(s string) (schema.GroupVersionKind, error) {
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return schema.GroupVersionKind{Version: parts[0], Kind: parts[1]}, nil
	case len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "":
		return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}, nil
	}
	return schema.GroupVersionKind{}, fmt.Errorf("invalid kind %q: want group/version/Kind or v1/Kind", s)
}

// forbiddenKinds can never be allowed: they are cluster-scoped or grant
// privileges beyond the preview namespace.
var forbiddenKinds = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true,
	"CustomResourceDefinition": true, "MutatingWebhookConfiguration": true,
	"ValidatingWebhookConfiguration": true, "PersistentVolume": true,
	"Node": true, "PriorityClass": true, "StorageClass": true,
	"ResourceQuota": true, "LimitRange": true, "NetworkPolicy": true,
}

// LoadTemplateSet loads and validates the template set in dir of fsys.
func LoadTemplateSet(fsys fs.FS, dir string) (*TemplateSet, error) {
	name := path.Base(dir)
	raw, err := fs.ReadFile(fsys, path.Join(dir, ConfigFile))
	if err != nil {
		return nil, fmt.Errorf("template set %q: %w", name, err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("template set %q: %s: %w", name, ConfigFile, err)
	}
	ts := &TemplateSet{Name: name, Config: cfg}
	if err := ts.validateConfig(); err != nil {
		return nil, fmt.Errorf("template set %q: %w", name, err)
	}
	if cfg.Host != "" {
		ts.host, err = template.New("host").Funcs(funcMap()).Option("missingkey=error").Parse(cfg.Host)
		if err != nil {
			return nil, fmt.Errorf("template set %q: host: %w", name, err)
		}
	}
	files, err := fs.Glob(fsys, path.Join(dir, ManifestDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("template set %q: %w", name, err)
	}
	sort.Strings(files)
	for _, f := range files {
		body, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, fmt.Errorf("template set %q: %w", name, err)
		}
		tpl, err := template.New(path.Base(f)).Funcs(funcMap()).Option("missingkey=error").Parse(string(body))
		if err != nil {
			return nil, fmt.Errorf("template set %q: %w", name, err)
		}
		ts.manifests = append(ts.manifests, tpl)
	}
	return ts, nil
}

func (t *TemplateSet) validateConfig() error {
	var errs []error
	if t.Config.NamespacePrefix == "" {
		t.Config.NamespacePrefix = "pr"
	}
	switch t.Config.URLScheme {
	case "":
		t.Config.URLScheme = "https"
	case "http", "https":
	default:
		errs = append(errs, fmt.Errorf("urlScheme must be http or https, got %q", t.Config.URLScheme))
	}
	switch t.Config.PodSecurity {
	case "":
		t.Config.PodSecurity = "baseline"
	case "restricted", "baseline", "privileged":
	default:
		errs = append(errs, fmt.Errorf("podSecurity must be restricted, baseline or privileged, got %q", t.Config.PodSecurity))
	}
	if t.Config.TTL.Duration < 0 {
		errs = append(errs, errors.New("ttl must not be negative"))
	}
	errs = append(errs, validateQuantities("quota", t.Config.Quota)...)
	if lr := t.Config.LimitRange; lr != nil {
		errs = append(errs, validateQuantities("limitRange.default", lr.Default)...)
		errs = append(errs, validateQuantities("limitRange.defaultRequest", lr.DefaultRequest)...)
	}
	kinds := t.Config.AllowedKinds
	if len(kinds) == 0 {
		kinds = DefaultAllowedKinds
	}
	t.allowedKinds = map[schema.GroupVersionKind]bool{}
	for _, k := range kinds {
		gvk, err := ParseKind(k)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if forbiddenKinds[gvk.Kind] || strings.HasPrefix(gvk.Group, "rbac.") {
			errs = append(errs, fmt.Errorf("kind %s may not be allowed in previews", k))
			continue
		}
		t.allowedKinds[gvk] = true
	}
	t.serviceTypes = map[string]bool{"ClusterIP": true}
	for _, st := range t.Config.AllowedServiceTypes {
		switch st {
		case "ClusterIP", "NodePort", "LoadBalancer", "ExternalName":
			t.serviceTypes[st] = true
		default:
			errs = append(errs, fmt.Errorf("unknown service type %q", st))
		}
	}
	return errors.Join(errs...)
}

func validateQuantities(field string, m map[string]string) []error {
	var errs []error
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := resource.ParseQuantity(m[k]); err != nil {
			errs = append(errs, fmt.Errorf("%s[%s]: invalid quantity %q", field, k, m[k]))
		}
	}
	return errs
}

// Catalog is the set of template sets available to the controller, keyed by
// directory name.
type Catalog struct {
	sets map[string]*TemplateSet
}

// LoadCatalog loads every subdirectory of root that contains a
// templateset.yaml.
func LoadCatalog(fsys fs.FS, root string) (*Catalog, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	c := &Catalog{sets: map[string]*TemplateSet{}}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := path.Join(root, e.Name())
		if _, err := fs.Stat(fsys, path.Join(dir, ConfigFile)); err != nil {
			continue
		}
		ts, err := LoadTemplateSet(fsys, dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.sets[ts.Name] = ts
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if len(c.sets) == 0 {
		return nil, fmt.Errorf("load templates: no template sets found under %q", root)
	}
	return c, nil
}

// NewCatalog builds a catalog from already-loaded sets.
func NewCatalog(sets ...*TemplateSet) *Catalog {
	c := &Catalog{sets: map[string]*TemplateSet{}}
	for _, s := range sets {
		c.sets[s.Name] = s
	}
	return c
}

// Get returns the named template set.
func (c *Catalog) Get(name string) (*TemplateSet, bool) {
	ts, ok := c.sets[name]
	return ts, ok
}

// Names lists the template sets in the catalog.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.sets))
	for n := range c.sets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// PruneKinds is the union of allowed kinds across the catalog; anything the
// controller might have created is one of these.
func (c *Catalog) PruneKinds() []schema.GroupVersionKind {
	seen := map[schema.GroupVersionKind]bool{}
	var out []schema.GroupVersionKind
	for _, n := range c.Names() {
		for _, k := range c.sets[n].AllowedKinds() {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

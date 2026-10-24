package render

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/naming"
)

// Bundle is the full desired state of one preview environment.
type Bundle struct {
	// Namespace is the preview namespace object.
	Namespace *unstructured.Unstructured
	// Objects are the namespaced objects in apply order: guard rails
	// first, then application manifests.
	Objects []*unstructured.Unstructured
	// Hash is a content hash over Namespace and Objects.
	Hash string
	// Values are the template values used, for status and diagnostics.
	Values Values
}

// ObjectKey identifies an object inside the preview namespace.
type ObjectKey struct {
	GVK  schema.GroupVersionKind
	Name string
}

// Keys returns the identity of every namespaced object in the bundle.
func (b *Bundle) Keys() map[ObjectKey]bool {
	out := make(map[ObjectKey]bool, len(b.Objects))
	for _, o := range b.Objects {
		out[ObjectKey{GVK: o.GroupVersionKind(), Name: o.GetName()}] = true
	}
	return out
}

// Workloads returns the Deployments and StatefulSets whose availability
// gates the Ready phase.
func (b *Bundle) Workloads() []ObjectKey {
	var out []ObjectKey
	for _, o := range b.Objects {
		gvk := o.GroupVersionKind()
		if gvk.Group == "apps" && (gvk.Kind == "Deployment" || gvk.Kind == "StatefulSet") {
			out = append(out, ObjectKey{GVK: gvk, Name: o.GetName()})
		}
	}
	return out
}

// Render produces the bundle for in. Errors are user-facing: they end up in
// the PreviewEnvironment status and the SCM failure status.
func (t *TemplateSet) Render(in Input) (*Bundle, error) {
	if in.Instance == "" || in.Repo.Owner == "" || in.Repo.Name == "" || in.PullRequest.Number < 1 {
		return nil, errors.New("render: instance, repository and pull request number are required")
	}
	v, err := t.values(in)
	if err != nil {
		return nil, err
	}
	common := t.commonLabels(in)
	var apps []*unstructured.Unstructured
	seen := map[ObjectKey]string{}
	for _, tpl := range t.manifests {
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, v); err != nil {
			return nil, fmt.Errorf("template %s: %w", tpl.Name(), err)
		}
		objs, err := decodeDocuments(buf.Bytes())
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", tpl.Name(), err)
		}
		for i, o := range objs {
			where := fmt.Sprintf("%s[%d]", tpl.Name(), i)
			if err := t.validateObject(o, v.Namespace); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			key := ObjectKey{GVK: o.GroupVersionKind(), Name: o.GetName()}
			if prev, dup := seen[key]; dup {
				return nil, fmt.Errorf("%s: duplicate %s %q (also in %s)", where, key.GVK.Kind, key.Name, prev)
			}
			seen[key] = where
			o.SetNamespace(v.Namespace)
			mergeStringMap(o.SetLabels, o.GetLabels(), common)
			mergeStringMap(o.SetAnnotations, o.GetAnnotations(), map[string]string{
				v1alpha1.AnnotationSHA: in.PullRequest.HeadSHA,
			})
			apps = append(apps, o)
		}
	}
	sortByApplyOrder(apps)

	ns := t.namespaceObject(v, in, common)
	objects := append(t.guardRails(v, common), apps...)
	b := &Bundle{Namespace: ns, Objects: objects, Values: v}
	b.Hash, err = hashObjects(append([]*unstructured.Unstructured{ns}, objects...))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// commonLabels are stamped on every object the controller creates. The
// instance label is what pruning and orphan collection select on.
func (t *TemplateSet) commonLabels(in Input) map[string]string {
	return map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelInstance:  in.Instance,
		v1alpha1.LabelRepo:      naming.LabelValue(in.Repo.Owner + "." + in.Repo.Name),
		v1alpha1.LabelPR:        strconv.Itoa(in.PullRequest.Number),
	}
}

func (t *TemplateSet) namespaceObject(v Values, in Input, common map[string]string) *unstructured.Unstructured {
	labels := map[string]string{
		"pod-security.kubernetes.io/enforce": t.Config.PodSecurity,
		"pod-security.kubernetes.io/warn":    t.Config.PodSecurity,
	}
	for k, val := range common {
		labels[k] = val
	}
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(v.Namespace)
	ns.SetLabels(labels)
	ns.SetAnnotations(map[string]string{
		v1alpha1.AnnotationSHA:   in.PullRequest.HeadSHA,
		v1alpha1.AnnotationOwner: in.Instance,
	})
	return ns
}

// decodeDocuments splits a multi-document YAML stream into objects,
// skipping empty documents (e.g. a template whose body is conditional).
// Numbers decode as int64 where integral, matching what the API server
// returns, so hashes and nested-field lookups are stable.
func decodeDocuments(data []byte) ([]*unstructured.Unstructured, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []*unstructured.Unstructured
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
		js, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
		trimmed := bytes.TrimSpace(js)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
			continue
		}
		if trimmed[0] != '{' {
			return nil, fmt.Errorf("document is not a mapping: %.40s", trimmed)
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(js); err != nil {
			return nil, fmt.Errorf("invalid object: %w", err)
		}
		out = append(out, u)
	}
}

func mergeStringMap(set func(map[string]string), existing, extra map[string]string) {
	out := make(map[string]string, len(existing)+len(extra))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	set(out)
}

// applyOrder ranks kinds so dependencies are applied first (config before
// workloads, workloads before ingress). Unknown kinds go last.
var applyOrder = map[string]int{
	"ResourceQuota": 0, "LimitRange": 1, "NetworkPolicy": 2,
	"ServiceAccount": 3, "Secret": 4, "ConfigMap": 5,
	"PersistentVolumeClaim": 6, "Service": 7, "Deployment": 8,
	"StatefulSet": 9, "Job": 10, "CronJob": 11, "Ingress": 12,
}

func rank(kind string) int {
	if r, ok := applyOrder[kind]; ok {
		return r
	}
	return len(applyOrder)
}

func sortByApplyOrder(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		ri, rj := rank(objs[i].GetKind()), rank(objs[j].GetKind())
		if ri != rj {
			return ri < rj
		}
		if objs[i].GetKind() != objs[j].GetKind() {
			return objs[i].GetKind() < objs[j].GetKind()
		}
		return objs[i].GetName() < objs[j].GetName()
	})
}

// hashObjects hashes the canonical JSON encoding of objs. encoding/json
// sorts map keys, so equal objects always hash equally.
func hashObjects(objs []*unstructured.Unstructured) (string, error) {
	h := sha256.New()
	for _, o := range objs {
		b, err := json.Marshal(o.Object)
		if err != nil {
			return "", fmt.Errorf("hash: %w", err)
		}
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

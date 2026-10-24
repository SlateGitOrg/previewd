package render

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Names of the generated guard-rail objects.
const (
	QuotaName              = "previewd-quota"
	LimitRangeName         = "previewd-limits"
	PolicyDefaultDeny      = "previewd-default-deny"
	PolicySameNamespace    = "previewd-allow-same-namespace"
	PolicyIngressNamespace = "previewd-allow-ingress-namespaces"
	PolicyEgress           = "previewd-allow-egress"
)

// guardRails builds the ResourceQuota, LimitRange and NetworkPolicies that
// every preview namespace gets regardless of what the app templates say.
func (t *TemplateSet) guardRails(v Values, labels map[string]string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	newObj := func(apiVersion, kind, name string, spec map[string]any) {
		o := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": v.Namespace,
				"labels":    toAnyMap(labels),
			},
			"spec": spec,
		}}
		out = append(out, o)
	}

	if len(t.Config.Quota) > 0 {
		newObj("v1", "ResourceQuota", QuotaName, map[string]any{"hard": toAnyMap(t.Config.Quota)})
	}
	if lr := t.Config.LimitRange; lr != nil && (len(lr.Default) > 0 || len(lr.DefaultRequest) > 0) {
		limit := map[string]any{"type": "Container"}
		if len(lr.Default) > 0 {
			limit["default"] = toAnyMap(lr.Default)
		}
		if len(lr.DefaultRequest) > 0 {
			limit["defaultRequest"] = toAnyMap(lr.DefaultRequest)
		}
		newObj("v1", "LimitRange", LimitRangeName, map[string]any{"limits": []any{limit}})
	}

	np := t.Config.NetworkPolicy
	policyTypes := []any{"Ingress"}
	if np.RestrictEgress {
		policyTypes = append(policyTypes, "Egress")
	}
	allPods := map[string]any{}
	newObj("networking.k8s.io/v1", "NetworkPolicy", PolicyDefaultDeny, map[string]any{
		"podSelector": allPods,
		"policyTypes": policyTypes,
	})
	newObj("networking.k8s.io/v1", "NetworkPolicy", PolicySameNamespace, map[string]any{
		"podSelector": map[string]any{},
		"policyTypes": []any{"Ingress"},
		"ingress":     []any{map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{}}}}},
	})
	if len(np.IngressFromNamespaces) > 0 {
		newObj("networking.k8s.io/v1", "NetworkPolicy", PolicyIngressNamespace, map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{"from": []any{map[string]any{
				"namespaceSelector": nameSelector(np.IngressFromNamespaces),
			}}}},
		})
	}
	if np.RestrictEgress {
		egress := []any{
			// Same-namespace traffic.
			map[string]any{"to": []any{map[string]any{"podSelector": map[string]any{}}}},
			// Cluster DNS.
			map[string]any{
				"to": []any{map[string]any{"namespaceSelector": nameSelector([]string{"kube-system"})}},
				"ports": []any{
					map[string]any{"protocol": "UDP", "port": int64(53)},
					map[string]any{"protocol": "TCP", "port": int64(53)},
				},
			},
		}
		if len(np.EgressCIDRs) > 0 {
			var to []any
			for _, c := range np.EgressCIDRs {
				to = append(to, map[string]any{"ipBlock": map[string]any{"cidr": c}})
			}
			egress = append(egress, map[string]any{"to": to})
		}
		newObj("networking.k8s.io/v1", "NetworkPolicy", PolicyEgress, map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{"Egress"},
			"egress":      egress,
		})
	}
	return out
}

// nameSelector selects namespaces by the immutable
// kubernetes.io/metadata.name label.
func nameSelector(names []string) map[string]any {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	values := make([]any, len(sorted))
	for i, n := range sorted {
		values[i] = n
	}
	return map[string]any{"matchExpressions": []any{map[string]any{
		"key":      "kubernetes.io/metadata.name",
		"operator": "In",
		"values":   values,
	}}}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

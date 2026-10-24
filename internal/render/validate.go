package render

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// validateObject enforces the rules that make it safe to apply rendered
// output with the controller's credentials:
//
//   - the kind is on the template set's allowlist (no RBAC, no
//     cluster-scoped objects);
//   - the object lives in the preview namespace;
//   - pod templates do not request host access or privileged containers;
//   - Services use an allowed type (ClusterIP by default).
//
// Templates are operator-authored, but they interpolate PR-controlled
// strings, so these checks are the backstop against template injection.
func (t *TemplateSet) validateObject(o *unstructured.Unstructured, namespace string) error {
	gvk := o.GroupVersionKind()
	if gvk.Kind == "" || gvk.Version == "" {
		return errors.New("object is missing apiVersion or kind")
	}
	if !t.allowedKinds[gvk] {
		return fmt.Errorf("kind %s is not allowed by template set %q", gvk.String(), t.Name)
	}
	name := o.GetName()
	if name == "" {
		return fmt.Errorf("%s has no metadata.name", gvk.Kind)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("%s name %q is invalid: %v", gvk.Kind, name, errs)
	}
	if ns := o.GetNamespace(); ns != "" && ns != namespace {
		return fmt.Errorf("%s %q targets namespace %q; previews may only write to %q", gvk.Kind, name, ns, namespace)
	}
	if o.GetGenerateName() != "" {
		return fmt.Errorf("%s %q uses generateName, which cannot be pruned deterministically", gvk.Kind, name)
	}
	if gvk.Group == "" && gvk.Kind == "Service" {
		st, _, _ := unstructured.NestedString(o.Object, "spec", "type")
		if st == "" {
			st = "ClusterIP"
		}
		if !t.serviceTypes[st] {
			return fmt.Errorf("Service %q uses type %s, which is not allowed", name, st)
		}
	}
	if spec, ok := podSpec(o); ok {
		if err := checkPodSpec(spec); err != nil {
			return fmt.Errorf("%s %q: %w", gvk.Kind, name, err)
		}
	}
	return nil
}

// podSpec finds the pod template spec of common workload kinds.
func podSpec(o *unstructured.Unstructured) (map[string]any, bool) {
	var fields []string
	switch o.GetKind() {
	case "Pod":
		fields = []string{"spec"}
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
		fields = []string{"spec", "template", "spec"}
	case "CronJob":
		fields = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		return nil, false
	}
	m, found, err := unstructured.NestedMap(o.Object, fields...)
	if err != nil || !found {
		return nil, false
	}
	return m, true
}

var dangerousCapabilities = map[string]bool{"ALL": true, "SYS_ADMIN": true, "NET_ADMIN": true, "SYS_PTRACE": true, "SYS_MODULE": true}

func checkPodSpec(spec map[string]any) error {
	var errs []error
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if v, _, _ := unstructured.NestedBool(spec, f); v {
			errs = append(errs, fmt.Errorf("%s is not allowed", f))
		}
	}
	vols, _, _ := unstructured.NestedSlice(spec, "volumes")
	for _, raw := range vols {
		if vol, ok := raw.(map[string]any); ok {
			if _, has := vol["hostPath"]; has {
				errs = append(errs, fmt.Errorf("hostPath volume %v is not allowed", vol["name"]))
			}
		}
	}
	for _, list := range []string{"initContainers", "containers", "ephemeralContainers"} {
		cs, _, _ := unstructured.NestedSlice(spec, list)
		for _, raw := range cs {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if priv, _, _ := unstructured.NestedBool(c, "securityContext", "privileged"); priv {
				errs = append(errs, fmt.Errorf("container %v is privileged", c["name"]))
			}
			adds, _, _ := unstructured.NestedStringSlice(c, "securityContext", "capabilities", "add")
			for _, capName := range adds {
				if dangerousCapabilities[capName] {
					errs = append(errs, fmt.Errorf("container %v adds capability %s", c["name"], capName))
				}
			}
			ports, _, _ := unstructured.NestedSlice(c, "ports")
			for _, p := range ports {
				if pm, ok := p.(map[string]any); ok {
					if _, has := pm["hostPort"]; has {
						errs = append(errs, fmt.Errorf("container %v uses a hostPort", c["name"]))
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}

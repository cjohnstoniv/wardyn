// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// A platform-owned agent-sandbox SandboxTemplate (WARDYN_K8S_AGENT_SANDBOX_TEMPLATE, experimental) as the
// base of the agent pod. Wardyn still builds the whole pod; from the template it takes only where the pod
// is scheduled and a set of annotations. Anything else the template sets refuses the run before any
// Sandbox exists, so a template field never silently fails to apply. No build tag: the parse and the
// merge are plain data and are tested without a cluster.

// sandboxTemplateAnnotationPrefix is the one annotation namespace a template may set on the agent pod.
const sandboxTemplateAnnotationPrefix = "sandbox-template.wardyn.dev/"

// sandboxTemplateTakes is the template's pod spec fields Wardyn copies, in the order its refusals name them.
var sandboxTemplateTakes = []string{"nodeSelector", "tolerations", "affinity", "topologySpreadConstraints", "priorityClassName", "schedulerName", "runtimeClassName"}

// sandboxTemplateSpecDefaults are the template spec fields read only by the extensions controller, which
// Wardyn does not run, at the values the API server defaults them to. Any other value is refused.
var sandboxTemplateSpecDefaults = map[string]any{
	"envVarsInjectionPolicy":     "Disallowed",
	"failedPodPolicy":            "Retain",
	"networkPolicyManagement":    "Managed",
	"volumeClaimTemplatesPolicy": "Disallowed",
	"service":                    false,
}

// sandboxTemplateBase is what a template contributes to the agent pod.
type sandboxTemplateBase struct {
	spec        corev1.PodSpec // only the sandboxTemplateTakes fields are ever set
	annotations map[string]string
}

// parseSandboxTemplate reads a SandboxTemplate object (its unstructured form) into what Wardyn takes from it,
// or refuses it, naming the first field outside the allowlist. An empty value (null, "", [] or {}) sets
// nothing and is accepted; false and 0 are settings and are refused.
func parseSandboxTemplate(obj map[string]any) (sandboxTemplateBase, error) {
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return sandboxTemplateBase{}, fmt.Errorf("spec is missing")
	}
	for _, k := range slices.Sorted(maps.Keys(spec)) {
		v := spec[k]
		if k == "podTemplate" || templateValueEmpty(v) {
			continue
		}
		if def, known := sandboxTemplateSpecDefaults[k]; known && v == def {
			continue
		}
		return sandboxTemplateBase{}, fmt.Errorf("spec.%s is refused: Wardyn creates a bare Sandbox and runs no extensions controller, so the template's own fields would not apply", k)
	}
	tmpl, ok := spec["podTemplate"].(map[string]any)
	if !ok {
		return sandboxTemplateBase{}, fmt.Errorf("spec.podTemplate is missing")
	}
	var base sandboxTemplateBase
	for _, k := range slices.Sorted(maps.Keys(tmpl)) {
		switch v := tmpl[k]; {
		case k == "spec":
		case k == "metadata":
			ann, err := templateAnnotations(v)
			if err != nil {
				return sandboxTemplateBase{}, err
			}
			base.annotations = ann
		case !templateValueEmpty(v):
			return sandboxTemplateBase{}, fmt.Errorf("spec.podTemplate.%s is refused", k)
		}
	}
	podSpec, _ := tmpl["spec"].(map[string]any)
	taken := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(podSpec)) {
		v := podSpec[k]
		switch {
		case slices.Contains(sandboxTemplateTakes, k):
			taken[k] = v
		case !templateValueEmpty(v):
			return sandboxTemplateBase{}, fmt.Errorf("spec.podTemplate.spec.%s is refused: Wardyn builds the agent pod itself and takes only %s from a template", k, strings.Join(sandboxTemplateTakes, ", "))
		}
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(taken, &base.spec); err != nil {
		return sandboxTemplateBase{}, fmt.Errorf("spec.podTemplate.spec: %w", err)
	}
	return base, base.validate()
}

// templateAnnotations is the template's pod annotations, all under sandboxTemplateAnnotationPrefix. Pod
// labels are refused: a label matching a platform NetworkPolicy that allows egress would widen the pod.
func templateAnnotations(meta any) (map[string]string, error) {
	m, _ := meta.(map[string]any)
	var out map[string]string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v := m[k]
		switch {
		case templateValueEmpty(v):
		case k == "labels":
			return nil, fmt.Errorf("spec.podTemplate.metadata.labels is refused: a pod label can match a NetworkPolicy that widens the agent pod's egress")
		case k == "annotations":
			ann, _ := v.(map[string]any)
			out = make(map[string]string, len(ann))
			for ak, av := range ann {
				s, isString := av.(string)
				name, under := strings.CutPrefix(ak, sandboxTemplateAnnotationPrefix)
				if !under || name == "" || len(validation.IsQualifiedName(ak)) > 0 || !isString {
					return nil, fmt.Errorf("spec.podTemplate.metadata.annotations key %q is refused: a template sets only string annotations under %s", ak, sandboxTemplateAnnotationPrefix)
				}
				out[ak] = s
			}
		default:
			return nil, fmt.Errorf("spec.podTemplate.metadata.%s is refused", k)
		}
	}
	return out, nil
}

// validate refuses a malformed name or node-selector entry here, before a Sandbox exists, rather than at
// the controller's pod create.
func (b sandboxTemplateBase) validate() error {
	for k, v := range b.spec.NodeSelector {
		if len(validation.IsQualifiedName(k)) > 0 || len(validation.IsValidLabelValue(v)) > 0 {
			return fmt.Errorf("spec.podTemplate.spec.nodeSelector %q=%q is not a valid label", k, v)
		}
	}
	names := map[string]string{"priorityClassName": b.spec.PriorityClassName, "schedulerName": b.spec.SchedulerName}
	if b.spec.RuntimeClassName != nil {
		names["runtimeClassName"] = *b.spec.RuntimeClassName
	}
	for _, field := range slices.Sorted(maps.Keys(names)) {
		if v := names[field]; v != "" && len(validation.IsDNS1123Subdomain(v)) > 0 {
			return fmt.Errorf("spec.podTemplate.spec.%s %q is not a valid name", field, v)
		}
	}
	return nil
}

// applyTo lays the template onto Wardyn's agent pod. A field Wardyn already set (placement, the confinement
// class's RuntimeClass) stands: the template may repeat it but never change it, and a different value
// refuses the run. Tolerations are added; a node-selector key or annotation is set when Wardyn has none.
func (b sandboxTemplateBase) applyTo(pod *corev1.Pod) error {
	s := &pod.Spec
	for _, k := range slices.Sorted(maps.Keys(b.spec.NodeSelector)) {
		v := b.spec.NodeSelector[k]
		if have, set := s.NodeSelector[k]; set && have != v {
			return fmt.Errorf("nodeSelector %q=%q conflicts with Wardyn's %q", k, v, have)
		}
		if s.NodeSelector == nil {
			s.NodeSelector = map[string]string{}
		}
		s.NodeSelector[k] = v
	}
	for _, tol := range b.spec.Tolerations {
		if !slices.ContainsFunc(s.Tolerations, func(t corev1.Toleration) bool { return reflect.DeepEqual(t, tol) }) {
			s.Tolerations = append(s.Tolerations, *tol.DeepCopy())
		}
	}
	if b.spec.Affinity != nil {
		if s.Affinity != nil && !reflect.DeepEqual(s.Affinity, b.spec.Affinity) {
			return fmt.Errorf("affinity conflicts with the affinity Wardyn's placement sets")
		}
		s.Affinity = b.spec.Affinity.DeepCopy()
	}
	if len(b.spec.TopologySpreadConstraints) > 0 {
		if len(s.TopologySpreadConstraints) > 0 && !reflect.DeepEqual(s.TopologySpreadConstraints, b.spec.TopologySpreadConstraints) {
			return fmt.Errorf("topologySpreadConstraints conflict with Wardyn's")
		}
		s.TopologySpreadConstraints = slices.Clone(b.spec.TopologySpreadConstraints)
	}
	if err := takeName("priorityClassName", &s.PriorityClassName, b.spec.PriorityClassName); err != nil {
		return err
	}
	if err := takeName("schedulerName", &s.SchedulerName, b.spec.SchedulerName); err != nil {
		return err
	}
	if rc := b.spec.RuntimeClassName; rc != nil && *rc != "" {
		if s.RuntimeClassName != nil && *s.RuntimeClassName != "" && *s.RuntimeClassName != *rc {
			return fmt.Errorf("runtimeClassName %q differs from %q, the RuntimeClass this run's confinement class sets", *rc, *s.RuntimeClassName)
		}
		name := *rc
		s.RuntimeClassName = &name
	}
	for _, k := range slices.Sorted(maps.Keys(b.annotations)) {
		v := b.annotations[k]
		if have, set := pod.Annotations[k]; set && have != v {
			return fmt.Errorf("annotation %q=%q conflicts with Wardyn's %q", k, v, have)
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[k] = v
	}
	return nil
}

// takeName sets *have to the template's name unless Wardyn set a different one, which refuses the run.
func takeName(field string, have *string, template string) error {
	if template == "" {
		return nil
	}
	if *have != "" && *have != template {
		return fmt.Errorf("%s %q conflicts with Wardyn's %q", field, template, *have)
	}
	*have = template
	return nil
}

// templateValueEmpty is a value that sets nothing: null, "", [] or {}.
func templateValueEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

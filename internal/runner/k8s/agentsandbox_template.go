// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// A platform-owned agent-sandbox SandboxTemplate (WARDYN_K8S_AGENT_SANDBOX_TEMPLATE, experimental) read for
// the agent pod. Wardyn still builds the whole pod, and from the template takes only annotations under one
// prefix. Placement and runtime come only from Wardyn's own settings, because the boot canary proves
// NetworkPolicy enforcement on that placement and no other. Anything else the template sets refuses the run
// before any Sandbox exists, so a template field never silently fails to apply. No build tag: the parse and
// the merge are plain data and are tested without a cluster.

// sandboxTemplateAnnotationPrefix is the one annotation namespace a template may set on the agent pod.
const sandboxTemplateAnnotationPrefix = "sandbox-template.wardyn.dev/"

// sandboxTemplateSpecDefaults are the template spec fields read only by the extensions controller, which
// Wardyn does not run, at the values the API server defaults them to. Any other value is refused.
var sandboxTemplateSpecDefaults = map[string]any{
	"envVarsInjectionPolicy":     "Disallowed",
	"failedPodPolicy":            "Retain",
	"networkPolicyManagement":    "Managed",
	"volumeClaimTemplatesPolicy": "Disallowed",
	"service":                    false,
}

// sandboxTemplateBase is what a template contributes to the agent pod: annotations only.
type sandboxTemplateBase struct {
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
	for _, k := range slices.Sorted(maps.Keys(podSpec)) {
		if !templateValueEmpty(podSpec[k]) {
			return sandboxTemplateBase{}, fmt.Errorf("spec.podTemplate.spec.%s is refused: Wardyn builds the whole agent pod and takes its placement and runtime only from its own settings, where the boot canary proves NetworkPolicy enforcement; a template sets only annotations under %s", k, sandboxTemplateAnnotationPrefix)
		}
	}
	return base, nil
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

// applyTo lays the template's annotations onto Wardyn's agent pod. An annotation Wardyn already set stands:
// the template may repeat it, and a different value refuses the run.
func (b sandboxTemplateBase) applyTo(pod *corev1.Pod) error {
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

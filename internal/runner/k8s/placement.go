// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// safeToEvictKey is the one Kubernetes-owned annotation a placement may set: an operator who
// accepts that the autoscaler may evict a run pod says so with it. It reaches no confinement.
const safeToEvictKey = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// Placement is where, and with what operator metadata, every pod this substrate creates is put: the
// agent pod, the proxy pod and the boot canary. WARDYN_K8S_SANDBOX_PLACEMENT carries it as JSON,
// rendered by the chart from k8s.sandbox.*. The canary takes the same placement so it proves
// NetworkPolicy enforcement on the nodes runs use.
//
// It is deliberately not a PodSpec patch: a values file must not reach the security context, service
// links or token automount that sandbox.go sets.
type Placement struct {
	NodeSelector      map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations       []corev1.Toleration `json:"tolerations,omitempty"`
	Affinity          *corev1.Affinity    `json:"affinity,omitempty"`
	PriorityClassName string              `json:"priorityClassName,omitempty"`
	PodAnnotations    map[string]string   `json:"podAnnotations,omitempty"`
	PodLabels         map[string]string   `json:"podLabels,omitempty"`
}

// parsePlacement decodes and validates WARDYN_K8S_SANDBOX_PLACEMENT. Empty means no placement. An
// unknown field is refused, so a misspelt key does not silently place nothing.
func parsePlacement(raw string) (Placement, error) {
	var p Placement
	if strings.TrimSpace(raw) == "" {
		return p, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Placement{}, fmt.Errorf("k8s: WARDYN_K8S_SANDBOX_PLACEMENT: %w", err)
	}
	if err := p.validate(); err != nil {
		return Placement{}, fmt.Errorf("k8s: WARDYN_K8S_SANDBOX_PLACEMENT: %w", err)
	}
	return p, nil
}

// validate refuses, never merges or drops, anything that could reach confinement: a reserved wardyn
// label (the NetworkPolicies select on them) and any kubernetes.io/ or k8s.io/ key other than
// safeToEvictKey (an AppArmor "unconfined" annotation is one such key).
func (p Placement) validate() error {
	for k, v := range p.PodLabels {
		if err := checkPlacementKey("podLabels", k); err != nil {
			return err
		}
		switch k {
		case labelManaged, labelRun, labelComponent:
			return fmt.Errorf("podLabels key %q is reserved: the run NetworkPolicies select on it", k)
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return fmt.Errorf("podLabels %q has an invalid value %q: %s", k, v, strings.Join(errs, "; "))
		}
	}
	for k := range p.PodAnnotations {
		if err := checkPlacementKey("podAnnotations", k); err != nil {
			return err
		}
	}
	if p.PriorityClassName != "" {
		if errs := validation.IsDNS1123Subdomain(p.PriorityClassName); len(errs) > 0 {
			return fmt.Errorf("priorityClassName %q is invalid: %s", p.PriorityClassName, strings.Join(errs, "; "))
		}
	}
	return nil
}

func checkPlacementKey(field, key string) error {
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return fmt.Errorf("%s key %q is invalid: %s", field, key, strings.Join(errs, "; "))
	}
	if key == safeToEvictKey {
		return nil
	}
	if prefix, _, ok := strings.Cut(strings.ToLower(key), "/"); ok &&
		(strings.HasSuffix(prefix, "kubernetes.io") || strings.HasSuffix(prefix, "k8s.io")) {
		return fmt.Errorf("%s key %q is refused: kubernetes.io/ and k8s.io/ keys can loosen confinement (only %s is allowed)", field, key, safeToEvictKey)
	}
	return nil
}

// apply puts the placement on pod and (re)writes its labels: the operator's podLabels join extra, and
// wardynLabels writes the reserved managed, run and component labels last. The one place every pod
// the substrate creates takes its placement from.
func (p Placement) apply(pod *corev1.Pod, runID uuid.UUID, component string, extra map[string]string) {
	labels := maps.Clone(extra)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, p.PodLabels)
	pod.Labels = wardynLabels(runID, component, labels)
	if len(p.PodAnnotations) > 0 {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		maps.Copy(pod.Annotations, p.PodAnnotations)
	}
	pod.Spec.NodeSelector = maps.Clone(p.NodeSelector)
	for i := range p.Tolerations {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, *p.Tolerations[i].DeepCopy())
	}
	pod.Spec.Affinity = p.Affinity.DeepCopy()
	pod.Spec.PriorityClassName = p.PriorityClassName
}

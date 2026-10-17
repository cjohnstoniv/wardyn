// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testPlacementJSON = `{
  "nodeSelector": {"pool": "sandbox"},
  "tolerations": [{"key": "sandbox", "operator": "Equal", "value": "true", "effect": "NoSchedule"}],
  "affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
    {"matchExpressions": [{"key": "zone", "operator": "In", "values": ["a"]}]}]}}},
  "priorityClassName": "sandbox-low",
  "podAnnotations": {"cluster-autoscaler.kubernetes.io/safe-to-evict": "true", "team.example.com/owner": "platform"},
  "podLabels": {"team": "platform"}
}`

// assertPlaced checks one pod carries the whole of testPlacementJSON.
func assertPlaced(t *testing.T, what string, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.NodeSelector["pool"] != "sandbox" {
		t.Errorf("%s nodeSelector = %v, want pool=sandbox", what, pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "sandbox" {
		t.Errorf("%s tolerations = %v, want the sandbox toleration", what, pod.Spec.Tolerations)
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		t.Errorf("%s affinity = %v, want the node affinity", what, pod.Spec.Affinity)
	}
	if pod.Spec.PriorityClassName != "sandbox-low" {
		t.Errorf("%s priorityClassName = %q, want sandbox-low", what, pod.Spec.PriorityClassName)
	}
	if pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"] != "true" || pod.Annotations["team.example.com/owner"] != "platform" {
		t.Errorf("%s annotations = %v, want both placement annotations", what, pod.Annotations)
	}
	if pod.Labels["team"] != "platform" {
		t.Errorf("%s labels = %v, want team=platform", what, pod.Labels)
	}
	if pod.Labels[labelManaged] != "true" || pod.Labels[labelRun] == "" || pod.Labels[labelComponent] == "" {
		t.Errorf("%s labels = %v, want the reserved managed, run and component labels", what, pod.Labels)
	}
}

// TestPlacement_AllThreePods is the P1 spine: the agent pod, the proxy pod and the canary pod take
// the same placement. A canary placed differently would prove NetworkPolicy enforcement on nodes runs
// never use.
func TestPlacement_AllThreePods(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	installDeleteCollectionSupport(t, cs)
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{
		Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", SandboxPlacement: testPlacementJSON,
	})
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	spec := testSandboxSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	canaries := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" || a.GetResource().Resource != "pods" {
			continue
		}
		pod := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		switch {
		case strings.HasPrefix(pod.Name, "wardyn-egress-canary-"):
			canaries++
			assertPlaced(t, "canary pod", pod)
		case pod.Name == proxyPodName(spec.RunID):
			assertPlaced(t, "proxy pod", pod)
			if pod.Labels["team"] != "platform" {
				t.Errorf("proxy pod label team = %q, want the placement value to beat the run label %q", pod.Labels["team"], spec.Labels["team"])
			}
		case pod.Name == agentPodName(spec.RunID):
			assertPlaced(t, "agent pod", pod)
		}
	}
	if canaries != 2 {
		t.Errorf("canary pods created = %d, want 2 (phase A and phase B), each placed", canaries)
	}
}

// TestPlacement_CanaryNetPolSelectorUnchanged: placement labels go on the pods only. The canary's
// deny-all policy still selects on exactly the three reserved labels.
func TestPlacement_CanaryNetPolSelectorUnchanged(t *testing.T) {
	_, cs := newTestDriver(t, Config{SandboxPlacement: testPlacementJSON})
	seen := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" || a.GetResource().Resource != "networkpolicies" {
			continue
		}
		seen++
		np := a.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		if got := np.Spec.PodSelector.MatchLabels; len(got) != 3 || got["team"] != "" {
			t.Errorf("canary NetworkPolicy selector = %v, want exactly the three reserved labels", got)
		}
	}
	if seen != 1 {
		t.Errorf("canary NetworkPolicies created = %d, want 1 (phase B)", seen)
	}
}

// TestPlacement_EmptyIsNoOp: no placement leaves the pods as they were.
func TestPlacement_EmptyIsNoOp(t *testing.T) {
	var p Placement
	pod := &corev1.Pod{}
	id := uuid.New()
	p.apply(pod, id, componentAgent, nil)
	if pod.Spec.NodeSelector != nil || pod.Spec.Tolerations != nil || pod.Spec.Affinity != nil ||
		pod.Spec.PriorityClassName != "" || pod.Annotations != nil {
		t.Errorf("empty placement changed the pod: %+v", pod)
	}
	if pod.Labels[labelRun] != id.String() || len(pod.Labels) != 3 {
		t.Errorf("labels = %v, want exactly the three reserved labels", pod.Labels)
	}
}

// TestPlacement_RefusedAtBoot: every refusal is a boot error that names the key, never a silent drop
// or merge. safe-to-evict is the one Kubernetes-owned key accepted.
func TestPlacement_RefusedAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name, json, wantKey string // wantKey == "" => must boot
	}{
		{"apparmor annotation", `{"podAnnotations": {"container.apparmor.security.beta.kubernetes.io/agent": "unconfined"}}`, "container.apparmor.security.beta.kubernetes.io/agent"},
		{"seccomp annotation", `{"podAnnotations": {"seccomp.security.alpha.kubernetes.io/pod": "unconfined"}}`, "seccomp.security.alpha.kubernetes.io/pod"},
		{"k8s.io annotation", `{"podAnnotations": {"foo.k8s.io/x": "y"}}`, "foo.k8s.io/x"},
		{"kubernetes.io label", `{"podLabels": {"app.kubernetes.io/name": "x"}}`, "app.kubernetes.io/name"},
		{"safe-to-evict as a label", `{"podLabels": {"cluster-autoscaler.kubernetes.io/safe-to-evict": "true"}}`, ""},
		{"reserved managed label", `{"podLabels": {"wardyn.managed": "false"}}`, "wardyn.managed"},
		{"reserved run label", `{"podLabels": {"wardyn.run-id": "x"}}`, "wardyn.run-id"},
		{"reserved component label", `{"podLabels": {"wardyn.component": "proxy"}}`, "wardyn.component"},
		{"bad label value", `{"podLabels": {"team": "has space"}}`, "team"},
		{"unknown field", `{"nodeSelctor": {"pool": "x"}}`, "nodeSelctor"},
		{"not json", `pool=sandbox`, "invalid character"},
		{"safe-to-evict annotation", `{"podAnnotations": {"cluster-autoscaler.kubernetes.io/safe-to-evict": "true"}}`, ""},
		{"empty placement", ``, ""},
		{"plain placement", testPlacementJSON, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			installCanaryReactor(t, cs, false)
			_, err := newWithClient(context.Background(), cs, testRestConfig(), Config{
				Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", SandboxPlacement: tc.json,
			})
			// The label form of safe-to-evict is allowlisted too (the same single key).
			if tc.wantKey == "" {
				if err != nil {
					t.Fatalf("boot refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("booted, want a refusal naming %q", tc.wantKey)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not name %q", err, tc.wantKey)
			}
			for _, a := range cs.Actions() {
				if a.GetVerb() == "create" {
					t.Errorf("refused placement still created %s: the refusal must come before the canary", a.GetResource().Resource)
				}
			}
		})
	}
}

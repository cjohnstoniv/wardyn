// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// warm_pool_k8s_test.go proves warm pools against a real cluster: the idle placeholder holds nothing, and a run that
// fits the pool takes it and starts on its node while the pool refills. Guarded by WARDYN_TEST_K8S=1, and run under
// both backends (WARDYN_TEST_K8S_AGENT_SANDBOX=1 switches it, as for every k8s.New in this package).

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

const warmConfPool = "conf"

// warmPoolRunner is a driver over the suite's cluster with one pool of one placeholder for the suite's agent image. At
// cleanup it closes the driver, then deletes every warm placeholder and the deny policy.
func warmPoolRunner(t *testing.T) (*asEnv, *orchestrator.Orchestrator) {
	t.Helper()
	if os.Getenv("WARDYN_TEST_K8S") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 not set; skipping the warm pool cases")
	}
	proxy := os.Getenv("WARDYN_PROXY_IMAGE")
	if proxy == "" {
		t.Fatal("WARDYN_PROXY_IMAGE must name the real wardyn-proxy build")
	}
	e := newASEnv(t)
	sub, err := k8s.New(k8sSuiteConfig(k8s.Config{
		Namespaces: []string{e.ns},
		ProxyImage: proxy,
		Record:     true,
		WarmPools:  []k8s.WarmPool{{Name: warmConfPool, Image: e.image, Class: "CC1", CPU: "200m", Memory: "256Mi", Size: 1}},
	}))
	if err != nil {
		t.Fatalf("k8s.New with a warm pool: %v", err)
	}
	r := orchestrator.New(sub)
	t.Cleanup(func() {
		_ = r.Close()
		ctx, cancel := context.WithTimeout(context.Background(), conformance.ASWarmCleanupBound)
		defer cancel()
		_ = e.cs.CoreV1().Pods(e.ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "wardyn.component=warm"})
		_ = e.cs.NetworkingV1().NetworkPolicies(e.ns).Delete(ctx, "wardyn-warm-deny", metav1.DeleteOptions{})
	})
	return e, r
}

// warmPodReady is a pod that is Ready and not being deleted.
func warmPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// waitReadyPlaceholder waits up to limit for a Ready placeholder of the pool whose UID is not in not, and returns it.
func waitReadyPlaceholder(ctx context.Context, t *testing.T, e *asEnv, limit time.Duration, not ...k8stypes.UID) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		list, err := e.cs.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{LabelSelector: "wardyn.warm-pool=" + warmConfPool})
		if err != nil {
			t.Fatalf("list the placeholders: %v", err)
		}
		for i := range list.Items {
			if warmPodReady(&list.Items[i]) && !slices.Contains(not, list.Items[i].UID) {
				return &list.Items[i]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Ready placeholder of pool %s within %s", warmConfPool, limit)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestWarmPoolPlaceholderHoldsNothing(t *testing.T) {
	e, _ := warmPoolRunner(t)
	ctx := context.Background()
	listed := waitReadyPlaceholder(ctx, t, e, conformance.ASWarmReadyWait)
	pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, listed.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-read the placeholder: %v", err)
	}
	s := pod.Spec
	if len(s.InitContainers) != 0 || len(s.EphemeralContainers) != 0 || len(s.Volumes) != 0 {
		t.Errorf("init %d, ephemeral %d, volumes %d; want none", len(s.InitContainers), len(s.EphemeralContainers), len(s.Volumes))
	}
	for _, c := range s.Containers {
		if len(c.Env) != 0 || len(c.EnvFrom) != 0 || len(c.VolumeMounts) != 0 {
			t.Errorf("container %s has env %d, envFrom %d, mounts %d; want none", c.Name, len(c.Env), len(c.EnvFrom), len(c.VolumeMounts))
		}
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken {
		t.Error("the placeholder may mount a service account token")
	}
	if _, ok := pod.Labels["wardyn.run-id"]; ok {
		t.Error("the placeholder carries a run label")
	}
	np, err := e.cs.NetworkingV1().NetworkPolicies(e.ns).Get(ctx, "wardyn-warm-deny", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the deny policy: %v", err)
	}
	if got := np.Spec.PodSelector.MatchLabels; len(got) != 1 || got["wardyn.component"] != "warm" || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("the deny policy selects %+v, want wardyn.component=warm alone", np.Spec.PodSelector)
	}
	if len(np.Spec.PolicyTypes) != 2 || len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 {
		t.Errorf("the deny policy has types %v, %d ingress and %d egress rules; want both types and no rule", np.Spec.PolicyTypes, len(np.Spec.Ingress), len(np.Spec.Egress))
	}
}

func TestWarmPoolPlaceholderClaimedAndRefilled(t *testing.T) {
	e, r := warmPoolRunner(t)
	ctx := context.Background()
	held := waitReadyPlaceholder(ctx, t, e, conformance.ASWarmReadyWait)
	node := held.Spec.NodeName
	runID := uuid.New()
	sb, err := e.startRun(ctx, t, r, runID, func(s *runner.SandboxSpec) {
		s.Resources = runner.Resources{CPUMillis: 200, MemoryMiB: 128}
	})
	if err != nil {
		t.Fatalf("start a run that fits the pool: %v", err)
	}
	list, err := e.cs.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{LabelSelector: "wardyn.warm-pool=" + warmConfPool})
	if err != nil {
		t.Fatalf("list the placeholders: %v", err)
	}
	for i := range list.Items {
		if list.Items[i].UID == held.UID {
			t.Errorf("the placeholder %s of UID %s is still there after the run started", held.Name, held.UID)
		}
	}
	proxyPod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, "wardyn-proxy-"+runID.String(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the proxy pod: %v", err)
	}
	agentPod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the agent pod: %v", err)
	}
	for name, pod := range map[string]*corev1.Pod{"proxy": proxyPod, "agent": agentPod} {
		if !prefersNode(pod, node) {
			t.Errorf("the %s pod has no preferred term naming node %s", name, node)
		}
	}
	if agentPod.Spec.NodeName != node {
		t.Errorf("the agent pod runs on %q, want the placeholder's node %q", agentPod.Spec.NodeName, node)
	}
	if agentPod.Status.Phase != corev1.PodRunning {
		t.Errorf("the agent pod is %s, want Running", agentPod.Status.Phase)
	}
	refilled := waitReadyPlaceholder(ctx, t, e, conformance.ASWarmRefillWait, held.UID)
	if refilled.UID == held.UID {
		t.Error("the pool did not refill with a new placeholder")
	}
}

// prefersNode reports whether pod has a preferred node term on metadata.name naming node.
func prefersNode(pod *corev1.Pod, node string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return false
	}
	for _, term := range pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		for _, f := range term.Preference.MatchFields {
			if f.Key == "metadata.name" && f.Operator == corev1.NodeSelectorOpIn && slices.Equal(f.Values, []string{node}) {
				return true
			}
		}
	}
	return false
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// installProxyIPReactor scripts the proxy pod's Get to report a PodIP
// immediately (no real polling latency in tests) — "wardyn-proxy-" is a
// prefix ONLY the proxy pod name carries among objects routed through a
// "pods" reactor (the Secret/NetworkPolicy names sharing a "wardyn-proxy-"
// stem are different resource kinds entirely).
func installProxyIPReactor(t *testing.T, cs *fake.Clientset, ip string) {
	t.Helper()
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-proxy-") {
			return false, nil, nil
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: action.GetNamespace()},
			Status:     corev1.PodStatus{PodIP: ip},
		}
		return true, pod, nil
	})
}

func testSandboxSpec() runner.SandboxSpec {
	return runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            "wardyn/agent-claude:local",
		ConfinementClass: types.CC1,
		Env:              map[string]string{"HTTP_PROXY": "http://wardyn-proxy:3128"},
		ProxyConfig:      runner.ProxyConfig{ControlPlaneURL: "http://wardynd:8080", RunToken: "tok"},
		Resources:        runner.Resources{CPUMillis: 1000, MemoryMiB: 512, PidsLimit: 128, DiskMiB: 10},
		Labels:           map[string]string{"team": "test"},
	}
}

// TestCreateSandbox_RejectsMounts covers the single preflight chokepoint that
// makes host-mount paths fail closed on k8s: any spec.Mounts entry is
// refused before anything is created.
func TestCreateSandbox_RejectsMounts(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.ClearActions()

	spec := testSandboxSpec()
	spec.Mounts = []runner.Mount{{Source: "/home/op/work", Target: "/work"}}

	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox: want an error rejecting mounts, got nil")
	}
	if !errors.Is(err, errMountsUnsupported) {
		t.Errorf("err = %v, want errors.Is(err, errMountsUnsupported)", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("CreateSandbox rejected mounts but still created %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// TestCreateSandbox_OrderAndRef covers the required creation order — BOTH
// NetworkPolicies before any pod exists, proxy pod before agent pod — and
// that the returned Sandbox.Ref is the agent pod name.
func TestCreateSandbox_OrderAndRef(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	cs.ClearActions()

	spec := testSandboxSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if want := agentPodName(spec.RunID); sb.Ref != want {
		t.Errorf("Ref = %q, want %q", sb.Ref, want)
	}
	if sb.Driver != driverName {
		t.Errorf("Driver = %q, want %q", sb.Driver, driverName)
	}
	if sb.EnforcedClass != types.CC1 {
		t.Errorf("EnforcedClass = %q, want %q", sb.EnforcedClass, types.CC1)
	}

	var createOrder []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" {
			continue
		}
		createOrder = append(createOrder, a.GetResource().Resource)
	}
	want := []string{"secrets", "networkpolicies", "networkpolicies", "pods", "pods"}
	if !equalStrings(createOrder, want) {
		t.Fatalf("create order = %v, want %v", createOrder, want)
	}

	// Both NetworkPolicies exist strictly before either pod: the two
	// "networkpolicies" creates (indices 1,2) precede the two "pods" creates
	// (indices 3,4) — already implied by the exact sequence above, asserted
	// again explicitly since it's the security-critical invariant this test
	// exists to prove.
	netpolIdx, podIdx := -1, -1
	for i, r := range createOrder {
		if r == "networkpolicies" && netpolIdx == -1 {
			netpolIdx = i
		}
		if r == "pods" && podIdx == -1 {
			podIdx = i
		}
	}
	if !(netpolIdx < podIdx) {
		t.Errorf("a NetworkPolicy must be created before any pod: netpolIdx=%d podIdx=%d", netpolIdx, podIdx)
	}

	// Proxy pod before agent pod specifically (by created object name).
	var podNames []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" || a.GetResource().Resource != "pods" {
			continue
		}
		ca := a.(clienttesting.CreateAction)
		podNames = append(podNames, ca.GetObject().(metav1.Object).GetName())
	}
	if len(podNames) != 2 || podNames[0] != proxyPodName(spec.RunID) || podNames[1] != agentPodName(spec.RunID) {
		t.Errorf("pod create order = %v, want [%s, %s]", podNames, proxyPodName(spec.RunID), agentPodName(spec.RunID))
	}
}

// TestCreateSandbox_RollbackOnFailure is table-driven over every ordered
// creation step: an injected failure at step N must roll back everything
// created in steps 1..N-1 and leave nothing dangling.
func TestCreateSandbox_RollbackOnFailure(t *testing.T) {
	tests := []struct {
		name        string
		failVerb    string
		failResFn   func(spec runner.SandboxSpec) (resource, namePrefix string)
		wantCreated int // how many objects existed transiently before the failing step
	}{
		{"secret", "create", func(spec runner.SandboxSpec) (string, string) { return "secrets", secretName(spec.RunID) }, 0},
		{"agent netpol", "create", func(spec runner.SandboxSpec) (string, string) { return "networkpolicies", agentNetPolName(spec.RunID) }, 1},
		{"proxy netpol", "create", func(spec runner.SandboxSpec) (string, string) { return "networkpolicies", proxyNetPolName(spec.RunID) }, 2},
		{"proxy pod", "create", func(spec runner.SandboxSpec) (string, string) { return "pods", proxyPodName(spec.RunID) }, 3},
		{"agent pod", "create", func(spec runner.SandboxSpec) (string, string) { return "pods", agentPodName(spec.RunID) }, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			installProxyIPReactor(t, cs, "10.244.0.7")

			spec := testSandboxSpec()
			resource, name := tt.failResFn(spec)
			cs.PrependReactor(tt.failVerb, resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
				ca, ok := action.(clienttesting.CreateAction)
				if !ok {
					return false, nil, nil
				}
				if ca.GetObject().(metav1.Object).GetName() != name {
					return false, nil, nil
				}
				return true, nil, errors.New("injected failure at " + tt.name)
			})

			_, err := d.CreateSandbox(context.Background(), spec)
			if err == nil {
				t.Fatalf("CreateSandbox: want an error injected at %q, got nil", tt.name)
			}

			assertRunObjectsGone(t, cs, spec.RunID)
		})
	}
}

// TestCreateSandbox_RollbackOnProxyIPTimeout covers the podIP-wait failure
// path specifically: the proxy pod is created, but its IP never resolves
// (here: the reactor errors on every Get, aborting the poll immediately
// rather than exhausting the real 90s timeout) — CreateSandbox must still
// roll back the proxy pod + both netpols + the secret.
func TestCreateSandbox_RollbackOnProxyIPTimeout(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-proxy-") {
			return false, nil, nil
		}
		return true, nil, errors.New("simulated: proxy pod IP never resolves")
	})

	spec := testSandboxSpec()
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox: want an error, got nil")
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

func assertRunObjectsGone(t *testing.T, cs *fake.Clientset, runID uuid.UUID) {
	t.Helper()
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()}
	pods, err := cs.CoreV1().Pods(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("rollback left %d pod(s) behind", len(pods.Items))
	}
	netpols, err := cs.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list network policies: %v", err)
	}
	if len(netpols.Items) != 0 {
		t.Errorf("rollback left %d network polic(ies) behind", len(netpols.Items))
	}
	secrets, err := cs.CoreV1().Secrets(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("rollback left %d secret(s) behind", len(secrets.Items))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
// stem are different resource kinds entirely). Reads the REAL stored pod via
// cs.Tracker() (lock-safe: never the typed clientset, see
// installCanaryReactor's doc for why) and overlays ONLY Status.PodIP, so a
// caller that Gets the same pod again later (e.g. to inspect its Spec) still
// sees everything CreateSandbox actually set — a bare synthesized stub here
// previously left Spec.Containers empty and panicked such a caller.
func installProxyIPReactor(t *testing.T, cs *fake.Clientset, ip string) {
	t.Helper()
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-proxy-") {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, action.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.PodIP = ip
		return true, pod, nil
	})
}

// installAgentRunningReactor scripts the agent pod's Get to report its main
// container Running immediately (no real polling latency in tests) —
// "wardyn-agent-" is a prefix ONLY the agent pod name carries among objects
// routed through a "pods" reactor. Mirrors installProxyIPReactor's shape
// (overlay onto the REAL stored pod via cs.Tracker(), never a bare
// synthesized stub) so a caller that Gets the same pod again later still
// sees everything CreateSandbox actually set.
func installAgentRunningReactor(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-agent-") {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, action.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  mainContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
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
// TestCreateSandbox_TrustedCAPEM proves the WARDYN_TRUSTED_CA_FILE forward
// leg reaches BOTH k8s objects CreateSandbox builds from the spec dispatch
// already staged: the agent pod's env (spec.Env, mutated by
// installSandboxTrustedCA in internal/api BEFORE CreateSandbox is called —
// this test supplies it pre-mutated, matching what dispatch hands the
// driver) via envVars(spec.Env), and the proxy config Secret's JSON payload
// (spec.ProxyConfig.TrustedCAPEM, commit 3) via runner.BuildProxyConfig. No
// k8s-specific code carries this — it rides the SAME Env map and ProxyConfig
// every other sandbox field already does; this test is the proof of that,
// not a new code path.
func TestCreateSandbox_TrustedCAPEM(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Env["WARDYN_MITM_CA_PEM"] = "run-ca-pem\ncorp-ca-pem" // installSandboxTrustedCA's append shape
	spec.ProxyConfig.TrustedCAPEM = "corp-ca-pem"

	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	agentPod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	gotEnv, found := "", false
	for _, e := range agentPod.Spec.Containers[0].Env {
		if e.Name == "WARDYN_MITM_CA_PEM" {
			gotEnv, found = e.Value, true
		}
	}
	if !found || gotEnv != "run-ca-pem\ncorp-ca-pem" {
		t.Errorf("agent pod WARDYN_MITM_CA_PEM (found=%v) = %q, want %q", found, gotEnv, "run-ca-pem\ncorp-ca-pem")
	}

	sec, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy config secret: %v", err)
	}
	var cfg struct {
		TrustedCAPEM string `json:"trusted_ca_pem"`
	}
	if err := json.Unmarshal(sec.Data[proxyConfigSecretKey], &cfg); err != nil {
		t.Fatalf("unmarshal proxy config secret: %v", err)
	}
	if cfg.TrustedCAPEM != "corp-ca-pem" {
		t.Errorf("proxy config secret trusted_ca_pem = %q, want %q", cfg.TrustedCAPEM, "corp-ca-pem")
	}

	// Negative control: an unset knob puts NEITHER the pod env key nor the
	// secret's trusted_ca_pem key in place — byte-identical to a spec that
	// predates this feature.
	spec2 := testSandboxSpec()
	sb2, err := d.CreateSandbox(context.Background(), spec2)
	if err != nil {
		t.Fatalf("CreateSandbox (unset): %v", err)
	}
	agentPod2, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb2.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod (unset): %v", err)
	}
	for _, e := range agentPod2.Spec.Containers[0].Env {
		if e.Name == "WARDYN_MITM_CA_PEM" {
			t.Errorf("agent pod (unset) carries WARDYN_MITM_CA_PEM = %q, want absent", e.Value)
		}
	}
	sec2, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec2.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy config secret (unset): %v", err)
	}
	if bytes.Contains(sec2.Data[proxyConfigSecretKey], []byte("trusted_ca_pem")) {
		t.Errorf("proxy config secret (unset) contains a trusted_ca_pem key, want omitted (omitempty)")
	}
}

func TestCreateSandbox_OrderAndRef(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
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

// TestCreateSandbox_NetworkPolicyFields (M6) reads both run NetworkPolicies
// and the two pod specs back from the fake, checking every field the review
// flagged: agent Ingress==[] (deny all) and egress ONLY to the proxy on
// 3128; proxy ingress from-agent-only, and its DNS rule specifically (not
// just "some rule") carries the metadata-excluding peer (M4); hostAliases,
// automountServiceAccountToken:false, requests==limits, restartPolicy:Never,
// and the H2 agent-vs-default RunAsUser split.
func TestCreateSandbox_NetworkPolicyFields(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	agentNP, err := cs.NetworkingV1().NetworkPolicies(testNamespace).Get(context.Background(), agentNetPolName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent netpol: %v", err)
	}
	if len(agentNP.Spec.Ingress) != 0 {
		t.Errorf("agent netpol Ingress = %v, want empty (deny all)", agentNP.Spec.Ingress)
	}
	if len(agentNP.Spec.Egress) != 1 {
		t.Fatalf("agent netpol Egress = %d rules, want 1", len(agentNP.Spec.Egress))
	}
	eg := agentNP.Spec.Egress[0]
	if len(eg.Ports) != 1 || eg.Ports[0].Port == nil || eg.Ports[0].Port.IntVal != runner.ProxyListenPort {
		t.Errorf("agent netpol egress port = %+v, want %d", eg.Ports, runner.ProxyListenPort)
	}
	if len(eg.To) != 1 || eg.To[0].PodSelector == nil || eg.To[0].PodSelector.MatchLabels[labelComponent] != componentProxy {
		t.Errorf("agent netpol egress peer = %+v, want a podSelector matching the proxy component", eg.To)
	}

	proxyNP, err := cs.NetworkingV1().NetworkPolicies(testNamespace).Get(context.Background(), proxyNetPolName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy netpol: %v", err)
	}
	if len(proxyNP.Spec.Ingress) != 1 || proxyNP.Spec.Ingress[0].From[0].PodSelector.MatchLabels[labelComponent] != componentAgent {
		t.Errorf("proxy netpol ingress = %+v, want from-agent-only", proxyNP.Spec.Ingress)
	}
	for _, r := range proxyNP.Spec.Egress {
		isDNSRule := false
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntVal == 53 {
				isDNSRule = true
			}
		}
		hasMetadataExcept := false
		for _, peer := range r.To {
			if peer.IPBlock == nil {
				continue
			}
			for _, ex := range peer.IPBlock.Except {
				if ex == cloudMetadataAddr {
					hasMetadataExcept = true
				}
			}
		}
		if len(r.To) == 0 {
			t.Errorf("proxy netpol egress rule has no To peer (permits its ports to ALL destinations, including metadata): %+v", r)
		} else if !hasMetadataExcept {
			t.Errorf("proxy netpol egress rule's peer does not exclude the metadata address: %+v", r)
		}
		if isDNSRule && !hasMetadataExcept {
			t.Errorf("M4: the DNS rule specifically must exclude the metadata address: %+v", r)
		}
	}

	agentPod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if len(agentPod.Spec.HostAliases) != 1 || agentPod.Spec.HostAliases[0].Hostnames[0] != "wardyn-proxy" {
		t.Errorf("agent pod HostAliases = %+v, want a wardyn-proxy entry", agentPod.Spec.HostAliases)
	}
	if agentPod.Spec.AutomountServiceAccountToken == nil || *agentPod.Spec.AutomountServiceAccountToken {
		t.Errorf("agent pod AutomountServiceAccountToken = %v, want *false", agentPod.Spec.AutomountServiceAccountToken)
	}
	if agentPod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("agent pod RestartPolicy = %q, want Never", agentPod.Spec.RestartPolicy)
	}
	if agentPod.Spec.DNSPolicy != corev1.DNSNone || agentPod.Spec.DNSConfig == nil || len(agentPod.Spec.DNSConfig.Nameservers) == 0 {
		t.Errorf("agent pod DNSPolicy/DNSConfig = %q / %+v, want None with a loopback nameserver", agentPod.Spec.DNSPolicy, agentPod.Spec.DNSConfig)
	}
	ac := agentPod.Spec.Containers[0]
	if !resourceListsEqual(ac.Resources.Requests, ac.Resources.Limits) {
		t.Errorf("agent container requests %v != limits %v, want equal (hard cap)", ac.Resources.Requests, ac.Resources.Limits)
	}
	if ac.SecurityContext == nil || ac.SecurityContext.RunAsUser == nil || *ac.SecurityContext.RunAsUser != 1000 {
		t.Errorf("agent container RunAsUser = %v, want *1000 (H2)", ac.SecurityContext.RunAsUser)
	}

	proxyPod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy pod: %v", err)
	}
	pc := proxyPod.Spec.Containers[0]
	if pc.SecurityContext == nil || pc.SecurityContext.RunAsUser != nil {
		t.Errorf("proxy container RunAsUser = %v, want nil (H2: image default, distroless nonroot)", pc.SecurityContext.RunAsUser)
	}
}

func resourceListsEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || !v.Equal(bv) {
			return false
		}
	}
	return true
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

// TestCreateSandbox_RollbackOnAgentRunningTimeout covers the agent-pod
// readiness wait's failure path: the pod is created, but its main container
// never reports Running (here: the reactor errors on every Get, aborting the
// poll immediately rather than exhausting the real canaryWaitTimeout) —
// CreateSandbox must still roll back the agent pod itself + the proxy pod +
// both netpols + the secret. Mirrors TestCreateSandbox_RollbackOnProxyIPTimeout.
func TestCreateSandbox_RollbackOnAgentRunningTimeout(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-agent-") {
			return false, nil, nil
		}
		return true, nil, errors.New("simulated: agent pod's main container never starts")
	})

	spec := testSandboxSpec()
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox: want an error, got nil")
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

// TestCreateSandbox_RollbackWaitsForPodsGoneBeforeDroppingNetPols is the
// bug-k8s-1 regression test: CreateSandbox's failure path must share the
// SAME H3 wait-before-netpol-drop guard as StopSandbox/KillSandbox, not a
// hand-rolled fire-and-forget rollback list. A rollback that deletes the
// NetworkPolicies the instant the proxy pod's Delete is ISSUED (not once
// it's actually gone) hands a still-Terminating proxy pod — one that already
// holds this run's live MITM CA key and RunToken — default-allow egress for
// up to its full grace period. Proves the ORDERING via the actual sequence
// of API calls the rollback issues (pod delete-collection, then a pod list —
// the wait-for-gone poll — THEN the netpol delete-collection), mirroring
// TestTeardown_WaitsForPodsGoneBeforeDroppingNetPols in lifecycle_test.go.
func TestCreateSandbox_RollbackWaitsForPodsGoneBeforeDroppingNetPols(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	// Fail the agent pod's readiness wait so rollback fires with BOTH pods
	// and BOTH netpols already created — the maximal-exposure failure point.
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-agent-") {
			return false, nil, nil
		}
		return true, nil, errors.New("simulated: agent pod's main container never starts")
	})

	spec := testSandboxSpec()
	cs.ClearActions()
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox: want an error, got nil")
	}
	assertRunObjectsGone(t, cs, spec.RunID)

	podDeleteIdx, podListIdx, netpolDeleteIdx := -1, -1, -1
	for i, a := range cs.Actions() {
		res := a.GetResource().Resource
		switch {
		case a.GetVerb() == "delete-collection" && res == "pods" && podDeleteIdx == -1:
			podDeleteIdx = i
		case a.GetVerb() == "list" && res == "pods" && podListIdx == -1 && podDeleteIdx != -1:
			podListIdx = i
		case a.GetVerb() == "delete-collection" && res == "networkpolicies" && netpolDeleteIdx == -1:
			netpolDeleteIdx = i
		}
	}
	if podDeleteIdx == -1 || podListIdx == -1 || netpolDeleteIdx == -1 {
		t.Fatalf("missing expected rollback actions: podDelete=%d podList(wait)=%d netpolDelete=%d", podDeleteIdx, podListIdx, netpolDeleteIdx)
	}
	if !(podDeleteIdx < podListIdx && podListIdx < netpolDeleteIdx) {
		t.Errorf("want pod delete-collection < pod list (wait-for-gone) < netpol delete-collection, got indices %d, %d, %d — NetworkPolicies must not drop before rollback proves the pods are actually gone", podDeleteIdx, podListIdx, netpolDeleteIdx)
	}
}

// TestCreateSandbox_AgentContainerTerminated_FailsFast covers M5 (review
// round 2): a container that crashes before ever reaching Running (a bad
// image whose entrypoint exits immediately, CrashLoopBackOff's first cycle,
// ...) must fail FAST, not fall through to "keep polling" and burn the full
// canaryWaitTimeout. Asserted by WALL-CLOCK BOUND, not just "an error
// happened" — a bug that silently reverted to polling would still return an
// error eventually (ctx/test timeout), so only the timing proves the early
// exit actually fired.
func TestCreateSandbox_AgentContainerTerminated_FailsFast(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-agent-") {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, action.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  mainContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "boom"}},
		}}
		return true, pod, nil
	})

	spec := testSandboxSpec()
	start := time.Now()
	_, err := d.CreateSandbox(context.Background(), spec)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("CreateSandbox: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "terminated before ever reaching Running") {
		t.Errorf("err = %v, want it to name the early-exit reason", err)
	}
	// canaryWaitTimeout is 3 minutes; a genuinely fast exit finishes in
	// milliseconds against the fake clientset (k8sPollInterval is 200ms and
	// this fires on the FIRST poll) — 5s is a generous bound that still
	// fails hard if the fix regresses to polling out the full timeout.
	if elapsed > 5*time.Second {
		t.Errorf("CreateSandbox took %s, want a fast exit (< 5s) — Terminated must not fall through to polling out canaryWaitTimeout", elapsed)
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

// TestCreateSandbox_DriveShapesTheAgentPod covers the pod half of the user
// drive: the claim is ensured BEFORE the agent pod exists, and the pod carries
// the volume, the mount at the reserved target, and the fsGroup that makes a
// freshly provisioned block volume writable by uid 1000.
func TestCreateSandbox_DriveShapesTheAgentPod(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	cs.ClearActions()

	spec := testSandboxSpec()
	spec.Drive = testDriveMount()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	// Ordering: the claim must exist before the pod that mounts it, or the pod
	// comes up stuck on a volume the scheduler cannot bind.
	var order []string
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" {
			continue
		}
		if r := a.GetResource().Resource; r == "persistentvolumeclaims" || r == "pods" {
			order = append(order, r)
		}
	}
	if len(order) == 0 || order[0] != "persistentvolumeclaims" {
		t.Errorf("create order = %v, want the claim created before any pod", order)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("agent pod volumes = %v, want exactly the drive", pod.Spec.Volumes)
	}
	vol := pod.Spec.Volumes[0]
	if vol.Name != driveVolumeName || vol.PersistentVolumeClaim == nil {
		t.Fatalf("agent pod volume = %+v, want a %q persistentVolumeClaim volume", vol, driveVolumeName)
	}
	if vol.PersistentVolumeClaim.ClaimName != spec.Drive.ObjectName {
		t.Errorf("claimName = %q, want %q", vol.PersistentVolumeClaim.ClaimName, spec.Drive.ObjectName)
	}
	// PSS Restricted admits persistentVolumeClaim and forbids hostPath; no drive
	// backend on this substrate offers one, and none may ever be added here.
	if vol.HostPath != nil {
		t.Error("agent pod carries a hostPath volume — forbidden by Pod Security Standards Baseline and Restricted alike")
	}
	wantMount := corev1.VolumeMount{Name: driveVolumeName, MountPath: runner.DriveTarget, ReadOnly: false}
	if got := pod.Spec.Containers[0].VolumeMounts; len(got) != 1 || got[0] != wantMount {
		t.Errorf("main container mounts = %+v, want [%+v]", got, wantMount)
	}
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.FSGroup == nil || *sc.FSGroup != driveFSGroup {
		t.Fatalf("pod securityContext = %+v, want FSGroup %d", sc, driveFSGroup)
	}
	if sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy = %v, want OnRootMismatch (an Always recursive chown of a large drive is a minutes-long pod start)", sc.FSGroupChangePolicy)
	}

	// Negative control: a drive-less run's pod is byte-identical to what this
	// substrate produced before drives existed — no volume, no mount, and above
	// all no pod-level security context.
	spec2 := testSandboxSpec()
	sb2, err := d.CreateSandbox(context.Background(), spec2)
	if err != nil {
		t.Fatalf("CreateSandbox (drive-less): %v", err)
	}
	pod2, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb2.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod (drive-less): %v", err)
	}
	if len(pod2.Spec.Volumes) != 0 || len(pod2.Spec.Containers[0].VolumeMounts) != 0 {
		t.Errorf("drive-less pod carries volumes %v / mounts %v, want none", pod2.Spec.Volumes, pod2.Spec.Containers[0].VolumeMounts)
	}
	if pod2.Spec.SecurityContext != nil {
		t.Errorf("drive-less pod securityContext = %+v, want nil (hardening on this substrate is container-level)", pod2.Spec.SecurityContext)
	}
}

// TestCreateSandbox_DriveAsksGvisorForDirectfsOff pins the one pod field that
// exists because of the RUNTIME rather than the storage.
//
// gVisor's directfs donates a file descriptor per mount point to the sandbox
// and operates on it directly, which a network-backed export (the share case)
// does not reliably support. gVisor takes the override per MOUNT, from an
// annotation keyed by the volume's own name, so a drive pod under a runsc
// RuntimeClass carries it and nothing else does. The negative controls are the
// point: a CC1 pod has no RuntimeClass at all, and a Vault-tier microVM handler
// is not runsc — stamping either would be a request to a runtime that never
// reads it.
func TestCreateSandbox_DriveAsksGvisorForDirectfsOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		class   types.ConfinementClass
		handler string
		want    bool
	}{
		{"CC2 gVisor", types.CC2, "runsc", true},
		{"CC3 microVM", types.CC3, "kata-qemu", false},
		{"CC1, no RuntimeClass", types.CC1, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			if tc.handler != "" {
				cfg.ConfinementRuntimes = map[types.ConfinementClass]string{tc.class: "pinned"}
			}
			d, cs := newTestDriver(t, cfg)
			if tc.handler != "" {
				mustCreateRuntimeClass(t, cs, "pinned", tc.handler)
			}
			installProxyIPReactor(t, cs, "10.244.0.11")
			installAgentRunningReactor(t, cs)

			spec := testSandboxSpec()
			spec.ConfinementClass = tc.class
			spec.Drive = testDriveMount()
			sb, err := d.CreateSandbox(context.Background(), spec)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get agent pod: %v", err)
			}
			got, present := pod.Annotations[driveDirectfsAnnotation]
			if present != tc.want {
				t.Fatalf("%s present = %v (annotations %v), want %v", driveDirectfsAnnotation, present, pod.Annotations, tc.want)
			}
			if tc.want && got != "off" {
				t.Errorf("%s = %q, want %q", driveDirectfsAnnotation, got, "off")
			}
		})
	}

	// A drive-LESS gVisor run stamps nothing: the annotation names the drive
	// volume, and a pod with no such mount would carry a dangling request.
	d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "pinned"}})
	mustCreateRuntimeClass(t, cs, "pinned", "runsc")
	installProxyIPReactor(t, cs, "10.244.0.12")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	spec.ConfinementClass = types.CC2
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox (drive-less): %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod (drive-less): %v", err)
	}
	if _, present := pod.Annotations[driveDirectfsAnnotation]; present {
		t.Errorf("drive-less gVisor pod carries %s (%v), want none", driveDirectfsAnnotation, pod.Annotations)
	}
}

// TestCreateSandbox_DriveReadOnlyOnBothHalves pins the two flags that must
// agree: the volume's and the mount's. A read-only allocation that comes up
// writable because only one half carried the flag is a widening.
func TestCreateSandbox_DriveReadOnlyOnBothHalves(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Drive = testDriveMount()
	spec.Drive.ReadOnly = true
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if !pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Error("volume readOnly = false for a read-only allocation")
	}
	if !pod.Spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Error("volumeMount readOnly = false for a read-only allocation")
	}
}

// TestCreateSandbox_DriveSurvivesTeardown is the other half of "no run label":
// the run's whole sweep runs and the person's storage is still there. The
// selector assertion is the load-bearing one — the fake's DeleteCollection
// support does not filter by label, so the proof that the sweep CANNOT reach
// the claim is that the claim carries no key the selector names, and that no
// delete verb is ever issued against a claim at all (the chart grants none).
func TestCreateSandbox_DriveSurvivesTeardown(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Drive = testDriveMount()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	cs.ClearActions()
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	assertRunObjectsGone(t, cs, spec.RunID)

	pvc, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), spec.Drive.ObjectName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the drive's claim did not survive teardown: %v", err)
	}
	selector := labelRun + "=" + spec.RunID.String()
	if _, ok := pvc.Labels[labelRun]; ok {
		t.Errorf("claim labels %v are selectable by teardown's %q", pvc.Labels, selector)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "persistentvolumeclaims" && strings.HasPrefix(a.GetVerb(), "delete") {
			t.Errorf("teardown issued %q against a claim; the chart grants no delete verb — reclaim is an operator command", a.GetVerb())
		}
	}
}

// TestCreateSandbox_DriveRefusalCreatesNothing pins the drive check's place in
// preflight: a share whose claim an admin never provisioned fails the run before
// a Secret, a NetworkPolicy or a pod exists, so there is nothing to roll back.
func TestCreateSandbox_DriveRefusalCreatesNothing(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.ClearActions()

	spec := testSandboxSpec()
	spec.Drive = testDriveMount()
	spec.Drive.Backend = types.DriveBackendK8sPVCStatic

	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, errDriveClaimNotProvisioned) {
		t.Fatalf("err = %v, want errors.Is(err, errDriveClaimNotProvisioned)", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("a refused drive still created %s", a.GetResource().Resource)
		}
	}
}

// TestCreateSandbox_MountsStayRefusedWithADrive proves the two fields never
// merged: the blanket host-bind refusal still fires with a drive present, and it
// fires BEFORE the drive is provisioned.
func TestCreateSandbox_MountsStayRefusedWithADrive(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.ClearActions()

	spec := testSandboxSpec()
	spec.Drive = testDriveMount()
	spec.Mounts = []runner.Mount{{Source: "/home/op/work", Target: "/work"}}

	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, errMountsUnsupported) {
		t.Fatalf("err = %v, want errors.Is(err, errMountsUnsupported)", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("a refused mount still created %s", a.GetResource().Resource)
		}
	}
}

// TestCreateSandbox_SecretEnvRidesTheRunSecret is the F9-H1 fix at the driver
// seam: the two halves of a run's environment are delivered by two DIFFERENT
// mechanisms, and only one of them is readable off the Pod spec.
//
// spec.Env is platform configuration and stays inline (an operator debugging a
// run with `kubectl get pod -o yaml` needs to see it). spec.SecretEnv is
// credential material — resolveEnvSecretGrants' stored-secret values, the
// resident Bedrock keys — and reaches the container as a ValueFrom.SecretKeyRef
// into the per-run Secret, so `pods/get` in the runs namespace yields the
// variable's NAME and nothing else. Both halves must arrive under their own
// names: this is a change of carrier, not of contents.
func TestCreateSandbox_SecretEnvRidesTheRunSecret(t *testing.T) {
	const tokenVal = "ghp_live_stored_secret_9f2c"
	const awsVal = "ASIAWARDYNPROBEKEY42"
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Env["WARDYN_REPO_SLUG"] = "acme/widgets" // non-secret: dispatch's own config
	spec.SecretEnv = map[string]string{"CORP_API_TOKEN": tokenVal, "AWS_ACCESS_KEY_ID": awsVal}

	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	agentPod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	sec, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get run secret: %v", err)
	}

	byName := map[string]corev1.EnvVar{}
	for _, e := range agentPod.Spec.Containers[0].Env {
		byName[e.Name] = e
	}
	// Non-secret half: unchanged, inline, exactly as before the split.
	for _, k := range []string{"HTTP_PROXY", "WARDYN_REPO_SLUG"} {
		e, ok := byName[k]
		if !ok || e.Value != spec.Env[k] || e.ValueFrom != nil {
			t.Errorf("agent pod %s = %+v, want the inline value %q (non-secret env must stay readable in the pod spec)", k, e, spec.Env[k])
		}
	}
	// Credential half: name present, value absent, Secret carries it.
	for k, want := range map[string]string{"CORP_API_TOKEN": tokenVal, "AWS_ACCESS_KEY_ID": awsVal} {
		e, ok := byName[k]
		if !ok {
			t.Errorf("agent pod has no %s env: the credential never reached the sandbox", k)
			continue
		}
		if e.Value != "" {
			t.Errorf("agent pod %s carries an inline Value %q — API-readable via pods/get", k, e.Value)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
			e.ValueFrom.SecretKeyRef.Name != secretName(spec.RunID) || e.ValueFrom.SecretKeyRef.Key != secretEnvDataKey(k) {
			t.Errorf("agent pod %s ValueFrom = %+v, want secretKeyRef{%s/%s}", k, e.ValueFrom, secretName(spec.RunID), secretEnvDataKey(k))
		}
		if string(sec.Data[secretEnvDataKey(k)]) != want {
			t.Errorf("run Secret key %s = %q, want %q", secretEnvDataKey(k), sec.Data[secretEnvDataKey(k)], want)
		}
	}
	// The proxy's own config still shares the Secret, undisturbed, and no
	// credential value appears anywhere in EITHER pod spec.
	if len(sec.Data[proxyConfigSecretKey]) == 0 {
		t.Error("run Secret lost its proxy config: the agent env entries must not displace it")
	}
	proxyPod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy pod: %v", err)
	}
	for _, pod := range []*corev1.Pod{agentPod, proxyPod} {
		for _, c := range pod.Spec.Containers {
			for _, e := range c.Env {
				if strings.Contains(e.Value, tokenVal) || strings.Contains(e.Value, awsVal) {
					t.Errorf("pod %s container %s env %s carries a credential inline: %q", pod.Name, c.Name, e.Name, e.Value)
				}
			}
		}
	}

	// Negative control: a run with no credential env is byte-identical to one
	// that predates the split — no extra Secret keys, no ValueFrom env at all.
	spec2 := testSandboxSpec()
	sb2, err := d.CreateSandbox(context.Background(), spec2)
	if err != nil {
		t.Fatalf("CreateSandbox (no SecretEnv): %v", err)
	}
	agentPod2, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb2.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod (no SecretEnv): %v", err)
	}
	for _, e := range agentPod2.Spec.Containers[0].Env {
		if e.ValueFrom != nil {
			t.Errorf("agent pod env %s has a ValueFrom on a run with no SecretEnv: %+v", e.Name, e.ValueFrom)
		}
	}
	sec2, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec2.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get run secret (no SecretEnv): %v", err)
	}
	if len(sec2.Data) != 1 {
		t.Errorf("run Secret data keys = %v, want only %q on a run with no SecretEnv", sec2.Data, proxyConfigSecretKey)
	}
}

// TestPodStuckReason covers the sentence a pod that never started produces.
//
// The motivating case is the drive one: a claim that never bound leaves the pod
// Pending with NO container status at all, so every check inside
// waitContainerRunning's poll is reading an empty list and the caller used to
// get "context deadline exceeded" and nothing else. The scheduler had been
// saying why for the whole three minutes, in the one place nothing looked.
func TestPodStuckReason(t *testing.T) {
	unbound := "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/3 nodes are available"
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"never observed at all", nil, ""},
		{
			"the claim never bound",
			&corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: "Unschedulable", Message: unbound,
				}},
			}},
			unbound,
		},
		{
			// NOT the cross-node RWO attach case (that pod is SCHEDULED — see
			// podStuckReason's doc): this is the bound PV whose node affinity
			// excludes every candidate, which really does land here.
			"a bound PV whose node affinity excludes every node",
			&corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: "Unschedulable", Message: "node(s) had volume node affinity conflict",
				}},
			}},
			"volume node affinity conflict",
		},
		{
			"scheduled but pending, with nothing else said",
			&corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodPending,
				Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			}},
			"still Pending",
		},
		// The negative control: a Running pod fabricates no cause. A timeout on
		// one of these is about something else entirely and must say so by
		// staying silent.
		{"running", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := podStuckReason(tc.pod)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("podStuckReason = %q, want no fabricated cause", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("podStuckReason = %q, want it to carry %q", got, tc.want)
			}
		})
	}
}

// TestWaitContainerRunning_TimeoutNamesTheUnboundClaim is the other half: the
// enrichment actually reaches the caller, and therefore the run's failure hint.
// The deadline comes from the CALLER's context so the test does not sit through
// canaryWaitTimeout; the code path is identical either way.
func TestWaitContainerRunning_TimeoutNamesTheUnboundClaim(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	unbound := "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "wardyn-agent-stuck", Namespace: testNamespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: "Unschedulable", Message: unbound,
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := d.waitContainerRunning(ctx, pod.Name, mainContainerName)
	if err == nil {
		t.Fatal("waitContainerRunning: want a timeout on a pod that never starts")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to still wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), unbound) {
		t.Errorf("err = %q, want the scheduler's own reason — a bare deadline names nothing an operator can act on", err)
	}
}

// res2-01: podStuckReason's doc comment claimed the cross-node ReadWriteOnce
// case reaches PodScheduled. It does not — nothing schedules around RWO
// (kube-scheduler enforces only ReadWriteOncePod), so that pod IS scheduled and
// then stalls in ContainerCreating with a FailedAttachVolume warning EVENT,
// which this function never reads. docs/OPERATIONS.md ("User drives on
// Kubernetes") carries the corrected account; the comment must not contradict
// it, or an operator is sent to read a condition that says nothing about their
// failure. The behaviour below is the control: it is already right.
func TestPodStuckReasonCommentDoesNotClaimTheCrossNodeRWOCase(t *testing.T) {
	doc := docCommentAbove(t, "sandbox.go", "func podStuckReason(")
	if strings.Contains(doc, "already attached to a pod on another node") {
		t.Errorf("sandbox.go podStuckReason still claims a cross-node ReadWriteOnce attach reaches PodScheduled; that pod is SCHEDULED (PodScheduled=True) and stalls on a FailedAttachVolume event this function never reads:\n%s", doc)
	}
	if !strings.Contains(doc, "FailedAttachVolume") {
		t.Errorf("sandbox.go podStuckReason does not name what the cross-node ReadWriteOnce case actually produces (a FailedAttachVolume event), so the next reader has nowhere to go:\n%s", doc)
	}

	// Control: a pod in exactly that state — scheduled, Pending, waiting on the
	// attach — really does produce no storage cause here.
	pod := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodPending,
		Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  mainContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}},
	}}
	if got := podStuckReason(pod); strings.Contains(got, "unscheduled") {
		t.Errorf("podStuckReason(cross-node RWO shape) = %q, want no unscheduled claim — that pod IS scheduled", got)
	}
}

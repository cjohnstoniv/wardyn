// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// TestNewWithClient_CanaryEnforced covers the state machine's PASS path:
// phase A connects (no policy), phase B is blocked (deny-all in effect) —
// the substrate boots and advertises NetworkPolicy=true.
func TestNewWithClient_CanaryEnforced(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)

	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test"})
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	if !d.netPolEnforced {
		t.Errorf("netPolEnforced = false, want true")
	}
	if d.netPolOptedOut {
		t.Errorf("netPolOptedOut = true, want false")
	}
	assertCanaryCleanedUp(t, cs)
}

// TestNewWithClient_CanaryUnenforced_RefusesBoot covers the FAIL path: phase
// B connects DESPITE the deny-all policy (the CNI does not enforce
// NetworkPolicy) and AllowUnenforcedNetPol is NOT set — construction must
// refuse (fail closed, never boot silently unconfined).
func TestNewWithClient_CanaryUnenforced_RefusesBoot(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, true)

	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test"})
	if err == nil {
		t.Fatal("newWithClient: want an error refusing boot, got nil")
	}
	// And NO driver alongside the error: a caller that logged the error and
	// carried on must not find a usable substrate in its place.
	if d != nil {
		t.Errorf("driver = %v on a refusal, want nil", d)
	}
	if !errors.Is(err, errNetworkPolicyUnenforced) {
		t.Errorf("err = %v, want errors.Is(err, errNetworkPolicyUnenforced)", err)
	}
	// The refusal path sweeps its canary too — the PASS and opt-out paths are
	// not the only ones that must not leave a pod and a policy behind.
	assertCanaryCleanedUp(t, cs)
}

// TestNewWithClient_CanaryUnenforced_OptOut covers the documented escape
// hatch: the SAME unenforced verdict, but AllowUnenforcedNetPol=true
// downgrades it to construction succeeding — with the substrate STILL
// reporting NetworkPolicy=false and StructuralEgress=false (the A0 contract
// correction: an opted-out substrate must never read as confined).
func TestNewWithClient_CanaryUnenforced_OptOut(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, true)

	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", AllowUnenforcedNetPol: true})
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	if d.netPolEnforced {
		t.Errorf("netPolEnforced = true, want false (opted out, never reads as confined)")
	}
	if !d.netPolOptedOut {
		t.Errorf("netPolOptedOut = false, want true")
	}
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if cls.NetworkPolicy {
		t.Errorf("ClassSupport.NetworkPolicy = true, want false even under opt-out")
	}
	if cls.StructuralEgress {
		t.Errorf("ClassSupport.StructuralEgress = true, want false (this substrate never claims L0)")
	}
	// Nor ACKNOWLEDGED: the opt-out is an operator accepting an unenforced CNI,
	// which is the opposite of a substrate whose NetworkPolicy support has been
	// confirmed — a console reading this field must not show either as the other.
	if cls.NetworkPolicyAcknowledged {
		t.Errorf("ClassSupport.NetworkPolicyAcknowledged = true, want false under the opt-out")
	}
	assertCanaryCleanedUp(t, cs)
}

// TestNewWithClient_CanaryIndeterminate_RefusesBoot covers the third state:
// a non-network failure (here, phase A's pod stuck ImagePullBackOff) is
// indeterminate, not a "not enforced" verdict — construction refuses
// regardless of AllowUnenforcedNetPol (the opt-out only concerns a PROVEN
// unenforced policy, never a canary that couldn't run at all).
func TestNewWithClient_CanaryIndeterminate_RefusesBoot(t *testing.T) {
	cs := fake.NewClientset()
	installStuckCanaryReactor(t, cs)

	_, err := newWithClient(context.Background(), cs, testRestConfig(), Config{Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", AllowUnenforcedNetPol: true})
	if err == nil {
		t.Fatal("newWithClient: want an error refusing boot, got nil")
	}
	if !errors.Is(err, errCanaryIndeterminate) {
		t.Errorf("err = %v, want errors.Is(err, errCanaryIndeterminate)", err)
	}
}

// TestNewWithClient_CanaryPhaseBUnexpectedExitCode_Indeterminate is the M1
// regression test: the -egress-canary flag only ever exits 0 or 1 (see
// cmd/wardyn-proxy/main.go); any OTHER exit code (128, a StartError shape,
// is used here) must never be read as "enforced" or "unenforced" — it is
// indeterminate, same as any other non-network failure.
func TestNewWithClient_CanaryPhaseBUnexpectedExitCode_Indeterminate(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactorExitCodes(t, cs, 0, 128)

	_, err := newWithClient(context.Background(), cs, testRestConfig(), Config{Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", AllowUnenforcedNetPol: true})
	if err == nil {
		t.Fatal("newWithClient: want an error refusing boot, got nil")
	}
	if !errors.Is(err, errCanaryIndeterminate) {
		t.Errorf("err = %v, want errors.Is(err, errCanaryIndeterminate) — an exit code other than 0/1 must never resolve to a verdict", err)
	}
}

// TestNewWithClient_CanaryNetPolScopedToSuffix is the M2 regression test:
// the deny-all netpol's PodSelector (and the canary pod's own labels) must
// carry a per-invocation unique label (labelRun, reusing that key with the
// canary's own suffix as the value), so two wardynd instances booting
// concurrently in the same namespace can't have instance A's phase-B
// deny-all netpol also match instance B's phase-A pod.
func TestNewWithClient_CanaryNetPolScopedToSuffix(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	cs.ClearActions()

	if _, err := newWithClient(context.Background(), cs, testRestConfig(), Config{Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test"}); err != nil {
		t.Fatalf("newWithClient: %v", err)
	}

	var netpol *networkingv1.NetworkPolicy
	var pods []*corev1.Pod
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" {
			continue
		}
		switch a.GetResource().Resource {
		case "networkpolicies":
			netpol = a.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		case "pods":
			pods = append(pods, a.(clienttesting.CreateAction).GetObject().(*corev1.Pod))
		}
	}
	if netpol == nil {
		t.Fatal("no NetworkPolicy was created during the canary")
	}
	suffix := netpol.Spec.PodSelector.MatchLabels[labelRun]
	if suffix == "" {
		t.Fatalf("canary deny-all netpol selector missing %s — would cross-match another concurrent instance's canary pod: %v", labelRun, netpol.Spec.PodSelector.MatchLabels)
	}
	if netpol.Labels[labelRun] != suffix {
		t.Errorf("canary netpol's OWN labels[%s] = %q, want it to match its own selector value %q", labelRun, netpol.Labels[labelRun], suffix)
	}
	// The phase B pod (the one this netpol is meant to select) must carry
	// the SAME suffix value, so the selector actually matches it.
	matched := false
	for _, p := range pods {
		if p.Labels[labelRun] == suffix {
			matched = true
		}
	}
	if !matched {
		t.Errorf("no created canary pod carries labels[%s]=%q (the netpol's own selector value) — the selector would match nothing", labelRun, suffix)
	}
}

// TestNewWithClient_AckAmbientDefaultDeny_Boots is B1's PASS path: phase A
// reaches Running and exits exactly 1 — the shape an ambient (platform-
// applied) default-deny NetworkPolicy produces — and AckAmbientDefaultDeny
// is set. Construction must succeed, report acked (never enforced, which
// would overclaim proof phase B never actually produced), and phase B must
// never run at all: exactly ONE canary pod created.
func TestNewWithClient_AckAmbientDefaultDeny_Boots(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactorExitCodes(t, cs, 1, 0) // phase A itself exits 1
	cs.ClearActions()

	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{
		Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", AckAmbientDefaultDeny: true,
	})
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	if !d.netPolAcked {
		t.Error("netPolAcked = false, want true")
	}
	if d.netPolEnforced {
		t.Error("netPolEnforced = true, want false — acknowledged is not proof, phase B never ran")
	}
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !cls.NetworkPolicyAcknowledged {
		t.Error("ClassSupport.NetworkPolicyAcknowledged = false, want true")
	}
	if cls.NetworkPolicy {
		t.Error("ClassSupport.NetworkPolicy = true, want false — acknowledged must never read as proven")
	}

	podsCreated := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
			podsCreated++
		}
	}
	if podsCreated != 1 {
		t.Errorf("canary pods created = %d, want exactly 1 (phase B must be skipped when the ack applies)", podsCreated)
	}
	assertCanaryCleanedUp(t, cs)
}

// TestNewWithClient_AckAmbientDefaultDeny_StuckPodStaysIndeterminate proves
// the ack is scoped EXACTLY to "reached Running, exited 1" — a pod that never
// reaches Running at all (ImagePullBackOff, same fixture as the plain
// indeterminate test) must stay indeterminate and refuse boot regardless of
// AckAmbientDefaultDeny; there is nothing here that shape could plausibly
// acknowledge.
func TestNewWithClient_AckAmbientDefaultDeny_StuckPodStaysIndeterminate(t *testing.T) {
	cs := fake.NewClientset()
	installStuckCanaryReactor(t, cs)

	_, err := newWithClient(context.Background(), cs, testRestConfig(), Config{
		Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", AckAmbientDefaultDeny: true,
	})
	if err == nil {
		t.Fatal("newWithClient: want an error refusing boot, got nil")
	}
	if !errors.Is(err, errCanaryIndeterminate) {
		t.Errorf("err = %v, want errors.Is(err, errCanaryIndeterminate) — a stuck pod is not the ack's shape", err)
	}
}

// TestNewWithClient_AckAmbientDefaultDeny_HealthyPhaseAIsUnaffected proves the
// ack never suppresses a REAL two-phase test when phase A actually succeeds:
// the normal enforced/unenforced verdict must come out exactly as if the ack
// were never set.
func TestNewWithClient_AckAmbientDefaultDeny_HealthyPhaseAIsUnaffected(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false) // phase A exit 0, phase B exit 1 (enforced)

	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{
		Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test", AckAmbientDefaultDeny: true,
	})
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	if !d.netPolEnforced {
		t.Error("netPolEnforced = false, want true — a healthy phase A must run the real two-phase test")
	}
	if d.netPolAcked {
		t.Error("netPolAcked = true, want false — nothing was acknowledged, phase A never failed")
	}
}

// TestNewWithClient_NoAck_ExitOneNamesTheEnv is the negative: without the ack
// set, the SAME phase-A-exits-1 shape stays indeterminate, and its error
// names the env var so an operator hitting this on a real managed cluster
// knows the override exists.
func TestNewWithClient_NoAck_ExitOneNamesTheEnv(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactorExitCodes(t, cs, 1, 0)

	_, err := newWithClient(context.Background(), cs, testRestConfig(), Config{
		Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test",
	})
	if err == nil {
		t.Fatal("newWithClient: want an error refusing boot, got nil")
	}
	if !errors.Is(err, errCanaryIndeterminate) {
		t.Errorf("err = %v, want errors.Is(err, errCanaryIndeterminate)", err)
	}
	if !strings.Contains(err.Error(), "WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY") {
		t.Errorf("err = %q, want it to name WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY", err.Error())
	}
}

// assertCanaryCleanedUp checks that no wardyn.component=canary pod or
// NetworkPolicy is left behind after construction — "clean up canary pods +
// the deny-all netpol in all paths" (the A1 contract).
func assertCanaryCleanedUp(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	listOpts := metav1.ListOptions{LabelSelector: labelComponent + "=" + componentCanary}
	pods, err := cs.CoreV1().Pods(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("canary pods left behind: %d", len(pods.Items))
	}
	netpols, err := cs.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), listOpts)
	if err != nil {
		t.Fatalf("list network policies: %v", err)
	}
	if len(netpols.Items) != 0 {
		t.Errorf("canary network policies left behind: %d", len(netpols.Items))
	}
}

// TestApiserverHostPort_ParsesSchemeHost is a small unit check that the
// canary's dial target is derived from rest.Config.Host without DNS/egress —
// a bare host:port and a scheme+host both resolve; a missing port defaults
// to 443.
func TestApiserverHostPort_ParsesSchemeHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"https://10.96.0.1:443", "10.96.0.1:443"},
		{"https://10.96.0.1", "10.96.0.1:443"},
		{"http://apiserver.internal:6443", "apiserver.internal:6443"},
	}
	for _, tc := range cases {
		got, err := apiserverHostPort(&rest.Config{Host: tc.host})
		if err != nil {
			t.Fatalf("apiserverHostPort(%q): %v", tc.host, err)
		}
		if got != tc.want {
			t.Errorf("apiserverHostPort(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

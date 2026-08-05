// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
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

	_, err := newWithClient(context.Background(), cs, testRestConfig(), Config{Namespace: testNamespace, ProxyImage: "wardyn/wardyn-proxy:test"})
	if err == nil {
		t.Fatal("newWithClient: want an error refusing boot, got nil")
	}
	if !errors.Is(err, errNetworkPolicyUnenforced) {
		t.Errorf("err = %v, want errors.Is(err, errNetworkPolicyUnenforced)", err)
	}
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

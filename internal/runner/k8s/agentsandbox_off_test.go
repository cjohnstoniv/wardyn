// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// The agent-sandbox backend off (the default) builds no dynamic client, asks the cluster nothing
// about Sandboxes, and leaves the boot it always had.
func TestAgentSandboxOff_BuildsNoDynamicClientAndAsksNothing(t *testing.T) {
	prev := newDynamicClient
	newDynamicClient = func(*rest.Config) (dynamic.Interface, error) {
		t.Error("a dynamic client was built with the backend off")
		return nil, nil
	}
	t.Cleanup(func() { newDynamicClient = prev })
	d, cs := newTestDriver(t, Config{})
	if d.dynamic != nil {
		t.Error("off driver holds a dynamic client")
	}
	for _, a := range cs.Actions() {
		switch r := a.GetResource(); {
		case r.Resource == "resource": // FakeDiscovery's record of a discovery call
			t.Errorf("off boot made a discovery call: %s", a.GetVerb())
		case r.Resource == "selfsubjectaccessreviews":
			t.Error("off boot reviewed permissions")
		case r.Group == agentSandboxGroup:
			t.Errorf("off boot touched %s", r.Resource)
		}
	}
}

// The NetworkPolicy overrides keep their meaning with the backend off: an unenforced canary under
// WARDYN_K8S_ALLOW_UNENFORCED_NETPOL still boots, as before.
func TestAgentSandboxOff_OverridesStillBoot(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, true)
	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{AllowUnenforcedNetPol: true, Namespaces: []string{testNamespace}, ProxyImage: "p"})
	if err != nil {
		t.Fatalf("off boot with the override: %v", err)
	}
	if d.netPolEnforced || d.dynamic != nil {
		t.Errorf("netPolEnforced=%v dynamic=%v, want false and nil", d.netPolEnforced, d.dynamic)
	}
}

// Off, create and teardown issue the same writes in the same order as before the backend existed:
// both NetworkPolicies, the Secret, the proxy pod, the agent pod; then pods, Secrets, NetworkPolicies.
// No Sandbox, no pod patch, and the agent pod carries no gate.
func TestAgentSandboxOff_CreateAndTeardownWritesAreUnchanged(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	cs.ClearActions()
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSandboxSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	var writes []string
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
			continue
		}
		writes = append(writes, a.GetVerb()+" "+a.GetResource().Resource)
		if ca, ok := a.(clienttesting.CreateAction); ok {
			if pod, ok := ca.GetObject().(*corev1.Pod); ok && pod.Name == sb.Ref && len(pod.Spec.SchedulingGates) != 0 {
				t.Errorf("off agent pod carries scheduling gates %+v", pod.Spec.SchedulingGates)
			}
		}
	}
	want := []string{
		"create networkpolicies", "create networkpolicies", "create secrets", "create pods", "create pods",
		"delete-collection pods", "delete-collection secrets", "delete-collection networkpolicies",
	}
	if !slices.Equal(writes, want) {
		t.Errorf("off writes = %v\nwant %v", writes, want)
	}
}

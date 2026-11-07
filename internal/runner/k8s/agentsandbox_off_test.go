// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
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

// Off, create, status and teardown issue the same writes in the same order as before the backend
// existed: both NetworkPolicies, the Secret, the proxy pod, the agent pod; nothing for a status read;
// then pods, Secrets, NetworkPolicies. No Sandbox, no pod patch, and the agent pod carries no gate.
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
	if _, err := d.Status(ctx, sb.Ref); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := d.AgentStatus(ctx, sb.Ref, execContainerName); err != nil {
		t.Fatalf("AgentStatus: %v", err)
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

// Off, a pod that the backend would call replaced (gated, its selected label gone, its image changed)
// reads as it always did: Status by its phase, Wait as an agent never exec'd, and Exec goes ahead.
// With no dynamic client, a check taken here would panic.
func TestAgentSandboxOff_ReplacedPodChecksAreNotTaken(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	runID := uuid.New()
	ref := createAgentPodFixture(t, cs, runID, "wardyn/agent:test", nil)
	ctx := context.Background()
	pod, err := cs.CoreV1().Pods(testNamespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: agentSandboxGate}}
	pod.Spec.Containers[0].Image = "registry.example/other:1"
	delete(pod.Labels, labelComponent)
	pod.Status.Phase = corev1.PodPending
	if _, err := cs.CoreV1().Pods(testNamespace).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	st, err := d.Status(ctx, ref)
	if err != nil || st.State != types.RunStarting || st.Message == agentPodReplacedMessage || st.FailureHint != "" {
		t.Errorf("off Status = %+v, %v; want STARTING by the pod's phase", st, err)
	}
	if st, err := d.AgentStatus(ctx, ref, execContainerName); err != nil || st.State != types.RunStopped || st.Message != "agent exec not found" {
		t.Errorf("off AgentStatus = %+v, %v; want STOPPED \"agent exec not found\"", st, err)
	}
	if _, err := d.Wait(ctx, ref); err == nil || !strings.Contains(err.Error(), "no agent exec tracked") {
		t.Errorf("off Wait err = %v, want \"no agent exec tracked\"", err)
	}
	if _, err := d.Exec(ctx, ref, []string{"agent-run", "task"}); err != nil {
		t.Errorf("off Exec on a gated pod: %v, want it to go ahead", err)
	}
}

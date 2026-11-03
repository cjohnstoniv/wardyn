// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// asAdmitted is an on Driver with asSpec's run created, its pod admitted and ungated.
func asAdmitted(t *testing.T) (*Driver, *fake.Clientset, *dynamicfake.FakeDynamicClient, string) {
	t.Helper()
	d, cs, dyn := newSandboxRig(t, Config{}, controllerMaking(t, nil))
	sb, err := d.CreateSandbox(context.Background(), asSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return d, cs, dyn, sb.Ref
}

// changeAgentPod rewrites the stored agent pod, as something outside Wardyn would.
func changeAgentPod(t *testing.T, cs *fake.Clientset, ref string, mutate func(*corev1.Pod)) {
	t.Helper()
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, ref)
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	pod := obj.(*corev1.Pod).DeepCopy()
	mutate(pod)
	if err := cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatalf("update agent pod: %v", err)
	}
}

// writesInOrder records every write either fake receives, in the order they arrive.
func writesInOrder(cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient) *[]string {
	var order []string
	record := func(a clienttesting.Action) (bool, runtime.Object, error) {
		switch a.GetVerb() {
		case "get", "list", "watch":
		default:
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource)
		}
		return false, nil, nil
	}
	cs.PrependReactor("*", "*", record)
	dyn.PrependReactor("*", "*", record)
	return &order
}

// Kill deletes the run's Sandbox, under its UID, and sees it gone before deleting any pod: the
// controller would otherwise make the deleted pod again.
func TestAgentSandboxLifecycle_TeardownDeletesTheSandboxBeforeThePods(t *testing.T) {
	d, cs, dyn, ref := asAdmitted(t)
	order := writesInOrder(cs, dyn)
	dyn.ClearActions()
	if err := d.KillSandbox(context.Background(), ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	want := []string{"delete sandboxes", "delete-collection pods", "delete-collection secrets", "delete-collection networkpolicies"}
	if !slices.Equal(*order, want) {
		t.Errorf("teardown writes = %v\nwant %v", *order, want)
	}
	var verbs []string
	for _, a := range dyn.Actions() {
		verbs = append(verbs, a.GetVerb())
		if da, ok := a.(clienttesting.DeleteAction); ok {
			if p := da.GetDeleteOptions().Preconditions; p == nil || p.UID == nil || *p.UID != asSandboxUID {
				t.Errorf("Sandbox delete preconditions = %+v, want UID %s", p, asSandboxUID)
			}
		}
	}
	if wantVerbs := []string{"get", "delete", "get"}; !slices.Equal(verbs, wantVerbs) {
		t.Errorf("Sandbox calls = %v, want %v: read, delete, then see it gone", verbs, wantVerbs)
	}
	if _, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, ref); !isNotFound(err) {
		t.Errorf("the run's Sandbox is still there (get err %v)", err)
	}
	assertRunObjectsGone(t, cs, asRunID)
}

// A Sandbox that cannot be deleted fails the teardown before any pod is deleted, so the run's
// NetworkPolicies, Secret and pods all stay: a retry finds them.
func TestAgentSandboxLifecycle_SandboxDeleteErrorKeepsTheNetworkPolicies(t *testing.T) {
	d, cs, dyn, ref := asAdmitted(t)
	dyn.PrependReactor("delete", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("sandbox delete refused")
	})
	order := writesInOrder(cs, dyn)
	err := d.KillSandbox(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "delete agent sandbox") || !strings.Contains(err.Error(), "sandbox delete refused") {
		t.Fatalf("KillSandbox err = %v, want the Sandbox delete's error", err)
	}
	if want := []string{"delete sandboxes"}; !slices.Equal(*order, want) {
		t.Errorf("writes = %v, want only %v", *order, want)
	}
	ctx := context.Background()
	listOpts := metav1.ListOptions{LabelSelector: labelRun + "=" + asRunID.String()}
	netpols, err := cs.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, listOpts)
	if err != nil || len(netpols.Items) != 2 {
		t.Errorf("NetworkPolicies after a failed Sandbox delete = %d (err %v), want both kept", len(netpols.Items), err)
	}
	secrets, err := cs.CoreV1().Secrets(testNamespace).List(ctx, listOpts)
	if err != nil || len(secrets.Items) != 1 {
		t.Errorf("Secrets after a failed Sandbox delete = %d (err %v), want it kept", len(secrets.Items), err)
	}
}

// replacedCases are the ways a pod stops being the one Wardyn admitted.
var replacedCases = []struct {
	name   string
	mutate func(*corev1.Pod)
}{
	{"made again by the controller, gated", func(p *corev1.Pod) {
		p.UID, p.Spec.SchedulingGates = "agent-pod-uid-2", []corev1.PodSchedulingGate{{Name: agentSandboxGate}}
	}},
	{"selected label missing", func(p *corev1.Pod) { delete(p.Labels, labelComponent) }},
	{"selected label changed", func(p *corev1.Pod) { p.Labels[labelRun] = uuid.NewString() }},
	{"container image changed", func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.example/other:1" }},
	{"init container image changed", func(p *corev1.Pod) { p.Spec.InitContainers[0].Image = "registry.example/other:1" }},
}

func TestAgentSandboxLifecycle_ReplacedPodReadsFailed(t *testing.T) {
	for _, tc := range replacedCases {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, _, ref := asAdmitted(t)
			ctx := context.Background()
			if st, err := d.Status(ctx, ref); err != nil || st.State != types.RunRunning {
				t.Fatalf("admitted pod Status = %+v, %v; want RUNNING before the change", st, err)
			}
			changeAgentPod(t, cs, ref, func(p *corev1.Pod) {
				if len(p.Spec.InitContainers) == 0 {
					t.Fatal("asSpec's pod has no init container to change")
				}
				tc.mutate(p)
			})
			for _, read := range []struct {
				name string
				get  func() (string, types.RunState, error)
			}{
				{"Status", func() (string, types.RunState, error) {
					st, err := d.Status(ctx, ref)
					return st.Message, st.State, err
				}},
				{"AgentStatus", func() (string, types.RunState, error) {
					st, err := d.AgentStatus(ctx, ref, execContainerName)
					return st.Message, st.State, err
				}},
			} {
				msg, state, err := read.get()
				if err != nil || state != types.RunFailed || msg != agentPodReplacedMessage {
					t.Errorf("%s = %s %q, %v; want FAILED with the replaced-pod sentence", read.name, state, msg, err)
				}
			}
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if _, err := d.Wait(wctx, ref); err == nil || !strings.Contains(err.Error(), agentPodReplacedMessage) {
				t.Errorf("Wait err = %v, want the replaced-pod sentence", err)
			}
		})
	}
}

// Exec refuses a gated pod before adding the agent's container to it.
func TestAgentSandboxLifecycle_ExecRefusesAGatedPod(t *testing.T) {
	d, cs, _, ref := asAdmitted(t)
	changeAgentPod(t, cs, ref, replacedCases[0].mutate)
	cs.ClearActions()
	_, err := d.Exec(context.Background(), ref, []string{"agent-run", "task"})
	if err == nil || !strings.Contains(err.Error(), agentPodReplacedMessage) {
		t.Fatalf("Exec err = %v, want the replaced-pod sentence", err)
	}
	for _, a := range cs.Actions() {
		if a.GetSubresource() == "ephemeralcontainers" {
			t.Errorf("Exec wrote %s %s/%s to a gated pod", a.GetVerb(), a.GetResource().Resource, a.GetSubresource())
		}
	}
}

// A pod whose Sandbox is already gone (Wardyn's own teardown deletes it first) has no images to be
// compared with; it reads by its phase until it is collected.
func TestAgentSandboxLifecycle_PodWithoutItsSandboxReadsByPhase(t *testing.T) {
	d, _, dyn, ref := asAdmitted(t)
	if err := dyn.Tracker().Delete(sandboxesGVR, testNamespace, ref); err != nil {
		t.Fatalf("delete Sandbox: %v", err)
	}
	if st, err := d.Status(context.Background(), ref); err != nil || st.State != types.RunRunning {
		t.Errorf("Status = %+v, %v; want RUNNING", st, err)
	}
}

// The orphan sweep finds a run by its Sandbox alone, with no NetworkPolicy or pod left to name it.
func TestAgentSandboxLifecycle_SweepFindsALoneSandbox(t *testing.T) {
	d, _, dyn := newSandboxRig(t, Config{}, controllerMaking(t, nil))
	runID := uuid.New()
	lone := &unstructured.Unstructured{Object: map[string]any{"apiVersion": agentSandboxAPIVersion, "kind": "Sandbox"}}
	lone.SetName(agentPodName(runID))
	lone.SetNamespace(testNamespace)
	lone.SetUID("lone-sandbox-uid")
	lone.SetLabels(wardynLabels(runID, componentAgent, nil))
	lone.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour)))
	if err := dyn.Tracker().Create(sandboxesGVR, lone, testNamespace); err != nil {
		t.Fatalf("seed Sandbox: %v", err)
	}
	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
	if err != nil || swept != 1 {
		t.Fatalf("SweepOrphanedSandboxes = %d, %v; want 1 run swept", swept, err)
	}
	if _, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, lone.GetName()); !isNotFound(err) {
		t.Errorf("the lone Sandbox is still there (get err %v)", err)
	}
	for _, a := range dyn.Actions() {
		if la, ok := a.(clienttesting.ListAction); ok {
			if sel := la.GetListRestrictions().Labels.String(); sel != labelManaged+"=true" {
				t.Errorf("Sandbox list selector = %q, want %s=true", sel, labelManaged)
			}
		}
	}
}

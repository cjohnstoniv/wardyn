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
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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
			if st, err := d.Status(ctx, ref); err != nil || st.State != types.RunRunning || st.FailureHint != "" {
				t.Fatalf("admitted pod Status = %+v, %v; want RUNNING with no failure hint before the change", st, err)
			}
			changeAgentPod(t, cs, ref, func(p *corev1.Pod) {
				if len(p.Spec.InitContainers) == 0 {
					t.Fatal("asSpec's pod has no init container to change")
				}
				tc.mutate(p)
			})
			for _, read := range []struct {
				name string
				get  func() (runner.Status, error)
			}{
				{"Status", func() (runner.Status, error) { return d.Status(ctx, ref) }},
				{"AgentStatus", func() (runner.Status, error) { return d.AgentStatus(ctx, ref, execContainerName) }},
			} {
				st, err := read.get()
				if err != nil || st.State != types.RunFailed || st.Message != agentPodReplacedMessage {
					t.Errorf("%s = %s %q, %v; want FAILED with the replaced-pod sentence", read.name, st.State, st.Message, err)
				}
				// The run page shows a run's failure hint, never its status message.
				if st.FailureHint != agentPodReplacedMessage {
					t.Errorf("%s FailureHint = %q, want the replaced-pod sentence for the run page", read.name, st.FailureHint)
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

// A running pod whose Sandbox is gone (deleted with the orphan policy, or not yet collected) has no
// template to be compared with, so it reads ended, image changed or not. Wait reads it as a gone pod:
// Wardyn's own teardown ends the run before it deletes the Sandbox, so that end is the only one.
func TestAgentSandboxLifecycle_PodWithoutItsSandboxReadsFailed(t *testing.T) {
	for _, changed := range []bool{false, true} {
		d, cs, dyn, ref := asAdmitted(t)
		changeAgentPod(t, cs, ref, func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			if changed {
				p.Spec.Containers[0].Image = "registry.example/other:1"
			}
		})
		if err := dyn.Tracker().Delete(sandboxesGVR, testNamespace, ref); err != nil {
			t.Fatalf("delete Sandbox: %v", err)
		}
		ctx := context.Background()
		for name, read := range map[string]func() (runner.Status, error){
			"Status":      func() (runner.Status, error) { return d.Status(ctx, ref) },
			"AgentStatus": func() (runner.Status, error) { return d.AgentStatus(ctx, ref, execContainerName) },
		} {
			if st, err := read(); err != nil || st.State != types.RunFailed || st.FailureHint != agentPodReplacedMessage {
				t.Errorf("image changed %v: %s = %+v, %v; want FAILED with the replaced-pod sentence", changed, name, st, err)
			}
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if code, err := d.Wait(wctx, ref); err != nil || code != notFoundExitCode {
			t.Errorf("image changed %v: Wait = %d, %v; want %d, nil, as for a gone pod", changed, code, err, notFoundExitCode)
		}
		cancel()
	}
}

// An image swapped in the Sandbox template changes the pod and the template alike, so only the
// Sandbox's generation shows it: the API server sets 1 on create, and Wardyn never changes a Sandbox.
// The fake client sets none, which reads as unchanged; generation 1 is the real API's created Sandbox.
// A Sandbox being deleted reads ended whatever its images: a finalizer can hold it with its pod
// running, setting its deletion time adds one to its generation, and its pod is going with it. Wait
// reads it as a gone pod, so a run Wardyn's own teardown already ended is not ended twice.
func TestAgentSandboxLifecycle_ChangedSandboxSpecReadsFailed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gen      int64
		deleting bool
		swap     bool
		want     types.RunState
	}{
		{"created, as the API server leaves it", 1, false, true, types.RunRunning},
		{"spec changed after create", 2, false, true, types.RunFailed},
		{"being deleted, images swapped", 2, true, true, types.RunFailed},
		{"being deleted, images unchanged", 2, true, false, types.RunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, dyn, ref := asAdmitted(t)
			obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, ref)
			if err != nil {
				t.Fatalf("get Sandbox: %v", err)
			}
			sb := obj.(*unstructured.Unstructured).DeepCopy()
			sb.SetGeneration(tc.gen)
			if tc.deleting {
				now := metav1.Now()
				sb.SetDeletionTimestamp(&now)
			}
			const swapped = "registry.example/other:1"
			if tc.swap {
				containers, _, _ := unstructured.NestedSlice(sb.Object, "spec", "podTemplate", "spec", "containers")
				containers[0].(map[string]any)["image"] = swapped
				if err := unstructured.SetNestedSlice(sb.Object, containers, "spec", "podTemplate", "spec", "containers"); err != nil {
					t.Fatalf("set template containers: %v", err)
				}
			}
			if err := dyn.Tracker().Update(sandboxesGVR, sb, testNamespace); err != nil {
				t.Fatalf("update Sandbox: %v", err)
			}
			if tc.swap {
				changeAgentPod(t, cs, ref, func(p *corev1.Pod) { p.Spec.Containers[0].Image = swapped })
			}
			ctx := context.Background()
			for name, read := range map[string]func() (runner.Status, error){
				"Status":      func() (runner.Status, error) { return d.Status(ctx, ref) },
				"AgentStatus": func() (runner.Status, error) { return d.AgentStatus(ctx, ref, execContainerName) },
			} {
				st, err := read()
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if name == "Status" && st.State != tc.want {
					t.Errorf("Status = %+v; want %s", st, tc.want)
				}
				if replaced := st.FailureHint == agentPodReplacedMessage; replaced != (tc.want == types.RunFailed) {
					t.Errorf("%s = %+v; want the replaced-pod sentence only for a changed spec", name, st)
				}
			}
			if tc.deleting {
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if code, err := d.Wait(wctx, ref); err != nil || code != notFoundExitCode {
					t.Errorf("Wait = %d, %v; want %d, nil, as for a gone pod", code, err, notFoundExitCode)
				}
			}
		})
	}
}

// A Sandbox deleted with the orphan policy and made again under the same name starts again at
// generation 1, so a new template and the surviving pod naming the same changed image would pass the
// image and generation checks: only the pod's controlling owner UID tells it is not the run's Sandbox.
// The rig's create reactor gives every Sandbox asSandboxUID and controllerMaking copies it into the
// pod's owner reference, so an admitted run's owner matches unless a case changes it.
func TestAgentSandboxLifecycle_RecreatedSandboxReadsFailed(t *testing.T) {
	const swapped = "registry.example/other:1"
	for _, tc := range []struct {
		name  string
		uid   k8stypes.UID
		owner func(*corev1.Pod)
		want  types.RunState
	}{
		{"same Sandbox, owner UID matches", asSandboxUID, nil, types.RunRunning},
		{"made again with a new UID", "sandbox-uid-2", nil, types.RunFailed},
		{"pod has no controlling Sandbox owner", asSandboxUID, func(p *corev1.Pod) { p.OwnerReferences = nil }, types.RunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, dyn, ref := asAdmitted(t)
			obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, ref)
			if err != nil {
				t.Fatalf("get Sandbox: %v", err)
			}
			sb := obj.(*unstructured.Unstructured).DeepCopy()
			if err := dyn.Tracker().Delete(sandboxesGVR, testNamespace, ref); err != nil {
				t.Fatalf("orphan-delete Sandbox: %v", err)
			}
			sb.SetUID(tc.uid)
			sb.SetGeneration(1)
			sb.SetResourceVersion("")
			containers, _, _ := unstructured.NestedSlice(sb.Object, "spec", "podTemplate", "spec", "containers")
			containers[0].(map[string]any)["image"] = swapped
			if err := unstructured.SetNestedSlice(sb.Object, containers, "spec", "podTemplate", "spec", "containers"); err != nil {
				t.Fatalf("set template containers: %v", err)
			}
			if err := dyn.Tracker().Create(sandboxesGVR, sb, testNamespace); err != nil {
				t.Fatalf("make the Sandbox again: %v", err)
			}
			changeAgentPod(t, cs, ref, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = swapped
				if tc.owner != nil {
					tc.owner(p)
				}
			})
			ctx := context.Background()
			for name, read := range map[string]func() (runner.Status, error){
				"Status":      func() (runner.Status, error) { return d.Status(ctx, ref) },
				"AgentStatus": func() (runner.Status, error) { return d.AgentStatus(ctx, ref, execContainerName) },
			} {
				st, err := read()
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if name == "Status" && st.State != tc.want {
					t.Errorf("Status = %+v; want %s", st, tc.want)
				}
				if replaced := st.FailureHint == agentPodReplacedMessage; replaced != (tc.want == types.RunFailed) {
					t.Errorf("%s = %+v; want the replaced-pod sentence only for another Sandbox", name, st)
				}
			}
		})
	}
}

// With the backend on too, stopping a run whose agent pod's label names another live run removes the
// ref's run, its Sandbox first, and leaves the other run and its Sandbox whole.
func TestAgentSandboxLifecycle_TeardownKeysOnTheRefNotTheLabel(t *testing.T) {
	d, cs, dyn, refA := asAdmitted(t)
	ctx := context.Background()
	specB := asSpec()
	specB.RunID = uuid.New()
	sbB, err := d.CreateSandbox(ctx, specB)
	if err != nil {
		t.Fatalf("CreateSandbox B: %v", err)
	}
	before := runObjectCounts(t, cs, specB.RunID)
	changeAgentPod(t, cs, refA, func(p *corev1.Pod) { p.Labels[labelRun] = specB.RunID.String() })

	if err := d.StopSandbox(ctx, refA); err != nil {
		t.Fatalf("StopSandbox A: %v", err)
	}
	assertRunObjectsGone(t, cs, asRunID)
	if _, err := cs.Tracker().Get(podsGVR, testNamespace, refA); !isNotFound(err) {
		t.Errorf("A's relabelled agent pod is still there (get err %v)", err)
	}
	if _, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, refA); !isNotFound(err) {
		t.Errorf("A's Sandbox is still there (get err %v)", err)
	}
	if after := runObjectCounts(t, cs, specB.RunID); after != before {
		t.Errorf("stopping run A changed run B's objects: %+v, want %+v", after, before)
	}
	if _, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, sbB.Ref); err != nil {
		t.Errorf("run B's Sandbox: %v, want it kept", err)
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

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// midInitProxy creates the harness run's proxy on node n1, bound, its staging init container
// running and its main container waiting, and records it as the start wait would.
func (h *runnerHarness) midInitProxy(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	h.node(t, "n1", "uid-n1", "m1", "s1", "10.0.0.5")
	pod := h.proxyPod("n1")
	pod.Spec.InitContainers = []corev1.Container{{Name: stageProxyConfigInitName}}
	pod.Status.Phase = corev1.PodPending
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: stageProxyConfigInitName, ContainerID: "containerd://i1",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: proxyContainerName,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
	b := &bracket{d: h.d, runID: h.runID, component: componentProxy}
	if err := b.before(ctx, pod.Name); err != nil {
		t.Fatal(err)
	}
	created, err := h.cs.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.created(created); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.observePod(ctx, h.runID, componentProxy, created); err != nil {
		t.Fatal(err)
	}
	pe := h.ev.get(t, h.runID).Pods[componentProxy]
	if pe.Assoc != assocCausal || !pe.Containers[stageProxyConfigInitName].Anchored || !pe.clean(proxyContainerName) {
		t.Fatalf("setup: assoc %q, containers %+v", pe.Assoc, pe.Containers)
	}
}

// terminateMidInit is the final status kubelet v1.31.4 wrote for a proxy deleted between its init
// container's exit and its main container's start (#3537): Failed, Initialized=True, the init
// container Completed, the main container left waiting.
func terminateMidInit(p *corev1.Pod) {
	p.ResourceVersion = "11"
	p.Status.Phase = corev1.PodFailed
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: stageProxyConfigInitName, ContainerID: "containerd://i1",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", ContainerID: "containerd://i1"}}}}
}

// createRunObjects creates the run's Secret and both NetworkPolicies, labelled as CreateSandbox labels them.
func (h *runnerHarness) createRunObjects(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	l := func(component string) map[string]string {
		return wardynLabels(h.runID, component, map[string]string{runner.LabelRunnerID: testRunnerID})
	}
	if _, err := h.cs.CoreV1().Secrets(testNamespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName(h.runID), Namespace: testNamespace, Labels: l(componentProxy)}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, comp := range map[string]string{agentNetPolName(h.runID): componentAgent, proxyNetPolName(h.runID): componentProxy} {
		if _, err := h.cs.NetworkingV1().NetworkPolicies(testNamespace).Create(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: l(comp)}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *runnerHarness) assertMainNeverStartedWitness(t *testing.T) {
	t.Helper()
	pe := h.ev.get(t, h.runID).Pods[componentProxy]
	if pe.Witness == nil || pe.Witness.Arm != armMainNeverStarted || !pe.FinalizerOff {
		t.Fatalf("proxy witness %+v, finalizer off %v; want %s with the finalizer off", pe.Witness, pe.FinalizerOff, armMainNeverStarted)
	}
	if len(pe.Witness.Containers) != 1 || pe.Witness.Containers[0].Name != stageProxyConfigInitName || pe.Witness.Containers[0].Tier != tier1 {
		t.Fatalf("witness containers %+v, want the staging init container bound by tier 1", pe.Witness.Containers)
	}
}

// TestRunnerTeardownReleasesAProxyDeletedBetweenInitAndMain: CreateSandbox's rollback deletes a
// proxy whose init container has finished and whose main container has not started (#3537). The
// kubelet's final status for it is the main-never-started arm's, so the rollback completes once
// that status is there, and a rollback held before it is completed by the sweep's next pass. Until
// then the pod, its Secret and both NetworkPolicies stay.
func TestRunnerTeardownReleasesAProxyDeletedBetweenInitAndMain(t *testing.T) {
	ctx := context.Background()
	zero := int64(0)
	t.Run("held rollback, swept once the kubelet reports", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.midInitProxy(t)
		h.createRunObjects(t)
		if err := h.d.teardownByRunID(ctx, h.runID, &zero); err == nil {
			t.Fatal("the rollback completed before the kubelet reported the pod stopped")
		}
		if left := runObjectCounts(t, h.cs, h.runID); left != (runObjects{pods: 1, secrets: 1, netpols: 2}) {
			t.Fatalf("a held rollback left %+v, want the pod, the Secret and both NetworkPolicies", left)
		}
		obj, err := h.cs.Tracker().Get(podsGVR, testNamespace, proxyPodName(h.runID))
		if err != nil {
			t.Fatal(err)
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		terminateMidInit(pod)
		if err := h.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
			t.Fatal(err)
		}
		n, err := h.d.SweepOrphanedSandboxes(ctx, 0, alwaysOrphan)
		if err != nil || n != 1 {
			t.Fatalf("sweep swept %d: %v; want the held run released", n, err)
		}
		h.assertMainNeverStartedWitness(t)
		if left := runObjectCounts(t, h.cs, h.runID); left != (runObjects{}) {
			t.Fatalf("the sweep left %+v", left)
		}
	})
	t.Run("rollback after the kubelet reports", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.midInitProxy(t)
		h.createRunObjects(t)
		h.onDelete = terminateMidInit
		if err := h.d.teardownByRunID(ctx, h.runID, &zero); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		h.assertMainNeverStartedWitness(t)
		if left := runObjectCounts(t, h.cs, h.runID); left != (runObjects{}) {
			t.Fatalf("the rollback left %+v", left)
		}
	})
}

// TestRunnerMainNeverStartedArmBindsToTheCausalNode: the main-never-started status completes only
// for the recorded pod UID on its causal node, with no recorded execution of a main container.
func TestRunnerMainNeverStartedArmBindsToTheCausalNode(t *testing.T) {
	cases := []struct {
		name string
		edit func(*runEvidence, *podEvidence, *corev1.Pod, *nodeHolder) *nodeHolder
		arm  string
	}{
		{name: "bound to the causal node", arm: armMainNeverStarted},
		{name: "a main container with a baseline", edit: func(_ *runEvidence, pe *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
			pe.Containers["c"] = &containerIDs{Baseline: "containerd://c1", Anchored: true}
			return post
		}},
		{name: "a main container with a latch", edit: func(_ *runEvidence, pe *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
			pe.Containers["c"] = &containerIDs{Baseline: "containerd://c1", Anchored: true, Latched: []string{"containerd://c2"}}
			return post
		}},
		{name: "a main container running", edit: func(_ *runEvidence, pe *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
			pe.Containers["c"] = &containerIDs{Baseline: "containerd://c1", Anchored: true}
			pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "c", ContainerID: "containerd://c1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
			return post
		}},
		{name: "a main container that ran takes the terminal arm", arm: armTerminal, edit: func(_ *runEvidence, pe *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
			pe.Containers["c"] = &containerIDs{Baseline: "containerd://c1", Anchored: true}
			pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "c", ContainerID: "containerd://c1",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, ContainerID: "containerd://c1"}}}
			return post
		}},
		{name: "another pod UID", edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
			pod.UID = "uid-other"
			return post
		}},
		{name: "a node name reuse", edit: func(ev *runEvidence, _ *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
			ev.Reuse = []runner.NodeSample{{Name: "n1", UID: "uid-x"}}
			return post
		}},
		{name: "no causal association", edit: func(_ *runEvidence, pe *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
			pe.Assoc = assocU1
			return post
		}},
		{name: "another Node after the status read", edit: func(_ *runEvidence, _ *podEvidence, _ *corev1.Pod, post *nodeHolder) *nodeHolder {
			p := *post
			p.Node.UID = "uid-n1-replacement"
			return &p
		}},
		{name: "a host IP not the holder's", edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
			pod.Status.HostIP = "10.0.0.99"
			return post
		}},
		{name: "the pod garbage collector's Failed", edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
			pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reasonDeletionByPodGC})
			return post
		}},
		{name: "not being deleted", edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
			pod.DeletionTimestamp = nil
			return post
		}},
		{name: "an init container ID unlike its baseline", edit: func(_ *runEvidence, _ *podEvidence, pod *corev1.Pod, post *nodeHolder) *nodeHolder {
			pod.Status.InitContainerStatuses[0].State.Terminated.ContainerID = "containerd://i2"
			return post
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, pe, pod, n := evidenceFixture()
			pe.Containers = map[string]*containerIDs{"init": {Baseline: "containerd://i1", Anchored: true}}
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			pod.Spec.InitContainers = []corev1.Container{{Name: "init"}}
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}}
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", ContainerID: "containerd://i1",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", ContainerID: "containerd://i1"}}}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
			post := &nodeHolder{Node: n}
			if c.edit != nil {
				post = c.edit(ev, pe, pod, post)
			}
			v := ev.evaluate(pe, pod, post)
			if v.arm != c.arm {
				t.Fatalf("arm %q (%s), want %q", v.arm, v.why, c.arm)
			}
			if c.arm == armMainNeverStarted && v.tiers["init"] != tier1 {
				t.Fatalf("init container tier %q, want tier1", v.tiers["init"])
			}
		})
	}
}

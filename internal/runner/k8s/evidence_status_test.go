// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// midInitDeletedProxy is the proxy pod a delete reached between its init container's exit and its
// main container's start, as kubelet v1.31.4 left it (#3537): Failed, deleting, Initialized=True,
// the init container Completed, the main container still waiting on PodInitializing.
func midInitDeletedProxy() *corev1.Pod {
	now := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "wardyn-proxy-x", UID: "uid-proxy", DeletionTimestamp: &now, Finalizers: []string{"wardyn.dev/stop-proof"}},
		Spec: corev1.PodSpec{NodeName: "n1", RestartPolicy: corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{{Name: "stage-proxy-config"}}, Containers: []corev1.Container{{Name: "wardyn-proxy"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed, HostIP: "10.0.0.5", PodIP: "10.250.0.10",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionFalse},
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "stage-proxy-config", ContainerID: "containerd://i1", Ready: true,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", ContainerID: "containerd://i1"}}}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "wardyn-proxy",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}},
		},
	}
}

func everyContainerClean(string) bool { return true }

// TestUnstartedArmAcceptsAPodDeletedBetweenInitAndMain: the status of a pod deleted after its init
// containers completed and before any main container started is the main-never-started arm's,
// and every sign that a main container ran, or that the status is not the kubelet's final one,
// refuses it.
func TestUnstartedArmAcceptsAPodDeletedBetweenInitAndMain(t *testing.T) {
	main := func(p *corev1.Pod) *corev1.ContainerStatus { return &p.Status.ContainerStatuses[0] }
	initStatus := func(p *corev1.Pod) *corev1.ContainerStatus { return &p.Status.InitContainerStatuses[0] }
	yes := true
	cases := []struct {
		name  string
		edit  func(*corev1.Pod)
		clean func(string) bool
		arm   string
	}{
		{name: "deleted between init and main", arm: armMainNeverStarted},
		{name: "never initialized still takes its own arm", arm: armNeverInit, edit: func(p *corev1.Pod) {
			p.Status.Conditions[1].Status = corev1.ConditionFalse
			initStatus(p).State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}
			initStatus(p).ContainerID = ""
		}},
		{name: "a main container a read saw with a runtime ID", clean: func(c string) bool { return c != "wardyn-proxy" }},
		{name: "a main container running", edit: func(p *corev1.Pod) {
			main(p).State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			main(p).ContainerID = "containerd://m1"
		}},
		{name: "a main container that started and exited", edit: func(p *corev1.Pod) {
			main(p).State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, ContainerID: "containerd://m1"}}
			main(p).ContainerID = "containerd://m1"
		}},
		{name: "a waiting main container with a container ID", edit: func(p *corev1.Pod) { main(p).ContainerID = "containerd://m1" }},
		{name: "a waiting main container marked started", edit: func(p *corev1.Pod) { main(p).Started = &yes }},
		{name: "a waiting main container that restarted", edit: func(p *corev1.Pod) { main(p).RestartCount = 1 }},
		{name: "a main container the kubelet lost after it ran", edit: func(p *corev1.Pod) {
			main(p).LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"}}
		}},
		{name: "not being deleted", edit: func(p *corev1.Pod) { p.DeletionTimestamp = nil }},
		{name: "not terminal yet", edit: func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }},
		{name: "running", edit: func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }},
		{name: "the pod garbage collector's Failed", edit: func(p *corev1.Pod) {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reasonDeletionByPodGC})
		}},
		{name: "no status for the main container", edit: func(p *corev1.Pod) { p.Status.ContainerStatuses = nil }},
		{name: "no status for the init container", edit: func(p *corev1.Pod) { p.Status.InitContainerStatuses = nil }},
		{name: "no Initialized condition", edit: func(p *corev1.Pod) { p.Status.Conditions = slices.Delete(p.Status.Conditions, 1, 2) }},
		{name: "an init container that failed", edit: func(p *corev1.Pod) { initStatus(p).State.Terminated.ExitCode = 1 }},
		{name: "an init container still running", edit: func(p *corev1.Pod) {
			initStatus(p).State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}},
		{name: "no init container in the spec", edit: func(p *corev1.Pod) {
			p.Spec.InitContainers, p.Status.InitContainerStatuses = nil, nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := midInitDeletedProxy()
			if c.edit != nil {
				c.edit(pod)
			}
			clean := c.clean
			if clean == nil {
				clean = everyContainerClean
			}
			arm, inits := unstartedArm(pod, clean)
			if arm != c.arm {
				t.Fatalf("arm %q, want %q", arm, c.arm)
			}
			if c.arm == armMainNeverStarted && !slices.Equal(inits, []string{"stage-proxy-config"}) {
				t.Fatalf("terminated inits %v, want the staging init container for its tier", inits)
			}
		})
	}
}

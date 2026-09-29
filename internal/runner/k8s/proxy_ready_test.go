// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// TestCreateSandbox_AgentWaitsForProxyReady: a proxy PodIP is not a started
// proxy — the CNI assigns it before any image pull — so CreateSandbox must not
// create the agent pod until the proxy is Ready, must fail fast naming a
// terminal proxy state, and must roll back everything it created.
func TestCreateSandbox_AgentWaitsForProxyReady(t *testing.T) {
	waiting := func(reason string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "simulated"}}
	}
	for _, tc := range []struct {
		name       string
		initState  *corev1.ContainerState // nil: init container already completed
		mainState  corev1.ContainerState
		mainReady  bool
		wantErrHas string // "" for the healthy control
	}{
		{name: "main_image_pull_backoff", mainState: waiting("ImagePullBackOff"), wantErrHas: "ImagePullBackOff"},
		{name: "main_crash_loop", mainState: waiting("CrashLoopBackOff"), wantErrHas: "CrashLoopBackOff"},
		{name: "main_config_error", mainState: waiting("CreateContainerConfigError"), wantErrHas: "CreateContainerConfigError"},
		{name: "init_err_image_pull", initState: ptrState(waiting("ErrImagePull")), mainState: waiting("PodInitializing"), wantErrHas: "ErrImagePull"},
		{
			name:       "init_failed",
			initState:  &corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			mainState:  waiting("PodInitializing"),
			wantErrHas: stageProxyConfigInitName,
		},
		{name: "init_still_pending", initState: ptrState(waiting("ContainerCreating")), mainState: waiting("PodInitializing"), wantErrHas: "context deadline exceeded"},
		{name: "running_not_ready", mainState: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, wantErrHas: "context deadline exceeded"},
		{name: "healthy_proxy_control", mainState: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, mainReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			installAgentRunningReactor(t, cs)
			spec := testSandboxSpec()
			spec.Interactive = true
			cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				ga := action.(clienttesting.GetAction)
				if ga.GetName() != proxyPodName(spec.RunID) {
					return false, nil, nil
				}
				obj, err := cs.Tracker().Get(podsGVR, action.GetNamespace(), ga.GetName())
				if err != nil {
					return true, nil, err
				}
				pod := obj.(*corev1.Pod).DeepCopy()
				pod.Status.PodIP = "10.244.0.7"
				pod.Status.Phase = corev1.PodPending
				initState := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}
				if tc.initState != nil {
					initState = *tc.initState
				}
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: stageProxyConfigInitName, State: initState}}
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: proxyContainerName, State: tc.mainState, Ready: tc.mainReady}}
				if tc.mainReady {
					pod.Status.Phase = corev1.PodRunning
				}
				return true, pod, nil
			})
			cs.ClearActions()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err := d.CreateSandbox(ctx, spec)

			agentCreated := false
			for _, action := range cs.Actions() {
				if ca, ok := action.(clienttesting.CreateAction); ok && action.GetResource().Resource == "pods" {
					if ca.GetObject().(*corev1.Pod).Name == agentPodName(spec.RunID) {
						agentCreated = true
					}
				}
			}
			if tc.wantErrHas == "" {
				if err != nil || !agentCreated {
					t.Fatalf("healthy proxy did not launch the agent: err=%v agentCreated=%v", err, agentCreated)
				}
				return
			}
			if err == nil || agentCreated {
				t.Fatalf("proxy never became ready, yet err=%v agentCreated=%v", err, agentCreated)
			}
			if !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Errorf("err = %v, want it to name %q", err, tc.wantErrHas)
			}
			assertRunObjectsGone(t, cs, spec.RunID)
		})
	}
}

func ptrState(s corev1.ContainerState) *corev1.ContainerState { return &s }

// TestWaitPodIP_SchedulingAndStartBounds pins the two bounds separately: the
// IP (scheduling) within podIPWaitTimeout, then Ready within canaryWaitTimeout
// — so a cold proxy pull on a fresh node is not failed at the scheduling bound.
func TestWaitPodIP_SchedulingAndStartBounds(t *testing.T) {
	ipBound, startBound := podIPWaitTimeout, canaryWaitTimeout
	t.Cleanup(func() { podIPWaitTimeout, canaryWaitTimeout = ipBound, startBound })
	podIPWaitTimeout, canaryWaitTimeout = 400*time.Millisecond, 1600*time.Millisecond

	const podName = "wardyn-proxy-bounds"
	for _, tc := range []struct {
		name       string
		status     func(elapsed time.Duration) corev1.PodStatus
		wantErrHas []string // nil: want success
		minElapsed time.Duration
		maxElapsed time.Duration
	}{
		{
			name: "no_ip_fails_at_the_scheduling_bound",
			status: func(time.Duration) corev1.PodStatus {
				return corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes are available",
				}}}
			},
			wantErrHas: []string{"got no IP within", "Unschedulable"},
			minElapsed: 400 * time.Millisecond,
			maxElapsed: 1200 * time.Millisecond,
		},
		{
			name: "cold_pull_past_the_scheduling_bound_succeeds",
			status: func(elapsed time.Duration) corev1.PodStatus {
				st := corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.244.0.7", ContainerStatuses: []corev1.ContainerStatus{{
					Name: proxyContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}}}
				if elapsed > 800*time.Millisecond {
					st.Phase = corev1.PodRunning
					st.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
					st.ContainerStatuses[0].Ready = true
				}
				return st
			},
		},
		{
			name: "not_ready_fails_at_the_start_bound_naming_the_reason",
			status: func(time.Duration) corev1.PodStatus {
				return corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.244.0.7", ContainerStatuses: []corev1.ContainerStatus{{
					Name: proxyContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}}}
			},
			wantErrHas: []string{"context deadline exceeded", proxyContainerName + ": ContainerCreating"},
			minElapsed: 1600 * time.Millisecond,
			maxElapsed: 5 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			start := time.Now()
			cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if action.(clienttesting.GetAction).GetName() != podName {
					return false, nil, nil
				}
				return true, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: action.GetNamespace()},
					Status:     tc.status(time.Since(start)),
				}, nil
			})
			ip, err := d.waitPodIP(context.Background(), podName, nil)
			elapsed := time.Since(start)
			if tc.wantErrHas == nil {
				if err != nil || ip != "10.244.0.7" {
					t.Fatalf("waitPodIP = %q, %v; want the IP once Ready, past podIPWaitTimeout", ip, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("waitPodIP = %q, nil; want an error", ip)
			}
			for _, want := range tc.wantErrHas {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}
			if elapsed < tc.minElapsed || elapsed > tc.maxElapsed {
				t.Errorf("failed after %s, want within [%s, %s]", elapsed, tc.minElapsed, tc.maxElapsed)
			}
		})
	}
}

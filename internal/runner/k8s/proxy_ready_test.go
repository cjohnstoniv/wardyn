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

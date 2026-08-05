// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestStatus_MapsPodPhase(t *testing.T) {
	d, cs := newTestDriver(t, Config{})

	cases := []struct {
		name      string
		mutate    func(*corev1.PodStatus)
		wantState types.RunState
		wantExit  *int
	}{
		{"pending", func(s *corev1.PodStatus) { s.Phase = corev1.PodPending }, types.RunStarting, nil},
		{"running", func(s *corev1.PodStatus) { s.Phase = corev1.PodRunning }, types.RunRunning, nil},
		{"succeeded", func(s *corev1.PodStatus) {
			s.Phase = corev1.PodSucceeded
			s.ContainerStatuses = []corev1.ContainerStatus{{Name: mainContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
		}, types.RunStopped, intPtr(0)},
		{"failed", func(s *corev1.PodStatus) {
			s.Phase = corev1.PodFailed
			s.ContainerStatuses = []corev1.ContainerStatus{{Name: mainContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}}}
		}, types.RunFailed, intPtr(137)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
			setPodStatus(t, cs, testNamespace, ref, tc.mutate)

			st, err := d.Status(context.Background(), ref)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if st.State != tc.wantState {
				t.Errorf("State = %q, want %q", st.State, tc.wantState)
			}
			if tc.wantExit != nil {
				if st.ExitCode == nil || *st.ExitCode != *tc.wantExit {
					t.Errorf("ExitCode = %v, want %d", st.ExitCode, *tc.wantExit)
				}
			}
		})
	}
}

func TestStatus_WaitingReasonSurfacedInMessage(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, ref, func(s *corev1.PodStatus) {
		s.Phase = corev1.PodPending
		s.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  mainContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "rpc error: pull access denied"}},
		}}
	})

	st, err := d.Status(context.Background(), ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != types.RunStarting {
		t.Errorf("State = %q, want STARTING", st.State)
	}
	if st.Message == "" {
		t.Error("Message = \"\", want the waiting reason/detail surfaced (ImagePullBackOff)")
	}
}

func TestStatus_NotFound(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	st, err := d.Status(context.Background(), "wardyn-agent-does-not-exist")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != types.RunStopped {
		t.Errorf("State = %q, want STOPPED for a not-found sandbox", st.State)
	}
}

// TestTeardown_GhostRefIsIdempotent covers "ghost ref returns nil": tearing
// down a ref that never existed (or was already torn down) is success, not
// an error — both StopSandbox and KillSandbox.
func TestTeardown_GhostRefIsIdempotent(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	if err := d.StopSandbox(context.Background(), "wardyn-agent-ghost"); err != nil {
		t.Errorf("StopSandbox on a ghost ref: %v, want nil", err)
	}
	if err := d.KillSandbox(context.Background(), "wardyn-agent-ghost"); err != nil {
		t.Errorf("KillSandbox on a ghost ref: %v, want nil", err)
	}
}

// TestTeardown_SweepsEverySiblingByLabel covers the label-selector sweep:
// resolving ref's run id from its wardyn.run-id label reaches the agent pod,
// the proxy pod, BOTH NetworkPolicies, and the config Secret in one pass —
// none left behind.
func TestTeardown_SweepsEverySiblingByLabel(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.9")

	spec := testSandboxSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	assertRunObjectsGone(t, cs, spec.RunID)

	// Idempotent: killing the now-gone ref again is still success.
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Errorf("second KillSandbox: %v, want nil (idempotent)", err)
	}
}

// TestTeardown_UnresolvedRunIDLabel covers the fail-closed edge case: the
// pod is found, but its wardyn.run-id label is missing (so the sibling
// proxy/netpols/secret cannot be located) — teardown must surface this
// honestly rather than report a false success.
func TestTeardown_UnresolvedRunIDLabel(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	name := "wardyn-agent-orphan"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, // no wardyn.run-id label
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: mainContainerName, Image: "x"}}},
	}
	if _, err := cs.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	err := d.StopSandbox(context.Background(), name)
	if err == nil {
		t.Fatal("StopSandbox on a label-less pod: want an error, got nil")
	}
}

func intPtr(i int) *int { return &i }

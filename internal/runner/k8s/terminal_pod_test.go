// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestTerminalPodEndsExecWithoutContainerExit(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		for _, state := range []string{"missing", "running", "waiting"} {
			t.Run(string(phase)+"/"+state, func(t *testing.T) {
				d, cs := newTestDriver(t, Config{})
				ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
				execID, err := d.Exec(t.Context(), ref, []string{"agent-run", "task"})
				if err != nil {
					t.Fatal(err)
				}
				setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
					st.Phase = phase
					if phase == corev1.PodFailed {
						st.Reason = "Evicted"
						st.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 64Mi"
					}
					if state == "missing" {
						return
					}
					status := corev1.ContainerStatus{Name: execID}
					if state == "running" {
						status.State.Running = &corev1.ContainerStateRunning{}
					} else {
						status.State.Waiting = &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}
					}
					st.EphemeralContainerStatuses = []corev1.ContainerStatus{status}
				})

				st, err := d.AgentStatus(t.Context(), ref, execID)
				if err != nil || st.State != types.RunFailed || (st.ExitCode != nil && *st.ExitCode == 0) {
					t.Errorf("AgentStatus = %+v, %v; want terminal failure without a fabricated successful exit", st, err)
				}
				if phase == corev1.PodFailed && !strings.Contains(st.Message, "Evicted") {
					t.Errorf("AgentStatus lost the eviction reason: %+v", st)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				code, err := d.Wait(ctx, ref)
				if err != nil || code == 0 {
					t.Errorf("Wait = %d, %v; want an authoritative nonzero exit, not continued polling", code, err)
				}
			})
		}
	}
}

func TestTerminalPodPreservesAgentExit(t *testing.T) {
	for _, exitCode := range []int32{0, 7} {
		d, cs := newTestDriver(t, Config{})
		ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
		execID, err := d.Exec(t.Context(), ref, []string{"agent-run", "task"})
		if err != nil {
			t.Fatal(err)
		}
		setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
			st.Phase = corev1.PodFailed
			st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
				Name:  execID,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
			}}
		})
		st, err := d.AgentStatus(t.Context(), ref, execID)
		if err != nil || st.ExitCode == nil || *st.ExitCode != int(exitCode) {
			t.Errorf("AgentStatus = %+v, %v; want actual agent exit %d", st, err, exitCode)
		}
		code, err := d.Wait(t.Context(), ref)
		if err != nil || code != int(exitCode) {
			t.Errorf("Wait = %d, %v; want actual agent exit %d", code, err, exitCode)
		}
	}
}

func TestFailedPodDoesNotInventSuccessfulMainExit(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.Phase = corev1.PodFailed
		st.Reason = "NodeLost"
	})
	st, err := d.AgentStatus(t.Context(), ref, "")
	if err != nil || st.State != types.RunFailed || st.ExitCode != nil {
		t.Fatalf("AgentStatus = %+v, %v; want FAILED with unknown exit, not exit 0", st, err)
	}
}

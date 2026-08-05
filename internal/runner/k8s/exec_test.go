// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestExec_Success covers the happy path: an ephemeral container named
// "wardyn-agent" is added, carrying argv as Command, the main container's
// image, its Env copied verbatim, the full restricted securityContext, and
// NO Resources (the apiserver rejects a resource request on an ephemeral
// container).
func TestExec_Success(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	runID := uuid.New()
	ref := createAgentPodFixture(t, cs, runID, "wardyn/agent-claude:local", map[string]string{"HOME": "/home/agent"})

	execID, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "do the task"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if execID != execContainerName {
		t.Errorf("execID = %q, want %q", execID, execContainerName)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) != 1 {
		t.Fatalf("EphemeralContainers = %d, want 1", len(pod.Spec.EphemeralContainers))
	}
	ec := pod.Spec.EphemeralContainers[0]
	if ec.Name != execContainerName {
		t.Errorf("ephemeral container name = %q, want %q", ec.Name, execContainerName)
	}
	if ec.Image != "wardyn/agent-claude:local" {
		t.Errorf("ephemeral container image = %q, want the main container's image", ec.Image)
	}
	if len(ec.Command) != 2 || ec.Command[0] != "/usr/local/bin/agent-run" {
		t.Errorf("ephemeral container command = %v", ec.Command)
	}
	if len(ec.Env) != 1 || ec.Env[0].Name != "HOME" {
		t.Errorf("ephemeral container env = %v, want the main container's env copied verbatim", ec.Env)
	}
	if ec.SecurityContext == nil || ec.SecurityContext.RunAsNonRoot == nil || !*ec.SecurityContext.RunAsNonRoot {
		t.Errorf("ephemeral container SecurityContext = %+v, want the full restricted set", ec.SecurityContext)
	}
	if len(ec.Resources.Limits) != 0 || len(ec.Resources.Requests) != 0 {
		t.Errorf("ephemeral container Resources = %+v, want zero-value (apiserver rejects it on ephemeral containers)", ec.Resources)
	}
}

// TestExec_SecondCallFails covers the once-per-ref contract: ephemeral
// containers are add-only, so a second Exec on the same ref must return a
// clear error — never a silent no-op, never the prior exec's id.
func TestExec_SecondCallFails(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)

	first, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "--selftest"})
	if err != nil {
		t.Fatalf("first Exec: %v", err)
	}

	second, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "the real task"})
	if err == nil {
		t.Fatal("second Exec: want an error, got nil")
	}
	if !errors.Is(err, errSecondExec) {
		t.Errorf("err = %v, want errors.Is(err, errSecondExec)", err)
	}
	if second != "" {
		t.Errorf("second Exec id = %q, want \"\" (never the prior exec's id)", second)
	}
	if second == first {
		t.Errorf("second Exec must never return the prior exec's id")
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) != 1 {
		t.Errorf("EphemeralContainers = %d after a refused second Exec, want 1 (no silent no-op growth, no duplicate add)", len(pod.Spec.EphemeralContainers))
	}
}

// TestExec_BYOIRefused covers the substrate-seam BYOI refusal: a
// wardyn-byoi/ image is refused on the FIRST Exec call (the selftest, in
// dispatch's real flow) — never even reaching the point of creating an
// ephemeral container — because BYOI's selftest-then-task double-exec is
// docker-only (ephemeral containers are add-only, impossible on k8s).
func TestExec_BYOIRefused(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn-byoi/some-customer-image:latest", nil)

	execID, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "--selftest"})
	if err == nil {
		t.Fatal("Exec: want an error refusing BYOI, got nil")
	}
	if !errors.Is(err, errBYOIUnsupported) {
		t.Errorf("err = %v, want errors.Is(err, errBYOIUnsupported)", err)
	}
	if execID != "" {
		t.Errorf("execID = %q, want \"\"", execID)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) != 0 {
		t.Errorf("EphemeralContainers = %d, want 0 (refused before any ephemeral container is created)", len(pod.Spec.EphemeralContainers))
	}
}

// TestExec_NonBYOI_NormalDispatchExecsOnce confirms the flip side of the BYOI
// refusal: a normal (non-BYOI) image's single Exec call — dispatch's real
// non-BYOI path (startAgentOrIdle execs the task exactly once, no selftest)
// — succeeds cleanly.
func TestExec_NonBYOI_NormalDispatchExecsOnce(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)

	if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "the task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
}

func TestExec_EmptyArgv(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)

	if _, err := d.Exec(context.Background(), ref, nil); err == nil {
		t.Fatal("Exec with empty argv: want an error, got nil")
	}
}

// TestWait_NotCalled covers the restart-safe "Exec not called?" guard: no
// ephemeral container in the pod's Spec means Wait must error rather than
// poll forever.
func TestWait_NotCalled(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)

	if _, err := d.Wait(context.Background(), ref); err == nil {
		t.Fatal("Wait with no Exec: want an error, got nil")
	}
}

// TestWait_ReturnsExitCode covers the normal completion path: a Terminated
// ephemeral-container status is read straight from the apiserver.
func TestWait_ReturnsExitCode(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name:  execContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
		}}
	})

	code, err := d.Wait(context.Background(), ref)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 3 {
		t.Errorf("Wait exit code = %d, want 3", code)
	}
}

// TestWait_PodNotFoundIsAuthoritative covers the A0 contract correction:
// unlike docker (where a vanished exec is an ERROR), a vanished POD on k8s IS
// authoritative completion — Wait returns cleanly with notFoundExitCode, not
// an error, so a concurrent KillSandbox's pod deletion doesn't strand the
// completion watcher retrying forever.
func TestWait_PodNotFoundIsAuthoritative(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	code, err := d.Wait(context.Background(), "wardyn-agent-does-not-exist")
	if err != nil {
		t.Fatalf("Wait on a not-found pod: %v, want a clean (authoritative) return", err)
	}
	if code != notFoundExitCode {
		t.Errorf("Wait exit code = %d, want notFoundExitCode (%d)", code, notFoundExitCode)
	}
}

// TestAgentStatus_EmptyExecID_FallsBackToStatus covers the exec-less
// fallback: agentExecID == "" means the pod IS the agent (mirrors docker).
func TestAgentStatus_EmptyExecID_FallsBackToStatus(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) { st.Phase = corev1.PodRunning })

	st, err := d.AgentStatus(context.Background(), ref, "")
	if err != nil {
		t.Fatalf("AgentStatus: %v", err)
	}
	if st.State != types.RunRunning {
		t.Errorf("State = %q, want RUNNING (fell back to Status)", st.State)
	}
}

// TestAgentStatus_RestartSafe_ReadsLiveFromAPIServer covers "restart-safe by
// construction": AgentStatus needs nothing but the apiserver and the
// agentExecID string a caller persisted — no in-memory state on the Driver at
// all (a freshly constructed Driver, as if wardynd had just restarted, reads
// the SAME answer as one that has been running the whole time).
func TestAgentStatus_RestartSafe_ReadsLiveFromAPIServer(t *testing.T) {
	_, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name:  execContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}}
	})

	// A brand-new Driver value over the SAME clientset, built with a bare
	// struct literal rather than newTestDriver/newWithClient (no canary,
	// no other construction-time state) — simulates a wardynd restart: ZERO
	// shared in-memory state with the Driver that ran Exec, only the
	// apiserver (the fake's backing store) in common.
	fresh := &Driver{clientset: cs, cfg: Config{Namespace: testNamespace}}

	st, err := fresh.AgentStatus(context.Background(), ref, execContainerName)
	if err != nil {
		t.Fatalf("AgentStatus: %v", err)
	}
	if st.State != types.RunStopped || st.ExitCode == nil || *st.ExitCode != 1 {
		t.Errorf("AgentStatus = %+v, want RunStopped exit_code=1", st)
	}
}

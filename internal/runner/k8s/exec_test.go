// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestExec_Success covers the happy path: an ephemeral container named
// "wardyn-agent" is added, carrying the main container's image, its Env
// copied verbatim, the full agent securityContext (H2: RunAsUser:1000, not
// merely RunAsNonRoot — every agent image documents a NAME-form USER the
// kubelet can't admission-check without it), NO Resources (the apiserver
// rejects a resource request on an ephemeral container), and argv wrapped
// by the recorder (SessionRecording: mirrors docker's recordCmd) delivering
// to the brokered proxy upload URL.
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
	wantURL := fmt.Sprintf("http://wardyn-proxy:%d/wardyn/v1/recordings/%s", runner.ProxyListenPort, runID)
	wantCmd := []string{"wardyn-rec", "-cast-dir", "/tmp", "-upload-url", wantURL, "-run", runID.String(), "--", "/usr/local/bin/agent-run", "do the task"}
	if !equalStrings(ec.Command, wantCmd) {
		t.Errorf("ephemeral container command = %v, want %v (recorder-wrapped, masked-upload URL)", ec.Command, wantCmd)
	}
	if len(ec.Env) != 1 || ec.Env[0].Name != "HOME" {
		t.Errorf("ephemeral container env = %v, want the main container's env copied verbatim", ec.Env)
	}
	if ec.SecurityContext == nil || ec.SecurityContext.RunAsNonRoot == nil || !*ec.SecurityContext.RunAsNonRoot {
		t.Errorf("ephemeral container SecurityContext = %+v, want the full restricted set", ec.SecurityContext)
	}
	if ec.SecurityContext == nil || ec.SecurityContext.RunAsUser == nil || *ec.SecurityContext.RunAsUser != 1000 {
		t.Errorf("ephemeral container RunAsUser = %v, want *1000 (H2: agent images document a name-form USER)", ec.SecurityContext.RunAsUser)
	}
	if len(ec.Resources.Limits) != 0 || len(ec.Resources.Requests) != 0 {
		t.Errorf("ephemeral container Resources = %+v, want zero-value (apiserver rejects it on ephemeral containers)", ec.Resources)
	}
}

// TestRecordCmd_UnresolvableRunID covers the defensive fallback: an
// unparseable/missing wardyn.run-id label degrades to a local-only
// (never-uploaded) cast rather than failing Exec outright — mirrors
// docker's own tolerance for an unresolvable run id label.
func TestRecordCmd_UnresolvableRunID(t *testing.T) {
	got := recordCmd(uuid.Nil, []string{"/usr/local/bin/agent-run", "task"})
	for _, arg := range got {
		if strings.Contains(arg, "-upload-url") || strings.Contains(arg, "http://") {
			t.Errorf("recordCmd(uuid.Nil, ...) = %v, want no upload URL when the run id is unresolvable", got)
		}
	}
	if got[0] != "wardyn-rec" {
		t.Errorf("recordCmd still wraps with wardyn-rec even without a run id, got %v", got)
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

// TestWait_FailsClosedOnHardWaitingReason is the 0.6.6 regression: an
// ephemeral exec container stuck Waiting on a Reason that will never resolve
// on its own (terminalWaitingReasons, canary.go) must return
// runner.ErrExecNeverStarted PROMPTLY, not poll forever waiting for a
// Terminated status that will never arrive — the shape that made a k8s
// connectivity probe hang for its full wait budget whatever the network did,
// because run.exec had already recorded success (the apiserver accepted the
// ephemeral container add) with no way to tell "still starting" from "will
// never start".
func TestWait_FailsClosedOnHardWaitingReason(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name: execContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: "secret \"wardyn-agent\" not found",
			}},
		}}
	})

	// A short deadline: if Wait fell through to its normal poll loop instead
	// of returning immediately on the hard reason, this proves it by timing
	// out before the loop's own ctx.Err() branch would fire.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := d.Wait(ctx, ref)
	if err == nil {
		t.Fatal("Wait: want an error for a container that will never start, got nil")
	}
	if !errors.Is(err, runner.ErrExecNeverStarted) {
		t.Errorf("err = %v, want errors.Is(err, runner.ErrExecNeverStarted)", err)
	}
	if !strings.Contains(err.Error(), "CreateContainerConfigError") {
		t.Errorf("err = %v, want it to name the observed Reason", err)
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

// TestAgentStatus_WaitingReasonSurfacedInMessage is the 0.6.6 regression: a
// Waiting ephemeral container used to report only State: RunStarting with no
// Reason at all, so neither an operator nor the site-config probe's
// timed_out detail (site_config_probe.go) could tell "still legitimately
// starting" from "stuck on a platform problem". Message must now name both
// the Reason and any Message the apiserver attached.
func TestAgentStatus_WaitingReasonSurfacedInMessage(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name: execContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: "secret \"wardyn-agent\" not found",
			}},
		}}
	})

	st, err := d.AgentStatus(context.Background(), ref, execContainerName)
	if err != nil {
		t.Fatalf("AgentStatus: %v", err)
	}
	if st.State != types.RunStarting {
		t.Errorf("State = %q, want STARTING", st.State)
	}
	if !strings.Contains(st.Message, "CreateContainerConfigError") || !strings.Contains(st.Message, "wardyn-agent") {
		t.Errorf("Message = %q, want it to name the Waiting Reason and Message", st.Message)
	}
}

// TestAgentStatus_ExecAddedButNotYetStarted_MustNotReadTerminal is the
// k8s half of GAP-RECONCILE-1, which the docker driver hardened and this
// substrate never did.
//
// Between UpdateEphemeralContainers returning (run.exec records SUCCESS) and
// the kubelet publishing the container's first status, the pod carries the
// exec in Spec.EphemeralContainers with NO matching entry in
// Status.EphemeralContainerStatuses. AgentStatus's fall-through
// (exec.go, the trailing `return runner.Status{State: types.RunStopped,
// Message: "agent exec not found"}`) reports that window as a DEFINITIVE
// terminal state with a NIL error -- indistinguishable from "the agent exec
// is gone".
//
// Both consumers finalize on exactly that pair: sweepRunWatchers
// (reconcile.go, `if serr == nil && isTerminalRunState(st.State)`) and
// reconcileWatch (reconcile.go, `if isTerminalRunState(st.State)`) turn a
// nil-ExitCode terminal into RunFailed + StopSandbox. A wardynd restart (or a
// watcher-lease handoff) landing in that window therefore kills a healthy,
// just-started k8s exec run and reports it FAILED.
//
// The docker driver refuses to make that call: an exec-404 while the
// container is still RUNNING returns an ERROR ("ambiguous; daemon restart
// under live-restore?"), which routes both consumers into their bounded
// retry/backoff instead of finalizing. k8s has strictly BETTER evidence here
// -- the pod Get succeeded and the exec is present in Spec -- so it must not
// report a terminal state it cannot support.
//
// Want: not-yet-started is RunStarting (the same verdict a Waiting status
// gets), or an error. Either routes the reconciler to retry. Have: RunStopped
// with a nil error.
func TestAgentStatus_ExecAddedButNotYetStarted_MustNotReadTerminal(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) { st.Phase = corev1.PodRunning })

	// Exec succeeds: the apiserver accepted the ephemeral container. The fake
	// clientset does not pretend to be the kubelet, which is exactly the real
	// pre-kubelet window -- Spec has the container, Status does not.
	execID, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "task"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	st, err := d.AgentStatus(context.Background(), ref, execID)
	if err != nil {
		return // an error is an acceptable answer: it routes the reconciler to retry.
	}
	if st.State.IsTerminal() {
		t.Fatalf("AgentStatus for an exec the apiserver accepted but the kubelet has not started yet = %+v; "+
			"a terminal state with a nil error makes sweepRunWatchers/reconcileWatch finalize a healthy run FAILED "+
			"and tear its sandbox down (docker returns an ambiguity ERROR for the same window, GAP-RECONCILE-1)", st)
	}
}

// TestExec_EphemeralContainerInheritsSecretKeyRefEnv is the OTHER half of the
// F9-H1 fix, and the half a driver is most likely to get wrong: the agent's
// real work runs in the ephemeral exec container, not the idle main one, so a
// credential that reached only the main container would break every env_secret
// grant — and one that reached the ephemeral container as an inline Value
// would put it straight back in the API-readable pod spec the fix just cleared.
//
// Exec copies main.Env verbatim, so the reference travels rather than the
// value. This test drives the FULL CreateSandbox -> Exec path (not a fixture
// pod) precisely so it fails if the two ever stop agreeing about the carrier.
func TestExec_EphemeralContainerInheritsSecretKeyRefEnv(t *testing.T) {
	const tokenVal = "ghp_live_stored_secret_9f2c"
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.SecretEnv = map[string]string{"CORP_API_TOKEN": tokenVal}
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run", "do the task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) != 1 {
		t.Fatalf("EphemeralContainers = %d, want 1", len(pod.Spec.EphemeralContainers))
	}
	ec := pod.Spec.EphemeralContainers[0]
	var got *corev1.EnvVar
	for _, e := range ec.Env {
		if e.Name == "CORP_API_TOKEN" {
			got = &e
		}
		if e.Value == tokenVal {
			t.Errorf("ephemeral container env %s carries the credential INLINE (API-readable via pods/get): %q", e.Name, e.Value)
		}
	}
	if got == nil {
		t.Fatalf("ephemeral container has no CORP_API_TOKEN: the agent process would run without its granted credential: %+v", ec.Env)
	}
	if got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil ||
		got.ValueFrom.SecretKeyRef.Name != secretName(spec.RunID) || got.ValueFrom.SecretKeyRef.Key != secretEnvDataKey("CORP_API_TOKEN") {
		t.Errorf("ephemeral container CORP_API_TOKEN ValueFrom = %+v, want secretKeyRef{%s/%s} — the same reference the main container holds",
			got.ValueFrom, secretName(spec.RunID), secretEnvDataKey("CORP_API_TOKEN"))
	}
	// Parity, stated as parity: whatever the main container got, the ephemeral
	// one got, so neither can drift into a different carrier on its own.
	main, ok := findContainer(pod.Spec.Containers, mainContainerName)
	if !ok {
		t.Fatal("agent pod lost its main container")
	}
	if len(ec.Env) != len(main.Env) {
		t.Errorf("ephemeral env (%d entries) and main env (%d entries) diverged: %+v vs %+v", len(ec.Env), len(main.Env), ec.Env, main.Env)
	}
}

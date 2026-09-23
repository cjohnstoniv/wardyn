// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// execExitRunner models the exact shape: the sandbox CONTAINER is still
// up (an idle `sleep infinity`, so container-level Status reports RUNNING), but the
// AGENT exec has already exited. AgentStatus — given the exec id persisted at Exec
// — reports the agent's real exit; the old container-Status boot reconciler never
// could, so a crashed-and-restarted exec run was stranded RUNNING forever with a
// live sandbox + un-revoked credentials.
type execExitRunner struct {
	*fakeRunner
	agentExit int
	stopped   []string
}

func (r *execExitRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil // idle container still up
}
func (r *execExitRunner) AgentStatus(_ context.Context, _, execID string) (runner.Status, error) {
	if execID == "" { // exec-less / main-process: container IS the agent
		return runner.Status{State: types.RunRunning}, nil
	}
	code := r.agentExit
	return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
}
func (r *execExitRunner) StopSandbox(_ context.Context, ref string) error {
	r.stopped = append(r.stopped, ref)
	return nil
}

// bootReconcileStore serves a single stranded RUNNING run to ReconcileOnBoot and
// records the terminal transition the reconciler applies (via UpdateRunStateIf).
// Mutex-guarded: the periodic sweeper drives it from its own goroutine.
type bootReconcileStore struct {
	store.Store
	run          types.AgentRun
	mu           sync.Mutex
	toState      types.RunState
	transitioned bool
	claimed      bool
	claims       int
	// beats / finals, when non-nil, receive every lease heartbeat and every
	// terminal transition (buffered, non-blocking) so a test can observe work the
	// sweeper goroutine does on its own schedule without polling.
	beats  chan uuid.UUID
	finals chan types.RunState

	// setExecIDErr, when non-nil, fails EVERY SetRunAgentExecID — the store
	// outage c5 is about. setExecIDCalls counts the attempts, so the single
	// retry is pinned as behaviour and not merely as a comment.
	setExecIDErr   error
	setExecIDCalls int
	// hint is the failure sentence failAndRevoke persisted (optional store
	// capability, runFailureHintSetter).
	hint string
}

func (s *bootReconcileStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hint = hint
	return nil
}

// hintValue reads the failure hint under lock (the reap runs on the sweeper's
// own goroutine, so a bare field read races with SetRunFailureHint above).
func (s *bootReconcileStore) hintValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hint
}

func (s *bootReconcileStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	return []types.AgentRun{s.run}, nil
}

// ClaimStaleRunWatchers models the real lease: the stranded run is claimable
// once, and a second sweep sees the fresh heartbeat the first one wrote and gets
// nothing. Adoption now runs through this claim, not the ListRuns scan. The
// sandbox_ref guard mirrors the real query — a run that never dispatched has
// nothing to watch and is the boot pass's business, not the sweep's.
func (s *bootReconcileStore) ClaimStaleRunWatchers(context.Context, string, time.Duration) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimed || s.run.SandboxRef == "" {
		return nil, nil
	}
	s.claimed = true
	return []types.AgentRun{s.run}, nil
}

func (s *bootReconcileStore) HeartbeatRunWatcher(_ context.Context, id uuid.UUID, _ string) error {
	select {
	case s.beats <- id: // nil channel blocks, so the default arm covers "not watching"
	default:
	}
	return nil
}

// RunWatcherFresh: this double models a CRASH scenario (the prior process's
// watcher is gone), so the lease is always STALE — the undispatched reaper must
// still reap a sandbox-less run past grace.
func (s *bootReconcileStore) RunWatcherFresh(context.Context, uuid.UUID, time.Duration) (bool, error) {
	return false, nil
}
func (s *bootReconcileStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id == s.run.ID {
		return s.run, nil
	}
	return types.AgentRun{}, store.ErrNotFound
}

// SetRunAgentExecID models the real scoped-write dispatch does mid-launch
// (store.go) so a test can drive startAgentOrIdle and then sweepRunWatchers
// against the SAME persisted value, rather than hand-setting AgentExecID and
// only ever exercising the guard's condition in isolation.
func (s *bootReconcileStore) SetSandboxRef(context.Context, uuid.UUID, string) error { return nil }

func (s *bootReconcileStore) SetRunAgentExecID(_ context.Context, id uuid.UUID, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setExecIDCalls++
	if s.setExecIDErr != nil {
		return s.setExecIDErr
	}
	if id == s.run.ID {
		s.run.AgentExecID = execID
	}
	return nil
}
func (s *bootReconcileStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toState, s.transitioned = to, true
	select {
	case s.finals <- to: // nil channel blocks, so the default arm covers "not watching"
	default:
	}
	return true, nil
}

// finalTransition reports the terminal state the reconciler wrote, if any.
func (s *bootReconcileStore) finalTransition() (types.RunState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toState, s.transitioned
}

func (s *bootReconcileStore) claimCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

// bootTestCtx is a daemon-lifetime BaseCtx bounded by the test: ReconcileOnBoot
// starts the periodic watcher sweeper on BaseCtx, and that goroutine must stop
// when the test does rather than outliving it against a dead fake.
func bootTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func execRun(t *testing.T, agentExecID string) types.AgentRun {
	t.Helper()
	runID := uuid.New()
	now := time.Now().UTC()
	return types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker",
		SandboxRef:   "sleep-infinity-" + runID.String(),
		AgentExecID:  agentExecID, // the value persisted at Exec time
	}
}

// TestReconcileOnBoot_ExecRunFinalizesFromAgentExit: after a restart, a RUNNING
// exec-based run whose agent has exited (but whose idle sandbox container is still
// up) must finalize + revoke + tear down, not strand. The reconciler observes
// AgentStatus (the persisted exec id), not container Status, which for an idle
// `sleep infinity` reports RUNNING forever. Counterfactual: the runner's container
// Status is RUNNING here — a reconciler reading it would re-attach the run and
// never finalize it, so `transitioned` would stay false.
func TestReconcileOnBoot_ExecRunFinalizesFromAgentExit(t *testing.T) {
	h := newHarness(t)
	fr := &execExitRunner{fakeRunner: &fakeRunner{}, agentExit: 0}
	run := execRun(t, "agent-exec-id")
	fake := &bootReconcileStore{run: run}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = fr
	cfg.Broker = h.broker
	cfg.BaseCtx = bootTestCtx(t)
	srv := New(cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("reconcile on boot: %v", err)
	}

	if !fake.transitioned || fake.toState != types.RunCompleted {
		t.Fatalf("an exec run whose agent exited 0 must finalize COMPLETED via AgentStatus; transitioned=%v to=%q — container Status alone reports RUNNING forever", fake.transitioned, fake.toState)
	}
	if h := fake.hintValue(); h != "" {
		t.Errorf("a run finalizing to a successful terminal state must get NO failure hint (it would paint a red reason chip on a completed run), got %q", h)
	}
	if len(fr.stopped) == 0 {
		t.Error("finalize must tear the still-up idle sandbox down")
	}
	revoked := false
	for _, id := range h.broker.revoked {
		if id == run.ID {
			revoked = true
		}
	}
	if !revoked {
		t.Errorf("finalize must run the credential revoke cascade for %s (the C3 property says was defeated); broker.revoked=%v", run.ID, h.broker.revoked)
	}
}

// TestReconcileOnBoot_ExecRunFailsFromNonZeroExit checks the exit-code mapping — a
// non-zero agent exit finalizes FAILED, not COMPLETED.
func TestReconcileOnBoot_ExecRunFailsFromNonZeroExit(t *testing.T) {
	h := newHarness(t)
	fr := &execExitRunner{fakeRunner: &fakeRunner{}, agentExit: 1}
	fake := &bootReconcileStore{run: execRun(t, "agent-exec-id")}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = fr
	cfg.Broker = h.broker
	cfg.BaseCtx = bootTestCtx(t)
	srv := New(cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("reconcile on boot: %v", err)
	}
	if !fake.transitioned || fake.toState != types.RunFailed {
		t.Fatalf("a non-zero agent exit must finalize FAILED; transitioned=%v to=%q", fake.transitioned, fake.toState)
	}
	if fake.hintValue() == "" {
		t.Error("a run finalizing to FAILED must get a failure hint so the badge carries a reason")
	}
}

// errProbeRunner's AgentStatus always errors — a persistent docker-daemon blip,
// NOT a terminal state.
type errProbeRunner struct{ *fakeRunner }

func (r *errProbeRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{}, errors.New("docker: daemon unreachable")
}

// TestReconcileOnBoot_TransientProbeErrorDoesNotFinalize: a transient AgentStatus
// error at boot must
// NOT finalize a possibly-healthy RUNNING run (that would false-kill it + revoke its
// creds on a daemon blip). A genuinely-gone sandbox reports a terminal STATE, not an
// error, so an error means "couldn't determine" → re-attach a watcher and retry.
func TestReconcileOnBoot_TransientProbeErrorDoesNotFinalize(t *testing.T) {
	h := newHarness(t)
	fr := &errProbeRunner{fakeRunner: &fakeRunner{}}
	fake := &bootReconcileStore{run: execRun(t, "agent-exec-id")}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = fr
	cfg.Broker = h.broker
	// Cancel the watcher base ctx right after boot so the re-attached goroutine
	// exits promptly (it would otherwise need reconcileMaxProbeErrors ticks anyway).
	ctx, cancel := context.WithCancel(context.Background())
	cfg.BaseCtx = ctx
	srv := New(cfg)

	err := srv.ReconcileOnBoot(context.Background())
	cancel()
	if err != nil {
		t.Fatalf("reconcile on boot: %v", err)
	}
	if fake.transitioned {
		t.Fatalf("a transient AgentStatus probe error must NOT finalize a healthy run (crown runs-fsm); it re-attaches instead — got a %q transition", fake.toState)
	}
}

// TestReconcileOnBoot_UndispatchedRunAgeGate pins the age gate. The pass is
// table-wide and now periodic, so under multiple replicas it runs constantly
// against runs OTHER replicas are still provisioning — a run legitimately has no
// sandbox_ref for its whole pre-dispatch window. Without the gate, one pod
// restart FAILs every in-flight pre-dispatch run in the fleet. The middle case is
// the margin: a build may finish at its full imageBuildTimeout deadline and STILL
// need a CreateSandbox image pull before sandbox_ref lands, so a run's age at
// that moment legitimately exceeds the build deadline.
func TestReconcileOnBoot_UndispatchedRunAgeGate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		finalize bool
	}{
		{"still inside the image-build window", time.Minute, false},
		{"at the build deadline, sandbox creation still to come", imageBuildTimeout + time.Minute, false},
		{"past the grace, nothing anywhere is still starting it", undispatchedGrace + time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			run := execRun(t, "")
			run.SandboxRef = "" // never dispatched
			run.CreatedAt = time.Now().UTC().Add(-tc.age)
			fake := &bootReconcileStore{run: run}
			cfg := baseTestConfig(h, fake)
			cfg.Runner = &fakeRunner{}
			cfg.Broker = h.broker
			cfg.BaseCtx = bootTestCtx(t)
			srv := New(cfg)

			if err := srv.ReconcileOnBoot(context.Background()); err != nil {
				t.Fatalf("reconcile on boot: %v", err)
			}
			to, got := fake.finalTransition()
			if got != tc.finalize {
				t.Fatalf("run created %s ago, no sandbox: finalized=%v (to=%q), want finalized=%v", tc.age, got, to, tc.finalize)
			}
			if got && to != types.RunFailed {
				t.Errorf("an abandoned pre-dispatch run must finalize FAILED, got %q", to)
			}
		})
	}
}

// TestStartCompletionWatcher_HoldsTheLease pins runs_lifecycle.go's lease hold.
// Deleting it leaves every other test green while production silently breaks the
// other way round: the run's live watcher stops refreshing the lease, so ~90s
// after dispatch every other replica's sweep sees an expired lease and adopts a
// run that is being watched perfectly well.
func TestStartCompletionWatcher_HoldsTheLease(t *testing.T) {
	h := newHarness(t)
	run := execRun(t, "agent-exec-id")
	fake := &bootReconcileStore{run: run, beats: make(chan uuid.UUID, 4)}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{} // Wait blocks until BaseCtx is cancelled
	cfg.Broker = h.broker
	cfg.BaseCtx = bootTestCtx(t)
	srv := New(cfg)

	srv.startCompletionWatcher(run.ID, run.SandboxRef, run.AgentExecID)

	select {
	case got := <-fake.beats:
		if got != run.ID {
			t.Fatalf("lease taken on %s, want the run being watched (%s)", got, run.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the completion watcher must take the run's watcher lease before it blocks on Wait; without it another replica adopts a run that is already being watched")
	}
}

// TestRunWatcherSweeper_PeriodicClaimAdoptsAndLeases covers the PERIODIC half —
// the part that makes adoption independent of the dead pod ever booting again.
// ReconcileOnBoot's one synchronous sweep would pass without it. Asserts both
// links of the chain: the ticker really re-issues the claim, and a claimed run
// gets a watcher that takes the lease (so the next replica leaves it alone).
func TestRunWatcherSweeper_PeriodicClaimAdoptsAndLeases(t *testing.T) {
	h := newHarness(t)
	run := execRun(t, "agent-exec-id")
	fake := &bootReconcileStore{run: run, beats: make(chan uuid.UUID, 8)}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{} // AgentStatus: RUNNING — adopt, do not finalize
	cfg.Broker = h.broker
	ctx := bootTestCtx(t)
	cfg.BaseCtx = ctx
	srv := New(cfg)

	go srv.runWatcherSweeper(ctx, 10*time.Millisecond)

	select {
	case got := <-fake.beats:
		if got != run.ID {
			t.Fatalf("lease taken on %s, want the claimed run (%s)", got, run.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the periodic sweeper must claim the stale lease and hand the run to a watcher that holds it")
	}
	if n := fake.claimCalls(); n == 0 {
		t.Error("the sweeper ticker never issued a claim")
	}
	if to, got := fake.finalTransition(); got {
		t.Errorf("a claimed run whose agent is still RUNNING must be re-watched, not finalized %q", to)
	}
}

// TestRunWatcherSweeper_PeriodicallyReapsUndispatchedOrphans covers the crash
// window the age gate opens: wardynd dies while a run is mid-build, and the
// restart 15s later finds it too young to touch. Nothing else can ever reap it —
// the watcher sweep requires a non-empty sandbox_ref and the idle reaper only
// lists RUNNING — so with a boot-only pass that run sits PENDING forever, holding a
// minted run token and un-revoked grants, until a human notices. The pass must
// therefore keep running on this process's ticker, not only at its boot.
func TestRunWatcherSweeper_PeriodicallyReapsUndispatchedOrphans(t *testing.T) {
	h := newHarness(t)
	run := execRun(t, "")
	run.SandboxRef = ""                                                    // died before dispatch
	run.CreatedAt = time.Now().UTC().Add(-undispatchedGrace - time.Minute) // and long past any build window
	fake := &bootReconcileStore{run: run, finals: make(chan types.RunState, 4)}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	ctx := bootTestCtx(t)
	cfg.BaseCtx = ctx
	srv := New(cfg)

	// Deliberately NOT ReconcileOnBoot: the periodic tick alone has to reap it.
	go srv.runWatcherSweeper(ctx, 10*time.Millisecond)

	select {
	case to := <-fake.finals:
		if to != types.RunFailed {
			t.Fatalf("an abandoned pre-dispatch run must be reaped FAILED, got %q", to)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper never reaped a sandbox-less run past undispatchedGrace; boot-only leaves it stranded non-terminal with un-revoked credentials for the life of the pod")
	}
	// #123: the reaped run must carry a failure hint, not a blank chip — the
	// hint write races the sweeper goroutine (it lands AFTER the finals send),
	// so poll briefly instead of reading it the instant finals fires.
	deadline := time.Now().Add(2 * time.Second)
	for fake.hintValue() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.hintValue() == "" {
		t.Error("a reaped never-dispatched run must get a failure hint so its FAILED badge carries a reason (#123)")
	}
}

// TestSweepRunWatchers_NeverExecdRunFinalizesWithoutAgeGate pins GAP-RECONCILE-2's
// close: a claimed non-interactive TASK run with a sandbox_ref but NO agent_exec_id
// (a crash between SetSandboxRef and Exec) must be finalized FAILED — NOT re-watched
// — even when it is YOUNG (well inside undispatchedGrace). Its dispatcher held the
// watcher lease continuously, so a stale-lease claim proves the dispatcher is gone
// and no agent will ever be exec'd; probing the idle sleep-infinity container would
// read RUNNING forever and strand it. Counterfactual: with the old age>grace gate a
// run this young fell through to reconcileWatch and stranded (transitioned stays
// false).
func TestSweepRunWatchers_NeverExecdRunFinalizesWithoutAgeGate(t *testing.T) {
	h := newHarness(t)
	run := execRun(t, "") // sandbox_ref set, agent_exec_id ""
	run.Task = "do the thing"
	run.CreatedAt = time.Now().UTC() // YOUNG — well inside undispatchedGrace
	fake := &bootReconcileStore{run: run}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{} // AgentStatus would report RUNNING — must not be consulted
	cfg.Broker = h.broker
	cfg.BaseCtx = bootTestCtx(t)
	srv := New(cfg)

	if err := srv.sweepRunWatchers(context.Background()); err != nil {
		t.Fatalf("sweepRunWatchers: %v", err)
	}
	to, got := fake.finalTransition()
	if !got || to != types.RunFailed {
		t.Fatalf("a never-exec'd task run must finalize FAILED regardless of age; transitioned=%v to=%q", got, to)
	}
}

// mainProcessRunner models a healthy EXEC-LESS (krun) launch: Exec succeeds
// but returns "" — there is no separate exec, the workload IS the container's
// main process — and AgentStatus reports the container's real liveness
// regardless of which exec id it is asked about, mirroring the docker
// driver's fallback to Status for "" or the mainProcessExecID sentinel
// (driver.go). probed records every execID AgentStatus was called with, so a
// test can prove the guard actually consulted the runner instead of
// short-circuiting to a kill.
type mainProcessRunner struct {
	*fakeRunner
	probed chan string
}

func (r *mainProcessRunner) Exec(context.Context, string, []string) (string, error) {
	return "", nil
}

func (r *mainProcessRunner) AgentStatus(_ context.Context, _, execID string) (runner.Status, error) {
	select {
	case r.probed <- execID:
	default:
	}
	return runner.Status{State: types.RunRunning}, nil
}

// TestSweepRunWatchers_ExecLessRunNotFinalized: a healthy exec-less
// (krun/CC3) launch has Runner.Exec return "" with no error — dispatch must
// not let that collide with the strand guard's "never exec'd" signal. This
// drives the real path end to end — startAgentOrIdle persists whatever
// dispatch decides via SetRunAgentExecID, then sweepRunWatchers reads that
// same persisted value back — rather than hand-setting AgentExecID, so it
// exercises the value dispatch persists (runs_dispatch.go's
// mainProcessExecID sentinel), not merely the guard's "== \"\"" condition
// in isolation.
//
// Counterfactual: if startAgentOrIdle persisted the bare "" Exec returned,
// the strand guard would finalize FAILED + tear down any non-interactive
// task run with AgentExecID=="" without ever probing the runner — killing
// this healthy run outright (transitioned=true, to=FAILED), which is
// exactly what this test must catch red.
func TestSweepRunWatchers_ExecLessRunNotFinalized(t *testing.T) {
	h := newHarness(t)
	run := execRun(t, "") // overwritten by startAgentOrIdle below, as in real dispatch
	run.Task = "do the thing"
	rn := &mainProcessRunner{fakeRunner: &fakeRunner{}, probed: make(chan string, 4)}
	fake := &bootReconcileStore{run: run}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rn
	cfg.Broker = h.broker
	cfg.BaseCtx = bootTestCtx(t)
	srv := New(cfg)

	// The dispatch phase under test: persists the real post-Exec value for an
	// exec-less launch, exactly as it does mid-dispatchRun.
	srv.startAgentOrIdle(context.Background(), run, run.SandboxRef, "wardyn/claude-code:latest", false)

	if err := srv.sweepRunWatchers(context.Background()); err != nil {
		t.Fatalf("sweepRunWatchers: %v", err)
	}
	if to, got := fake.finalTransition(); got {
		t.Fatalf("a healthy exec-less run must not be finalized; finalized=%v to=%q — the strand guard treated a legitimate exec-less launch as never-exec'd", got, to)
	}
	select {
	case <-rn.probed:
	default:
		t.Error("the guard must fall through to an AgentStatus probe for an exec-less run's persisted value, not short-circuit straight to a kill")
	}
}

var _ runner.Runner = (*execExitRunner)(nil)
var _ runner.Runner = (*errProbeRunner)(nil)
var _ runner.Runner = (*mainProcessRunner)(nil)

// sweepableImageBuilder implements both ImageBuilder and the optional
// ImageBuildSweeper capability, so ReconcileOnBoot's type assertion finds it
// — the wiring under test. The three ImageBuilder methods are never called by
// ReconcileOnBoot; they exist only so this type satisfies the field's type.
type sweepableImageBuilder struct {
	mu    sync.Mutex
	swept int
}

func (*sweepableImageBuilder) BuildDevcontainer(context.Context, string, string, string, io.Writer) (string, error) {
	return "", errors.New("not implemented")
}
func (*sweepableImageBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string, io.Writer) (string, error) {
	return "", errors.New("not implemented")
}
func (*sweepableImageBuilder) FinalizeBase(context.Context, string, string, io.Writer) (string, error) {
	return "", errors.New("not implemented")
}
func (s *sweepableImageBuilder) SweepOrphanedBuilds(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.swept++
	return nil
}

func (s *sweepableImageBuilder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.swept
}

// TestReconcileOnBoot_SweepsOrphanedBuildsIndependentOfRunner pins the third
// leg of ReconcileOnBoot (see its doc comment): an ImageBuilder implementing
// the optional ImageBuildSweeper capability is swept EVEN with no Runner
// configured — an image builder can be wired standalone, and unlike the two
// run reapers this leg must not short-circuit on s.cfg.Runner == nil.
func TestReconcileOnBoot_SweepsOrphanedBuildsIndependentOfRunner(t *testing.T) {
	sweeper := &sweepableImageBuilder{}
	srv := &Server{cfg: Config{ImageBuilder: sweeper}}
	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}
	if sweeper.count() != 1 {
		t.Fatalf("SweepOrphanedBuilds called %d times, want 1", sweeper.count())
	}
}

// TestOrphanedBuildSweeper_RunsOnCadence pins the fix for the boot-ONLY sweep:
// the sweep's own safety gate is an AGE gate (2*BuildTimeout), so under a
// supervised restart the orphan is always too young at the one moment the boot
// pass looks — and its writable layer would leak until the next boot. The sweep
// therefore has to keep running.
func TestOrphanedBuildSweeper_RunsOnCadence(t *testing.T) {
	sweeper := &sweepableImageBuilder{}
	srv := &Server{cfg: Config{ImageBuilder: sweeper}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.orphanedBuildSweeper(ctx, time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for sweeper.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := sweeper.count(); got < 3 {
		t.Fatalf("SweepOrphanedBuilds ran %d times in 5s at a 1ms cadence, want >= 3 (the sweep is still boot-only)", got)
	}
	cancel()
	// Cancellation must actually stop it — this goroutine lives for the life of
	// the daemon, so a ctx it ignores would outlive every test that starts one.
	time.Sleep(20 * time.Millisecond)
	stopped := sweeper.count()
	time.Sleep(20 * time.Millisecond)
	if after := sweeper.count(); after != stopped {
		t.Errorf("sweeper kept running after ctx cancel: %d -> %d", stopped, after)
	}
}

// TestReconcileOnBoot_ImageBuilderWithoutSweepCapabilityIsNoop asserts an
// ImageBuilder that doesn't implement ImageBuildSweeper (a plain fake, or any
// future non-docker target) never panics or errors ReconcileOnBoot — the
// capability is genuinely optional.
func TestReconcileOnBoot_ImageBuilderWithoutSweepCapabilityIsNoop(t *testing.T) {
	srv := &Server{cfg: Config{ImageBuilder: fakeImageBuilder{}}}
	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}
}

// TestDispatchExecIDWriteLostFailsTheRunLoudly is correctness-1/c5.
//
// SetRunAgentExecID was `_ =`'d — "best-effort, like SetSandboxRef". It is not
// like SetSandboxRef: reconcile.go's strand guard RESERVES the resulting "" for
// "the dispatcher died before it ever exec'd the agent", so a lost write makes a
// healthy, running agent indistinguishable from a corpse. The next boot
// finalizes it FAILED and tears the sandbox down — while the run.exec audit row
// this dispatch wrote said `success`. The trail disagreed with the outcome, and
// the only record of the disagreement was nothing at all.
//
// So: one retry (the write is idempotent; the failure this sees is a connection
// blip), then fail the run HERE, through the dispatch-failure path the Exec-error
// arm fifteen lines above already uses — the sandbox stopped, the run FAILED with
// a hint naming the write, and the audit row saying `failure`.
func TestDispatchExecIDWriteLostFailsTheRunLoudly(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness(t)
	run := execRun(t, "")
	run.Task = "do the thing"
	fr := &fakeRunner{}
	fake := &bootReconcileStore{run: run, setExecIDErr: errors.New("pool exhausted")}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = fr
	cfg.Broker = h.broker
	cfg.BaseCtx = bootTestCtx(t)
	srv := New(cfg)

	srv.startAgentOrIdle(context.Background(), run, run.SandboxRef, "wardyn/claude-code:latest", false)

	// Retried exactly once before giving up — a single attempt turns a blip into
	// a dead run, and an unbounded loop holds the dispatcher open forever.
	if fake.setExecIDCalls != 2 {
		t.Errorf("SetRunAgentExecID attempts = %d, want 2 (the write plus one retry)", fake.setExecIDCalls)
	}
	// FAILED, not left running for the reconciler to misread hours later.
	if to, got := fake.finalTransition(); !got || to != types.RunFailed {
		t.Fatalf("run state written = (%q, applied=%v), want FAILED — a run whose exec id was lost must not be left looking healthy", to, got)
	}
	fake.mu.Lock()
	hint := fake.hint
	fake.mu.Unlock()
	if !strings.Contains(hint, "exec id") {
		t.Errorf("failure hint = %q, want it to name the persisted-exec-id write that could not be made", hint)
	}
	// The audit row says FAILURE and carries the store error — the row that used
	// to say `success` is the one an operator reads when the reconciler kills the
	// run later.
	var execRows int
	for _, ev := range h.audit.events {
		if ev.Action != "run.exec" {
			continue
		}
		execRows++
		if ev.Outcome != "failure" {
			t.Errorf("run.exec outcome = %q, want failure", ev.Outcome)
		}
		if !strings.Contains(string(ev.Data), "pool exhausted") {
			t.Errorf("run.exec data = %s, want the store error", ev.Data)
		}
	}
	if execRows != 1 {
		t.Errorf("run.exec rows = %d, want exactly 1 (the failure)", execRows)
	}
	if !strings.Contains(buf.String(), "exec id") {
		t.Errorf("log = %q, want an error line naming the lost exec-id write", buf.String())
	}
}

// TestDispatchExecIDWritePersistsOnTheHappyPath is the other half: nothing above
// changes the ordinary launch — one write, no retry, the `success` row, and the
// run left RUNNING.
func TestDispatchExecIDWritePersistsOnTheHappyPath(t *testing.T) {
	h := newHarness(t)
	run := execRun(t, "")
	run.Task = "do the thing"
	fake := &bootReconcileStore{run: run}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	cfg.BaseCtx = bootTestCtx(t)
	srv := New(cfg)

	srv.startAgentOrIdle(context.Background(), run, run.SandboxRef, "wardyn/claude-code:latest", false)

	if fake.setExecIDCalls != 1 {
		t.Errorf("SetRunAgentExecID attempts = %d, want 1 on the happy path", fake.setExecIDCalls)
	}
	fake.mu.Lock()
	persisted := fake.run.AgentExecID
	fake.mu.Unlock()
	if persisted != "fake-exec-id" {
		t.Errorf("persisted exec id = %q, want the value Exec returned", persisted)
	}
	if to, got := fake.finalTransition(); got {
		t.Fatalf("a healthy launch was finalized %q", to)
	}
	for _, ev := range h.audit.events {
		if ev.Action == "run.exec" && ev.Outcome != "success" {
			t.Errorf("run.exec outcome = %q on the happy path, want success", ev.Outcome)
		}
	}
}

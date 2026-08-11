// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
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

// TestReconcileOnBoot_ExecRunFinalizesFromAgentExit is the regression:
// after a restart, a RUNNING exec-based run whose agent has exited (but whose idle
// sandbox container is still up) MUST finalize + revoke + tear down, not strand.
// The reconciler now observes AgentStatus (the persisted exec id) instead of
// container Status, which for an idle `sleep infinity` reports RUNNING forever.
// Counterfactual: the runner's container Status IS RUNNING here — with the old
// code the run is re-attached and never finalized, so `transitioned` stays false.
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
}

// errProbeRunner's AgentStatus always errors — a persistent docker-daemon blip,
// NOT a terminal state.
type errProbeRunner struct{ *fakeRunner }

func (r *errProbeRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{}, errors.New("docker: daemon unreachable")
}

// TestReconcileOnBoot_TransientProbeErrorDoesNotFinalize is the runs-fsm regression
// the completed crown review surfaced: a transient AgentStatus error at boot must
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

// TestRunWatcherSweeper_PeriodicallyReapsUndispatchedOrphans is the crash-window
// regression the age gate opened: wardynd dies while a run is mid-build, and the
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

var _ runner.Runner = (*execExitRunner)(nil)
var _ runner.Runner = (*errProbeRunner)(nil)

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

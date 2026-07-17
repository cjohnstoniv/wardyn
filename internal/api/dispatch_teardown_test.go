// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file pins the TEARDOWN-HONESTY invariant on dispatch's compensation
// sites, the one reconcile.go states verbatim for reconcileFinalize: a swallowed
// StopSandbox error leaves a live/routable container that the next boot SKIPS
// (the run is terminal by then ⇒ ReconcileOnBoot's isTerminalRunState guard
// drops it), abandoning it — and the proxy sidecar holding the resolved
// injection credential VALUES — forever with no record. reconcile_test.go
// asserts it for the reconciler; these assert it for the three dispatch sites,
// which used to `_ = s.cfg.Runner.StopSandbox(...)` and, at the kill-race site,
// audit "sandbox torn down" whether or not that had happened.

// dispatchTestStore is an in-memory Store with just the surface dispatch drives:
// one run, one atomic state cell. Deliberately NOT the PG harness — this
// invariant must be provable without WARDYN_TEST_PG.
type dispatchTestStore struct {
	store.Store
	mu    sync.Mutex
	run   types.AgentRun
	state types.RunState
}

func (s *dispatchTestStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.run
	r.State = s.state
	return r, nil
}

func (s *dispatchTestStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return false, nil
	}
	s.state = to
	return true, nil
}

func (s *dispatchTestStore) State() types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *dispatchTestStore) SetSandboxRef(context.Context, uuid.UUID, string) error { return nil }
func (s *dispatchTestStore) SetRunAgentExecID(context.Context, uuid.UUID, string) error {
	return nil
}
func (s *dispatchTestStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

// killRaceRunner models the kill-race: the STARTING->RUNNING CAS will lose (the
// store cell has already moved on), and the compensating teardown FAILS — the
// exact shape that abandons a live sandbox. onCreate runs inside CreateSandbox,
// the window a real kill lands in (an image pull takes minutes).
type killRaceRunner struct {
	*fakeRunner
	mu       sync.Mutex
	stopErr  error
	stops    int
	onCreate func()
}

func (r *killRaceRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	if r.onCreate != nil {
		r.onCreate()
	}
	return r.fakeRunner.CreateSandbox(ctx, spec)
}

func (r *killRaceRunner) StopSandbox(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops++
	return r.stopErr
}

func (r *killRaceRunner) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops
}

// execFailRunner fails the agent Exec (dispatch's other compensation site) and
// fails the teardown that follows.
type execFailRunner struct {
	*killRaceRunner
	execErr error
}

func (r *execFailRunner) Exec(context.Context, string, []string) (string, error) {
	return "", r.execErr
}

// dispatchTeardownFixture wires a Server whose store's state cell starts at
// `start` and whose runner is `rn`, then returns the run dispatch will drive.
func dispatchTeardownFixture(t *testing.T, rn runner.Runner, start types.RunState) (*Server, *dispatchTestStore, *recRecorder, types.AgentRun) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunPending,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &dispatchTestStore{run: run, state: start}
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = h.broker
	cfg.Audit = audit
	return New(cfg), st, audit, run
}

// findAudit returns the last event matching action+outcome for runID.
func findAudit(events []types.AuditEvent, runID uuid.UUID, action, outcome string) *types.AuditEvent {
	var hit *types.AuditEvent
	for i := range events {
		ev := &events[i]
		if ev.Action == action && ev.Outcome == outcome && ev.RunID != nil && *ev.RunID == runID {
			hit = ev
		}
	}
	return hit
}

// findTeardownError returns the run's event carrying a teardown_error, if any.
// The teardown outcome is its own fact, audited under the phase's action
// (run.dispatch / run.selftest / run.exec) — it is NOT necessarily the same
// event as the phase's own failure report.
func findTeardownError(events []types.AuditEvent, runID uuid.UUID) *types.AuditEvent {
	for i := range events {
		ev := &events[i]
		if ev.RunID != nil && *ev.RunID == runID && strings.Contains(string(ev.Data), "teardown_error") {
			return ev
		}
	}
	return nil
}

// TestDispatch_KillRaceTeardownFailure_AuditsTeardownError is the counterfactual
// for dispatch's kill-race compensation site: a kill wins the run mid-
// CreateSandbox, so dispatch tears the sandbox it just created back down — and
// when THAT fails, the orphan must be recorded. The run is KILLED (terminal) by
// then, so ReconcileOnBoot will never revisit it: this audit event is the only
// record that a live container + proxy holding injected credentials still exists.
// Counterfactual: with `_ = s.cfg.Runner.StopSandbox(...)` the error is
// discarded and no failure event carries teardown_error.
func TestDispatch_KillRaceTeardownFailure_AuditsTeardownError(t *testing.T) {
	rn := &killRaceRunner{fakeRunner: &fakeRunner{}, stopErr: errors.New("docker: daemon unreachable")}
	// Start PENDING so dispatch's entry claim wins, then flip the cell to KILLED
	// behind its back via the runner's CreateSandbox — modelling the kill that
	// lands while the image pulls.
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	rn.onCreate = func() {
		st.mu.Lock()
		st.state = types.RunKilled
		st.mu.Unlock()
	}

	srv.dispatch(context.Background(), run, "run-token", "wardyn/claude-code:latest",
		types.RunPolicySpec{MinConfinementClass: types.CC1}, nil, nil, nil, nil, false, "")

	if rn.stopCount() != 1 {
		t.Fatalf("dispatch must tear the orphaned sandbox down exactly once, got %d stops", rn.stopCount())
	}
	if got := st.State(); got != types.RunKilled {
		t.Fatalf("dispatch must not resurrect the killed run; state = %q, want KILLED", got)
	}
	if findAudit(audit.events, run.ID, "run.dispatch", "failure") == nil {
		t.Fatal("no run.dispatch/failure event for the aborted dispatch")
	}
	ev := findTeardownError(audit.events, run.ID)
	if ev == nil {
		t.Fatalf("a failed compensating teardown must be audited with teardown_error — the run is terminal, so no future boot reconciles this live sandbox; events=%s", auditDump(audit.events, run.ID))
	}
	if ev.Action != "run.dispatch" || ev.Outcome != "failure" {
		t.Errorf("teardown_error must be audited under the phase's action as a failure, got %s/%s", ev.Action, ev.Outcome)
	}
	if !strings.Contains(string(ev.Data), "daemon unreachable") {
		t.Errorf("the teardown_error must carry the real error text, got %s", ev.Data)
	}
	// HONESTY: never assert a containment step that was not observed.
	for i := range audit.events {
		ev := &audit.events[i]
		if ev.RunID != nil && *ev.RunID == run.ID && strings.Contains(string(ev.Data), "sandbox torn down") {
			t.Errorf("audit asserts %q while the teardown actually FAILED: %s", "sandbox torn down", ev.Data)
		}
	}
}

// TestDispatch_ExecFailureTeardownFailure_AuditsTeardownError is the same
// counterfactual at dispatch's exec-failure site: the run.exec/failure event
// used to carry only the Exec error, never the teardown error, so a sandbox
// orphaned on this path vanished from the system of record.
func TestDispatch_ExecFailureTeardownFailure_AuditsTeardownError(t *testing.T) {
	rn := &execFailRunner{
		killRaceRunner: &killRaceRunner{fakeRunner: &fakeRunner{}, stopErr: errors.New("docker: no such container")},
		execErr:        errors.New("docker: exec create: OCI runtime error"),
	}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)

	srv.dispatch(context.Background(), run, "run-token", "wardyn/claude-code:latest",
		types.RunPolicySpec{MinConfinementClass: types.CC1}, nil, nil, nil, nil, false, "")

	if rn.stopCount() != 1 {
		t.Fatalf("a failed Exec must tear the sandbox down exactly once, got %d stops", rn.stopCount())
	}
	if got := st.State(); got != types.RunFailed {
		t.Fatalf("a failed Exec must land the run FAILED, got %q", got)
	}
	// The Exec error and the teardown error are different facts; both must survive.
	if ev := findAudit(audit.events, run.ID, "run.exec", "failure"); ev == nil {
		t.Fatal("no run.exec/failure event emitted")
	}
	ev := findTeardownError(audit.events, run.ID)
	if ev == nil {
		t.Fatalf("a failed teardown on the exec-failure path must be audited with teardown_error; the run is FAILED (terminal) so no boot reconciles the live sandbox — events=%s", auditDump(audit.events, run.ID))
	}
	if !strings.Contains(string(ev.Data), "no such container") {
		t.Errorf("the teardown_error must carry the real error text, got %s", ev.Data)
	}
}

// waitTransientRunner models the docker blip that motivates the Wait-error
// handoff: Runner.Wait errors out (Driver.Wait gives up on the FIRST ExecInspect
// error), while the agent itself has really exited — which only an AgentStatus
// probe can see. The container is still up, so nothing else would ever tear it
// down.
type waitTransientRunner struct {
	*killRaceRunner
	waitErr   error
	agentExit int
}

func (r *waitTransientRunner) Wait(context.Context, string) (int, error) { return 0, r.waitErr }

func (r *waitTransientRunner) AgentStatus(_ context.Context, _, execID string) (runner.Status, error) {
	if execID == "" {
		return runner.Status{State: types.RunRunning}, nil
	}
	code := r.agentExit
	return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
}

// TestCompletionWatcher_TransientWaitError_FinalizesViaHandoff is the runs-fsm
// regression at the PRIMARY watcher — the one that runs for 100% of normal
// dispatches, unlike reconcileWatch which only runs after a daemon restart. A
// Wait error that is NOT the daemon shutting down is a probe failure, not "the
// run finished": this is the run's only watcher (one per dispatch, never
// respawned), so returning silently strands a RUNNING run with a live sandbox
// and un-revoked credentials, and the idle reaper is disabled on 6 of the 7
// shipped policies. The watcher now hands off to reconcileWatch, which tolerates
// bounded probe errors and then finalizes + revokes + tears down.
// Counterfactual: with the bare `return` on werr != nil the run stays RUNNING,
// the broker never revokes, and the sandbox is never stopped.
func TestCompletionWatcher_TransientWaitError_FinalizesViaHandoff(t *testing.T) {
	rn := &waitTransientRunner{
		killRaceRunner: &killRaceRunner{fakeRunner: &fakeRunner{}},
		waitErr:        errors.New("docker: wait: exec inspect: EOF"),
		agentExit:      0,
	}
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunPending,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &dispatchTestStore{run: run, state: types.RunPending}
	brk := &raceBroker{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = brk
	cfg.Audit = &raceAudit{} // the watcher audits from its goroutine
	// A LIVE base ctx: this is the daemon running normally, NOT shutting down —
	// the discriminator the handoff keys off.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	cfg.BaseCtx = baseCtx
	srv := New(cfg)

	srv.dispatch(context.Background(), run, "run-token", "wardyn/claude-code:latest",
		types.RunPolicySpec{MinConfinementClass: types.CC1}, nil, nil, nil, nil, false, "")

	// The watcher is detached; reconcileWatch probes on a 5s tick. Wait for the
	// WHOLE finalize, not just the state flip: the cascade wins the terminal CAS
	// FIRST and revokes after (deliberately — C002), so a poll that stops at
	// "terminal" can observe the run finalized microseconds before its revoke
	// lands and read revocations=0. Wait for both, then assert exactly-once.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) &&
		!(isTerminalRunState(st.State()) && brk.revocations(runID) > 0) {
		time.Sleep(20 * time.Millisecond)
	}

	if got := st.State(); got != types.RunCompleted {
		t.Fatalf("a transient Wait error must NOT abandon the run: the agent exited 0, so it must finalize COMPLETED via the AgentStatus handoff; state = %q", got)
	}
	if revs := brk.revocations(runID); revs != 1 {
		t.Errorf("the finalized run's credentials must be revoked exactly once (cascade-on-every-stop); got %d", revs)
	}
	if rn.stopCount() == 0 {
		t.Error("finalize must tear the still-up sandbox down — nothing else ever will (the run is terminal, so no boot reconciles it)")
	}
}

// auditDump renders a run's events for a failure message.
func auditDump(events []types.AuditEvent, runID uuid.UUID) string {
	var b strings.Builder
	for i := range events {
		ev := &events[i]
		if ev.RunID != nil && *ev.RunID == runID {
			b.WriteString(ev.Action + "/" + ev.Outcome + " " + string(ev.Data) + "; ")
		}
	}
	return b.String()
}

var (
	_ runner.Runner = (*killRaceRunner)(nil)
	_ runner.Runner = (*execFailRunner)(nil)
	_ runner.Runner = (*waitTransientRunner)(nil)
)

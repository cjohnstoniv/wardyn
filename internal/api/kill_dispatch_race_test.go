// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file is the CONCURRENT companion to revoke_order_test.go (U017): those
// tests pin the C002/C003 semantics for a CAS whose outcome is already decided;
// these actually race a kill against a dispatch forward-transition on one run
// under `go test -race`, asserting exactly one writer wins the state cell and
// the loser performs no revocation. It is the safety net required before any
// structural change to runs.go's dispatch/kill code.

// raceStore is a minimal race-safe Store: one run, one genuinely atomic
// compare-and-set state cell.
type raceStore struct {
	store.Store
	mu    sync.Mutex
	run   types.AgentRun
	state types.RunState
}

func (s *raceStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.run
	r.State = s.state
	return r, nil
}

func (s *raceStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return false, nil
	}
	s.state = to
	return true, nil
}

func (s *raceStore) State() types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// raceBroker records revocations race-safely.
type raceBroker struct {
	fakeBroker
	mu sync.Mutex
	n  map[uuid.UUID]int
}

func (b *raceBroker) RevokeRun(_ context.Context, runID uuid.UUID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == nil {
		b.n = map[uuid.UUID]int{}
	}
	b.n[runID]++
	return nil
}

func (b *raceBroker) revocations(runID uuid.UUID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n[runID]
}

// raceAudit is a race-safe no-op audit recorder (the harness recRecorder
// appends without a lock and would itself trip the race detector).
type raceAudit struct{ mu sync.Mutex }

func (r *raceAudit) Record(context.Context, types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return nil
}

// TestKillRun_DispatchForwardRace_ExactlyOneWinner races handleKillRun against
// the dispatch forward-transition (PENDING→STARTING) on the same run, repeatedly.
// Legal serializations: kill owns PENDING first (forward loses), forward moves
// first and the kill then legitimately kills the STARTING run (both "win"), or
// forward moves between the kill's read and its CAS (kill 409s). The invariants,
// every iteration:
//   - kill reported a win (202/500) → final state KILLED, ≥1 revocation;
//   - kill reported 409 → forward won, final state STARTING, ZERO revocations
//     (C002: a losing kill must never strip a still-live run's credentials).
//
// Run under -race this also proves the handler path itself is data-race-free.
func TestKillRun_DispatchForwardRace_ExactlyOneWinner(t *testing.T) {
	h := newHarness(t)
	const rounds = 200
	for i := 0; i < rounds; i++ {
		runID := uuid.New()
		st := &raceStore{
			run:   types.AgentRun{ID: runID},
			state: types.RunPending,
		}
		brk := &raceBroker{}
		cfg := baseTestConfig(h, st)
		cfg.Broker = brk
		cfg.Audit = &raceAudit{}
		srv := New(cfg)

		start := make(chan struct{})
		var (
			wg         sync.WaitGroup
			killCode   int
			forwardWon bool
		)
		wg.Add(2)
		go func() { // the kill switch
			defer wg.Done()
			<-start
			w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
			killCode = w.Code
		}()
		go func() { // dispatch's forward-transition, as dispatch performs it
			defer wg.Done()
			<-start
			forwardWon, _ = st.UpdateRunStateIf(context.Background(), runID, types.RunPending, types.RunStarting)
		}()
		close(start)
		wg.Wait()

		// 202 = clean kill; 500 = honest partial-teardown report — both OWN the
		// KILLED transition. Only 409 is a loss.
		killWon := killCode == http.StatusAccepted || killCode == http.StatusInternalServerError
		revs := brk.revocations(runID)
		final := st.State()

		if killWon {
			if final != types.RunKilled {
				t.Fatalf("round %d: kill won but final state=%s", i, final)
			}
			if revs < 1 {
				t.Fatalf("round %d: kill won the CAS but never revoked", i)
			}
		} else {
			if killCode != http.StatusConflict {
				t.Fatalf("round %d: losing kill must 409, got %d", i, killCode)
			}
			if !forwardWon {
				t.Fatalf("round %d: kill 409'd but forward also lost — nobody moved the run; final=%s", i, final)
			}
			if revs != 0 {
				t.Fatalf("round %d: losing kill revoked a still-live run's credentials (C002); revocations=%d", i, revs)
			}
			if final != types.RunStarting {
				t.Fatalf("round %d: forward won but final state=%s", i, final)
			}
		}
	}
}

// TestKillRun_FailAndRevokeRace_SingleRevocation races handleKillRun against
// failAndRevoke (a create/dispatch failure path, C003) — two TERMINAL writers.
// Exactly one may win, and the run's credentials are revoked exactly once.
func TestKillRun_FailAndRevokeRace_SingleRevocation(t *testing.T) {
	h := newHarness(t)
	const rounds = 200
	for i := 0; i < rounds; i++ {
		runID := uuid.New()
		st := &raceStore{
			run:   types.AgentRun{ID: runID},
			state: types.RunPending,
		}
		brk := &raceBroker{}
		cfg := baseTestConfig(h, st)
		cfg.Broker = brk
		cfg.Audit = &raceAudit{}
		srv := New(cfg)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			srv.failAndRevoke(context.Background(), runID, types.RunPending)
		}()
		close(start)
		wg.Wait()

		final := st.State()
		if final != types.RunKilled && final != types.RunFailed {
			t.Fatalf("round %d: final state must be a terminal winner's, got %s", i, final)
		}
		if revs := brk.revocations(runID); revs != 1 {
			t.Fatalf("round %d: two racing terminal writers must revoke exactly once, got %d (final=%s)", i, revs, final)
		}
	}
}

// ─── real-dispatch-seam kill race (X079/X093) ────────────────────────────────
//
// The two tests above race a kill against a raw state-cell CAS — they prove the
// CAS arbitration but not the DISPATCH SEAM: CreateSandbox provisions a real
// sandbox, SetSandboxRef persists its ref, and the STARTING->RUNNING CAS gates
// whether the run boots or is compensated. The fixtures also carried an empty
// SandboxRef, so handleKillRun's `run.SandboxRef != ""` guard skipped
// KillSandbox entirely — the teardown half of the cascade never executed. The
// tests below drive the REAL s.dispatch concurrently with the REAL handleKillRun
// over a store that actually persists the sandbox ref, so a racing kill sees the
// ref and its KillSandbox teardown fires.

// raceDispatchStore is a race-safe Store with the full surface s.dispatch drives
// AND a real sandbox-ref cell (the gap X093 names: dispatchTestStore's
// SetSandboxRef is a no-op, so its GetRun always returns an empty ref and kill's
// KillSandbox is dead code). refSet/killGate are the optional coordination for
// the deterministic interleave test — nil in the genuine-race test.
type raceDispatchStore struct {
	store.Store
	mu    sync.Mutex
	id    uuid.UUID
	state types.RunState
	ref   string
	// When non-nil, SetSandboxRef signals refSet after persisting the ref, then
	// blocks on killGate — letting a test land a kill in the exact in-flight
	// window (STARTING + sandbox created + ref persisted, RUNNING CAS not yet run)
	// that a real kill hits during a minutes-long image pull.
	refSet   chan struct{}
	killGate chan struct{}
}

func (s *raceDispatchStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return types.AgentRun{
		ID: s.id, State: s.state, SandboxRef: s.ref,
		Agent: "claude-code", CreatedBy: "t@example.com", ConfinementClass: types.CC1,
	}, nil
}

func (s *raceDispatchStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return false, nil
	}
	s.state = to
	return true, nil
}

func (s *raceDispatchStore) SetSandboxRef(_ context.Context, _ uuid.UUID, ref string) error {
	s.mu.Lock()
	s.ref = ref
	s.mu.Unlock()
	// Block INSIDE the STARTING window (lock released first, or a concurrent
	// kill's GetRun would deadlock) until the test lands its kill.
	if s.refSet != nil {
		close(s.refSet)
		<-s.killGate
	}
	return nil
}

func (s *raceDispatchStore) SetRunAgentExecID(context.Context, uuid.UUID, string) error { return nil }
func (s *raceDispatchStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

func (s *raceDispatchStore) State() types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// teardownCountRunner counts the two teardown seams independently: KillSandbox
// (the kill switch's half) and StopSandbox (dispatch's compensation when its
// RUNNING CAS loses). CreateSandbox is counted too, so an assertion can tell
// "kill won before any sandbox existed" (nothing to tear down) from "kill won
// with a live sandbox" (must tear down). It reuses fakeRunner for the real
// CreateSandbox ref shape and the ctx-honouring Wait.
type teardownCountRunner struct {
	*fakeRunner
	creates atomic.Int32
	stops   atomic.Int32
	kills   atomic.Int32
}

func (r *teardownCountRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	r.creates.Add(1)
	return r.fakeRunner.CreateSandbox(ctx, spec)
}
func (r *teardownCountRunner) StopSandbox(context.Context, string) error { r.stops.Add(1); return nil }
func (r *teardownCountRunner) KillSandbox(context.Context, string) error { r.kills.Add(1); return nil }

var _ runner.Runner = (*teardownCountRunner)(nil)

// dispatchRun is the PENDING run every seam test feeds to s.dispatch. Task is
// deliberately empty: a non-interactive run with no task sets RUNNING and
// returns WITHOUT execing an agent or starting a completion watcher, so a
// dispatch that wins RUNNING leaves no detached goroutine to leak — the
// kill-vs-dispatch teardown seam under test (CreateSandbox/SetSandboxRef/RUNNING
// CAS/teardown) is identical with or without a task.
// ponytail: Task="" avoids the watcher goroutine; the teardown seam is task-independent.
func dispatchRun(runID uuid.UUID) types.AgentRun {
	return types.AgentRun{
		ID: runID, Agent: "claude-code", CreatedBy: "t@example.com",
		ConfinementClass: types.CC1, State: types.RunPending,
	}
}

func runDispatch(srv *Server, run types.AgentRun) {
	srv.dispatchRun(context.Background(), run, dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
}

// TestKillRun_DispatchSandboxInFlight_TeardownOrdering is the DETERMINISTIC
// counterfactual for the seam X093 says goes untested: a kill lands while a
// dispatch has a real sandbox in flight (created + ref persisted, STARTING, not
// yet RUNNING). It pins the exact teardown ORDERING — kill wins the KILLED CAS,
// tears the sandbox down via KillSandbox, and revokes; dispatch's now-losing
// STARTING->RUNNING CAS then compensates the sandbox it created via StopSandbox.
// Both teardown halves execute exactly once, the run never resurrects to RUNNING,
// and credentials revoke exactly once. Reverting handleKillRun's SandboxRef guard
// or dispatch's compensating teardown flips one of the counts.
func TestKillRun_DispatchSandboxInFlight_TeardownOrdering(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &raceDispatchStore{
		id: runID, state: types.RunPending,
		refSet: make(chan struct{}), killGate: make(chan struct{}),
	}
	rn := &teardownCountRunner{fakeRunner: &fakeRunner{}}
	brk := &raceBroker{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = brk
	cfg.Audit = &raceAudit{}
	srv := New(cfg)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); runDispatch(srv, dispatchRun(runID)) }()

	// Wait until dispatch is in the in-flight window (sandbox created, ref
	// persisted, run STARTING) — where a real kill lands during an image pull.
	<-st.refSet

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")

	// The kill has fully completed (do() is synchronous) with the run KILLED;
	// release dispatch so its STARTING->RUNNING CAS deterministically LOSES and it
	// compensates the orphaned sandbox.
	close(st.killGate)
	wg.Wait()

	if w.Code != http.StatusAccepted {
		t.Fatalf("kill of an in-flight-dispatch run must 202 (clean teardown), got %d: %s", w.Code, w.Body.String())
	}
	if got := st.State(); got != types.RunKilled {
		t.Fatalf("dispatch must not resurrect the killed run to RUNNING; state = %q, want KILLED", got)
	}
	if n := rn.kills.Load(); n != 1 {
		t.Fatalf("the kill switch must tear the in-flight sandbox down exactly once via KillSandbox, got %d", n)
	}
	if n := rn.stops.Load(); n != 1 {
		t.Fatalf("dispatch must compensate its now-orphaned sandbox exactly once via StopSandbox, got %d", n)
	}
	if revs := brk.revocations(runID); revs != 1 {
		t.Fatalf("a won kill must revoke the run's credentials exactly once, got %d", revs)
	}
}

// TestKillRun_DispatchInFlightRace_ExactlyOnceTeardown is the GENUINE-race
// companion: over many rounds it races s.dispatch (the real
// CreateSandbox/SetSandboxRef/RUNNING-CAS seam) against handleKillRun, released
// together, under `go test -race`. Every legal interleaving must hold:
//   - kill won (202/500 ⇒ KILLED): credentials revoked exactly once; and if a
//     sandbox was ever created it is torn down (never orphaned), while a kill that
//     won BEFORE any sandbox existed tears nothing down;
//   - kill lost (409): dispatch won, so the run is RUNNING, its credentials are
//     NOT revoked (C002 — a losing kill must never strip a still-live run), and
//     its live sandbox is left UP (zero teardowns — no "RUNNING with a dead
//     sandbox").
func TestKillRun_DispatchInFlightRace_ExactlyOnceTeardown(t *testing.T) {
	h := newHarness(t)
	const rounds = 200
	for i := 0; i < rounds; i++ {
		runID := uuid.New()
		st := &raceDispatchStore{id: runID, state: types.RunPending}
		rn := &teardownCountRunner{fakeRunner: &fakeRunner{}}
		brk := &raceBroker{}
		cfg := baseTestConfig(h, st)
		cfg.Runner = rn
		cfg.Broker = brk
		cfg.Audit = &raceAudit{}
		srv := New(cfg)

		start := make(chan struct{})
		var (
			wg       sync.WaitGroup
			killCode int
		)
		wg.Add(2)
		go func() { // the real dispatch seam
			defer wg.Done()
			<-start
			runDispatch(srv, dispatchRun(runID))
		}()
		go func() { // the kill switch, racing an in-flight dispatch
			defer wg.Done()
			<-start
			w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
			killCode = w.Code
		}()
		close(start)
		wg.Wait()

		killWon := killCode == http.StatusAccepted || killCode == http.StatusInternalServerError
		created := rn.creates.Load() > 0
		teardowns := int(rn.stops.Load() + rn.kills.Load())
		revs := brk.revocations(runID)
		final := st.State()

		if killWon {
			if final != types.RunKilled {
				t.Fatalf("round %d: kill won (%d) but final=%s", i, killCode, final)
			}
			if revs != 1 {
				t.Fatalf("round %d: a won kill must revoke exactly once, got %d", i, revs)
			}
			if created && teardowns < 1 {
				t.Fatalf("round %d: kill won with a live sandbox but it was never torn down (orphan)", i)
			}
			if !created && teardowns != 0 {
				t.Fatalf("round %d: kill won before any sandbox existed, yet %d teardown(s) fired", i, teardowns)
			}
		} else {
			if killCode != http.StatusConflict {
				t.Fatalf("round %d: losing kill must 409, got %d", i, killCode)
			}
			if final != types.RunRunning {
				t.Fatalf("round %d: kill 409'd so dispatch won — final must be RUNNING, got %s", i, final)
			}
			if revs != 0 {
				t.Fatalf("round %d: a losing kill must not revoke a still-live run (C002), got %d", i, revs)
			}
			if teardowns != 0 {
				t.Fatalf("round %d: a still-RUNNING run must keep its live sandbox — no teardown, got %d (dead-sandbox bug)", i, teardowns)
			}
		}
	}
}

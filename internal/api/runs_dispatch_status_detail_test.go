// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// statusDetailStore is dispatchTestStore plus the one optional capability the
// startup-detail writer asserts on. Separate type, not a field on the shared
// double: a store that does NOT implement the setter is the pre-0.7.6 /
// test-double case the writer has to keep working for, and that case is only
// provable while some doubles still lack the method.
type statusDetailStore struct {
	*dispatchTestStore
	mu      sync.Mutex
	writes  []string
	block   chan struct{} // when non-nil, every write parks on it until its ctx dies
	ctxErrs []error
}

func (s *statusDetailStore) SetRunStatusDetail(ctx context.Context, _ uuid.UUID, detail string) error {
	s.mu.Lock()
	s.writes = append(s.writes, detail)
	block := s.block
	s.mu.Unlock()
	if block == nil {
		return nil
	}
	select {
	case <-block:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		s.ctxErrs = append(s.ctxErrs, ctx.Err())
		s.mu.Unlock()
		return ctx.Err()
	}
}

func (s *statusDetailStore) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// waitingRunner reports a scripted sequence of substrate reasons from INSIDE
// CreateSandbox — the window the real k8s poll loops live in, where the run has
// no sandbox_ref yet and nothing outside the driver can be asked what it is
// waiting on.
type waitingRunner struct {
	*fakeRunner
	details []string
	elapsed time.Duration
}

func (r *waitingRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	start := time.Now()
	for _, d := range r.details {
		spec.NotifyWaiting(d)
	}
	r.elapsed = time.Since(start)
	return r.fakeRunner.CreateSandbox(ctx, spec)
}

func statusDetailDispatchFixture(t *testing.T, rn runner.Runner) (*Server, *statusDetailStore, types.AgentRun) {
	t.Helper()
	h := newHarness(t)
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunPending,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &statusDetailStore{dispatchTestStore: &dispatchTestStore{run: run, state: types.RunPending}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = h.broker
	cfg.Audit = &recRecorder{}
	return New(cfg), st, run
}

func dispatchOnce(srv *Server, run types.AgentRun) {
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
}

// TestDispatch_WritesStatusDetailWhileCreateSandboxBlocks is finding 6's seam
// end to end on the control-plane side: the reason reaches the run row while
// CreateSandbox is still blocked, which is the only window it is of any use in.
//
// TWO writes for three reports: the driver dedupes per pod, but the dispatcher
// sees BOTH pods' loops (proxy, then agent) and a reason that repeats across
// them must still cost one UPDATE, not two — the poll is 200ms and the timeout
// it runs under is three minutes.
func TestDispatch_WritesStatusDetailWhileCreateSandboxBlocks(t *testing.T) {
	rn := &waitingRunner{fakeRunner: &fakeRunner{}, details: []string{
		"agent: ContainerCreating",
		"agent: ContainerCreating",
		"agent: ImagePullBackOff: rpc error: pull access denied",
	}}
	srv, st, run := statusDetailDispatchFixture(t, rn)

	dispatchOnce(srv, run)

	want := []string{"agent: ContainerCreating", "agent: ImagePullBackOff: rpc error: pull access denied"}
	got := st.written()
	if len(got) != len(want) {
		t.Fatalf("store writes = %q, want exactly %q — only a CHANGE of reason is worth an UPDATE", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("store writes = %q, want %q", got, want)
		}
	}
}

// TestDispatch_StatusDetailWriteNeverOutlivesItsDeadline is Codex #10. The
// callback is SYNCHRONOUS on CreateSandbox's own goroutine, dispatchRun runs
// under context.WithoutCancel, and the store hands its ctx to the pool — so a
// locked row or an exhausted pool would park the k8s poll loop on a diagnostic
// write while the canary's three-minute budget burns. Startup progress must
// never depend on startup DIAGNOSTICS being persistable: each write gets its own
// short deadline, an overdue one is dropped, and no goroutine is spawned per
// update (which would just move the pile-up somewhere unbounded).
func TestDispatch_StatusDetailWriteNeverOutlivesItsDeadline(t *testing.T) {
	rn := &waitingRunner{fakeRunner: &fakeRunner{}, details: []string{
		"agent: ContainerCreating",
		"agent: ImagePullBackOff: rpc error: pull access denied",
	}}
	srv, st, run := statusDetailDispatchFixture(t, rn)
	st.block = make(chan struct{}) // never closed: every write blocks until its ctx dies

	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatchOnce(srv, run)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never returned: a blocked status write trapped the driver's poll loop")
	}

	// Both writes were attempted and both were bounded — the driver kept going.
	if got := len(st.written()); got != 2 {
		t.Fatalf("attempted writes = %d, want 2", got)
	}
	st.mu.Lock()
	ctxErrs := len(st.ctxErrs)
	st.mu.Unlock()
	if ctxErrs != 2 {
		t.Fatalf("writes cancelled by their own deadline = %d, want 2", ctxErrs)
	}
	// The two bounded waits are the whole cost the driver paid.
	if rn.elapsed > 4*statusDetailWriteTimeout {
		t.Fatalf("CreateSandbox spent %v inside OnWaiting for two writes bounded at %v each", rn.elapsed, statusDetailWriteTimeout)
	}
}

// TestDispatch_StatusDetailIsOptional: a store that does not implement the
// setter (every test double, and any future backend) leaves OnWaiting nil, so a
// driver sees exactly the spec it saw before 0.7.6. A lost status line never
// fails a dispatch.
func TestDispatch_StatusDetailIsOptional(t *testing.T) {
	rn := &waitingRunner{fakeRunner: &fakeRunner{}, details: []string{"agent: ContainerCreating"}}
	h := newHarness(t)
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunPending,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &dispatchTestStore{run: run, state: types.RunPending}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = h.broker
	cfg.Audit = &recRecorder{}
	srv := New(cfg)

	dispatchOnce(srv, run)

	if st.State() == types.RunFailed {
		t.Fatal("dispatch failed on a store that cannot record a status detail")
	}
}

// TestDispatch_RecordsStartWaitByReason: finding 6's anecdote ("127s and 131s,
// on two occasions") measured by hand, twice, and believed only because somebody
// had written it down. wardyn_run_start_wait_seconds makes it a series — per
// REASON, so "slow because it pulls" and "slow because nothing will schedule it"
// are different lines on the graph rather than one number nobody can act on.
//
// The label set is CLOSED: a substrate can invent a reason, and a free-form
// label would be one series per string it ever says.
func TestDispatch_RecordsStartWaitByReason(t *testing.T) {
	rn := &waitingRunner{fakeRunner: &fakeRunner{}, details: []string{
		"agent: ContainerCreating",
		"agent: ImagePullBackOff: rpc error: pull access denied",
		"agent: SomeReasonNobodyHasSeen: what",
	}}
	srv, _, run := statusDetailDispatchFixture(t, rn)

	dispatchOnce(srv, run)

	var buf bytes.Buffer
	srv.metrics.write(&buf)
	out := buf.String()
	for _, want := range []string{
		`wardyn_run_start_wait_seconds_count{reason="ContainerCreating"} 1`,
		`wardyn_run_start_wait_seconds_count{reason="ImagePullBackOff"} 1`,
		// The unknown reason folds onto the catch-all rather than minting a series.
		`wardyn_run_start_wait_seconds_count{reason="other"} 1`,
		// The LAST reason is only closable by dispatch's own end-of-create call:
		// OnWaiting is never called again after CreateSandbox returns.
		`wardyn_run_start_wait_seconds_count{reason="Pulling"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, `reason="SomeReasonNobodyHasSeen"`) {
		t.Error("a substrate's own string became a metric label; the set must stay closed")
	}
}

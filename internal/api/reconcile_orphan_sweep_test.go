// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A terminal run's SandboxRef only survives finalizeRunTail non-empty when
// its StopSandbox call failed — and ReconcileOnBoot's
// `isTerminalRunState(run.State) { continue }` guard means the main loop
// never looks at that run again. The abandoned container (and its proxy
// sidecar, which resolved injected credential VALUES into memory at startup)
// would be audited once, at finalize time, and then be permanent. These
// tests pin
// reconcileOrphanedSandbox: ReconcileOnBoot must retry the teardown of every
// terminal run that still carries a ref, clearing the ref on success.

// orphanSweepStore is an in-memory Store exposing just the surface
// ReconcileOnBoot's terminal-run sweep drives: ListRuns + SetSandboxRef.
// Deliberately NOT the PG harness — this invariant must be provable without
// WARDYN_TEST_PG.
type orphanSweepStore struct {
	store.Store
	mu          sync.Mutex
	runs        []types.AgentRun
	clearedRefs []uuid.UUID
}

func (*orphanSweepStore) LiveImageCheckAttempts(context.Context) (map[uuid.UUID]bool, error) {
	return nil, nil
}

func (s *orphanSweepStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.AgentRun, len(s.runs))
	copy(out, s.runs)
	return out, nil
}

func (s *orphanSweepStore) SetSandboxRef(_ context.Context, id uuid.UUID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs {
		if s.runs[i].ID == id {
			s.runs[i].SandboxRef = ref
		}
	}
	if ref == "" {
		s.clearedRefs = append(s.clearedRefs, id)
	}
	return nil
}

func (s *orphanSweepStore) SetRunDiskMiB(context.Context, uuid.UUID, int) error { return nil }

// orphanSweepRunner records every StopSandbox call and can be told to fail on
// a given ref.
type orphanSweepRunner struct {
	*fakeRunner
	mu      sync.Mutex
	stopped []string
	failRef string
}

func (r *orphanSweepRunner) StopSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, ref)
	if ref == r.failRef {
		return context.DeadlineExceeded
	}
	return nil
}

func (r *orphanSweepRunner) stopCount(ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.stopped {
		if s == ref {
			n++
		}
	}
	return n
}

// TestReconcileOnBoot_SweepsOrphanedTerminalSandbox is the counterfactual for
// that guard: on its own, ReconcileOnBoot's terminal guard skips this run
// outright and StopSandbox is never called for it — the container leaks
// forever. The sweep tears it down and clears the ref so the run drops out of
// future sweeps.
func TestReconcileOnBoot_SweepsOrphanedTerminalSandbox(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed,
		RunnerTarget: "docker", SandboxRef: "orphan-container-1",
	}}}
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}

	if n := rn.stopCount("orphan-container-1"); n != 1 {
		t.Fatalf("terminal run's orphaned sandbox must be torn down exactly once on boot, got %d StopSandbox calls", n)
	}
	st.mu.Lock()
	gotRef := st.runs[0].SandboxRef
	cleared := len(st.clearedRefs)
	st.mu.Unlock()
	if gotRef != "" {
		t.Errorf("SandboxRef must be cleared after a successful sweep teardown, got %q", gotRef)
	}
	if cleared != 1 {
		t.Errorf("expected exactly one cleared ref, got %d", cleared)
	}
}

// TestReconcileOnBoot_OrphanSweepRetriesRevoke: a terminal run's SandboxRef
// survives only when its original finalizeRunTail teardown failed, but
// finalizeRunTail runs revokeRunCascade before that teardown — so the run
// reaching this boot pass may carry an un-revoked identity/broker credential
// independent of whether the retried teardown itself succeeds. An orphan
// sweep that retried only StopSandbox would leave that credential
// un-revoked. The boot pass must re-run the revoke cascade for every
// terminal run it walks.
func TestReconcileOnBoot_OrphanSweepRetriesRevoke(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed,
		RunnerTarget: "docker", SandboxRef: "orphan-container-2",
	}}}
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = h.broker
	srv := testServer(t, cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}

	revoked := false
	for _, id := range h.broker.revoked {
		if id == runID {
			revoked = true
		}
	}
	if !revoked {
		t.Errorf("boot orphan sweep must re-run the credential revoke cascade for %s; broker.revoked=%v", runID, h.broker.revoked)
	}
}

// TestReconcileOnBoot_OrphanSweepFailureAudited: when the retried teardown
// STILL fails, the ref must stay set (so the NEXT boot retries again) and the
// failure must be audited with teardown_error — silence would re-create the
// exact "audited once, then permanent" gap this fix closes.
func TestReconcileOnBoot_OrphanSweepFailureAudited(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed,
		RunnerTarget: "docker", SandboxRef: "still-stuck",
	}}}
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}, failRef: "still-stuck"}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}

	if n := rn.stopCount("still-stuck"); n != 1 {
		t.Fatalf("sweep must attempt the retry exactly once per boot, got %d", n)
	}
	st.mu.Lock()
	gotRef := st.runs[0].SandboxRef
	st.mu.Unlock()
	if gotRef != "still-stuck" {
		t.Errorf("a still-failing teardown must leave the ref set for the next boot to retry, got %q", gotRef)
	}
	var found *types.AuditEvent
	for i := range h.audit.events {
		ev := &h.audit.events[i]
		if ev.RunID != nil && *ev.RunID == runID && strings.Contains(string(ev.Data), "teardown_error") {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("a still-failing orphan-sweep teardown must be audited with teardown_error")
	}
}

// orphanTeardownRunner answers each ref's direct stop and kill with the error given, and records both.
type orphanTeardownRunner struct {
	*fakeRunner
	mu      sync.Mutex
	stops   map[string]error
	kills   map[string]error
	stopped []string
	killed  []string
}

func (r *orphanTeardownRunner) StopSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, ref)
	return r.stops[ref]
}

func (r *orphanTeardownRunner) KillSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.killed = append(r.killed, ref)
	return r.kills[ref]
}

// TestReconcileOrphanSweep_KillsARunnerSandboxNeverStopsIt: a terminal run's sandbox on a runner is
// killed, never stopped. A stop queued for an offline runner (at boot, every runner) is an end, which a
// runner that cannot keep an ended sandbox refuses, and so does a record kept only for containment;
// every runner takes a kill. A sandbox on no runner is still stopped, and each failed teardown is
// audited once.
func TestReconcileOrphanSweep_KillsARunnerSandboxNeverStopsIt(t *testing.T) {
	h := newHarness(t)
	runnerID := uuid.New()
	killed := placement.RunnerRefPrefix(runnerID.String()) + "killed"
	pending := placement.RunnerRefPrefix(runnerID.String()) + "pending"
	stuck := placement.RunnerRefPrefix(runnerID.String()) + "stuck"
	local := "local"
	now := time.Now().UTC()
	var runs []types.AgentRun
	for _, ref := range []string{killed, pending, stuck, local} {
		run := types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
			Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed, RunnerTarget: "docker", SandboxRef: ref}
		if ref != local {
			run.RunnerID = &runnerID
		}
		runs = append(runs, run)
	}
	st := &orphanSweepStore{runs: runs}
	rn := &orphanTeardownRunner{fakeRunner: &fakeRunner{},
		stops: map[string]error{local: errors.New("docker: stop: refused")},
		kills: map[string]error{pending: runner.ErrPendingOnRunner, stuck: context.DeadlineExceeded}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)

	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rn.killed, ","); got != killed+","+pending+","+stuck {
		t.Fatalf("kills %q, want one for each sandbox on the runner", got)
	}
	if got := strings.Join(rn.stopped, ","); got != local {
		t.Fatalf("stops %q, want only the sandbox on no runner", got)
	}
	for i, want := range []string{"", pending, stuck, local} {
		if got := st.runs[i].SandboxRef; got != want {
			t.Errorf("run %d: ref %q, want %q", i, got, want)
		}
		failures := 0
		for _, ev := range h.audit.snapshot() {
			if ev.RunID != nil && *ev.RunID == st.runs[i].ID && ev.Action == "sandbox.orphan.sweep" && ev.Outcome == "failure" {
				failures++
			}
		}
		if wantFailures := min(len(want), 1); failures != wantFailures {
			t.Errorf("run %d: %d failure rows, want %d", i, failures, wantFailures)
		}
	}
}

// orphanDeadlineRunner records whether each substrate call of the orphan passes carried a deadline.
type orphanDeadlineRunner struct {
	*fakeRunner
	stopBounded, sweepBounded bool
}

func (r *orphanDeadlineRunner) StopSandbox(ctx context.Context, _ string) error {
	_, r.stopBounded = ctx.Deadline()
	return nil
}

func (r *orphanDeadlineRunner) SweepOrphanedSandboxes(ctx context.Context, _ time.Duration, _ func(uuid.UUID) bool) (int, error) {
	_, r.sweepBounded = ctx.Deadline()
	return 0, nil
}

// A substrate that accepts the call and never answers must not hold boot (the control plane would never
// serve) or the single-threaded sweeper: the boot teardown of an orphan and the orphan sweep each run
// under a deadline of their own, whatever the caller's context.
func TestOrphanPassesBoundTheirSubstrateCalls(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed, RunnerTarget: "docker", SandboxRef: "local"}}}
	rn := &orphanDeadlineRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)
	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rn.stopBounded || !rn.sweepBounded {
		t.Fatalf("orphan teardown bounded %v, orphan sweep bounded %v; want both", rn.stopBounded, rn.sweepBounded)
	}
}

// keptFolderStore is orphanSweepStore whose runs in kept have a computer folder's pass back that
// keeps their sandbox for a resume.
type keptFolderStore struct {
	*orphanSweepStore
	kept map[uuid.UUID]bool
}

func (s *keptFolderStore) RunLocalFolderStatus(_ context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	if s.kept[id] {
		return types.RunLocalFolderStatus{Resume: &types.FolderResume{Stalled: 1}}, nil
	}
	return types.RunLocalFolderStatus{}, nil
}

func (s *keptFolderStore) SetRunLocalFolderStatusIf(context.Context, uuid.UUID, store.FolderStatusGuard, types.RunLocalFolderStatus) (bool, error) {
	return true, nil
}

func (s *keptFolderStore) BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, _ store.FolderStatusGuard, at time.Time) (types.RunLocalFolderStatus, bool, error) {
	st, err := s.RunLocalFolderStatus(ctx, id)
	st.Gen, st.GenAt = st.Gen+1, at
	return st, err == nil, err
}

func (s *keptFolderStore) GiveUpRunLocalFolder(context.Context, uuid.UUID, store.GiveUpParams) (store.GiveUpOutcome, error) {
	return store.GiveUpOutcome{}, nil
}

func (s *keptFolderStore) MarkGivenUpAudited(context.Context, uuid.UUID, int) (bool, error) {
	return false, nil
}

func (s *keptFolderStore) ListGivenUpUnaudited(context.Context, *uuid.UUID, int) ([]uuid.UUID, *uuid.UUID, error) {
	return nil, nil, nil
}

// orphanVerdictRunner records the orphan sweep's verdict on each run it is asked about.
type orphanVerdictRunner struct {
	*orphanSweepRunner
	verdicts map[uuid.UUID]bool
}

// StopProxy makes the runner a substrate that can cut a kept sandbox's agent off, which the orphan passes now ask
// for before they keep a sandbox (folderKeepVerdict).
func (r *orphanVerdictRunner) StopProxy(context.Context, string) error { return nil }

func (r *orphanVerdictRunner) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	for id := range r.verdicts {
		r.verdicts[id] = isOrphan(id)
	}
	return 0, nil
}

// hangingCutRunner is a substrate whose proxy stop never answers until its context ends.
type hangingCutRunner struct{ *orphanVerdictRunner }

func (hangingCutRunner) StopProxy(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// A cut that never answers is bounded on both orphan passes. On the boot pass it counts as unconfirmed and the
// sandbox kept for its computer folder is stopped. On the label sweep, run on its own fixture so the ref is still
// there and the cut is really sent, it ends at the sweep's bound and decides nothing: the run is not called an
// orphan, keeps its ref and is left to the terminal sweep. Catches: an unbounded cut in the boot pass or the label
// sweep, a timed-out boot cut read as a keep, and a label sweep with nothing to cut.
func TestOrphanPasses_AHangingCutTimesOutAndTheSandboxIsStopped(t *testing.T) {
	old := reconcileCutBound
	reconcileCutBound = 50 * time.Millisecond
	t.Cleanup(func() { reconcileCutBound = old })
	h := newHarness(t)
	now := time.Now().UTC()
	kept := uuid.New()
	run := types.AgentRun{ID: kept, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com", Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunCompleted, RunnerTarget: "docker", SandboxRef: "kept"}
	st := &keptFolderStore{orphanSweepStore: &orphanSweepStore{runs: []types.AgentRun{run}}, kept: map[uuid.UUID]bool{kept: true}}
	rn := &orphanVerdictRunner{orphanSweepRunner: &orphanSweepRunner{fakeRunner: &fakeRunner{}}, verdicts: map[uuid.UUID]bool{kept: false}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = hangingCutRunner{rn}
	srv := testServer(t, cfg)
	start := time.Now()
	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rn.stopCount("kept"); n != 1 {
		t.Errorf("boot orphan pass stopped the sandbox %d times; want 1: its cut timed out", n)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the boot pass took %v; a cut must end at its bound", took)
	}

	const bound = 100 * time.Millisecond
	lsrv, lst, lrn, _ := keptSweepRig(t, 1, bound)
	lrn.hang.Store(true)
	id := lst.runs[0].ID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // only a cut with no bound of its own waits this out
	defer cancel()
	start = time.Now()
	if err := lsrv.sweepOrphanedSandboxes(ctx); err != nil {
		t.Fatal(err)
	}
	if lrn.cuts() != 1 {
		t.Fatalf("the label sweep sent %d cuts; want 1, the kept run's", lrn.cuts())
	}
	if dl := lrn.deadline(0); dl.IsZero() || dl.After(start.Add(bound+time.Second)) {
		t.Errorf("the cut ran to %v, %v after the sweep began; want the sweep's bound of %v", dl, dl.Sub(start), bound)
	}
	if lrn.verdicts[id] || len(lst.clearedRefs) != 0 || lst.runs[0].SandboxRef == "" {
		t.Errorf("verdict %v, refs cleared %v; want the run left with its ref, not called an orphan", lrn.verdicts[id], lst.clearedRefs)
	}
}

// sweepCutRunner is orphanVerdictRunner whose proxy stop is counted with the deadline it ran under: while hang is set
// it never answers until its context ends, and otherwise it fails at once.
type sweepCutRunner struct {
	*orphanVerdictRunner
	hang      atomic.Bool
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *sweepCutRunner) StopProxy(ctx context.Context, _ string) error {
	dl, _ := ctx.Deadline()
	r.mu.Lock()
	r.deadlines = append(r.deadlines, dl)
	r.mu.Unlock()
	if r.hang.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return errors.New("the proxy did not stop")
}

func (r *sweepCutRunner) cuts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deadlines)
}

func (r *sweepCutRunner) deadline(i int) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.deadlines) {
		return time.Time{}
	}
	return r.deadlines[i]
}

// deadCtxRefusingAudit is recRecorder that refuses a row on a context that has ended, as the store's recorder does
// where no audit spool holds the row.
type deadCtxRefusingAudit struct{ recRecorder }

func (a *deadCtxRefusingAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.recRecorder.Record(ctx, ev)
}

// keptSweepRig is a server over n ended runs whose sandboxes are kept for their computer folders, on a
// sweepCutRunner, with the label sweep's bound shortened to bound and an audit log that refuses a row on an ended
// context.
func keptSweepRig(t *testing.T, n int, bound time.Duration) (*Server, *keptFolderStore, *sweepCutRunner, *deadCtxRefusingAudit) {
	t.Helper()
	old := orphanSweepBound
	orphanSweepBound = bound
	t.Cleanup(func() { orphanSweepBound = old })
	h := newHarness(t)
	now := time.Now().UTC()
	st := &keptFolderStore{orphanSweepStore: &orphanSweepStore{}, kept: map[uuid.UUID]bool{}}
	rn := &sweepCutRunner{orphanVerdictRunner: &orphanVerdictRunner{orphanSweepRunner: &orphanSweepRunner{fakeRunner: &fakeRunner{}},
		verdicts: map[uuid.UUID]bool{}}}
	for range n {
		id := uuid.New()
		st.runs = append(st.runs, types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com", Agent: "claude-code",
			ConfinementClass: types.CC1, State: types.RunCompleted, RunnerTarget: "docker", SandboxRef: "kept-" + id.String()})
		st.kept[id] = true
		rn.verdicts[id] = false
	}
	audit := &deadCtxRefusingAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Audit = audit
	return testServer(t, cfg), st, rn, audit
}

// cutFailureRows is the sandbox.orphan.sweep failure rows naming a cut's teardown_error for run id.
func cutFailureRows(audit *deadCtxRefusingAudit, id uuid.UUID) int {
	n := 0
	for _, ev := range audit.snapshot() {
		if ev.Action == "sandbox.orphan.sweep" && ev.Outcome == "failure" && ev.RunID != nil && *ev.RunID == id &&
			strings.Contains(string(ev.Data), "teardown_error") {
			n++
		}
	}
	return n
}

// Cuts that never answer share the label sweep's one bound, not a fresh bound each: three of them take that bound in
// all, and the failure row of the cut it ended may take up to one runTeardownBudget past it (the allowance the
// slow-row pin, TestOrphanSweep_ASlowFailureRowTakesOneTeardownBudgetPastTheBound, tests; the sweep's own reads and
// the lock's release are outside both). The first cut is sent and times out, and the others are not sent once the
// bound is spent. None of the runs is called an orphan or loses its ref, so each is still owed to the terminal sweep,
// and the next sweep, whose cuts answer, decides every one of them. Nothing is dropped. Catches: a fresh bound per
// cut (N times the bound per sweep), and a timed-out cut read as an orphan whose stop then fails on the spent sweep.
func TestOrphanSweep_HangingCutsShareTheSweepsBoundAndAreLeftOwed(t *testing.T) {
	old := reconcileCutBound
	reconcileCutBound = 100 * time.Millisecond // a fresh per-cut bound, were one given, would also time out
	t.Cleanup(func() { reconcileCutBound = old })
	const bound = 100 * time.Millisecond
	srv, st, rn, audit := keptSweepRig(t, 3, bound)
	rn.hang.Store(true)
	start := time.Now()
	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rn.cuts() != 1 {
		t.Errorf("%d cuts sent in one sweep; want 1, the rest left once the sweep's bound was spent", rn.cuts())
	}
	if dl := rn.deadline(0); dl.IsZero() || dl.After(start.Add(bound+time.Second)) {
		t.Errorf("the cut ran to %v after the sweep began; want the sweep's bound of %v", dl.Sub(start), bound)
	}
	rows := 0
	for _, r := range st.runs {
		if rn.verdicts[r.ID] || r.SandboxRef == "" {
			t.Errorf("run %s: verdict %v, ref %q; want it not an orphan and its ref kept", r.ID, rn.verdicts[r.ID], r.SandboxRef)
		}
		if kept, err := srv.keepsSandboxForFolder(context.Background(), r); !kept || err != nil {
			t.Errorf("run %s: still kept for its folder = %v, %v; want it owed", r.ID, kept, err)
		}
		rows += cutFailureRows(audit, r.ID)
	}
	if len(st.clearedRefs) != 0 || rows != 1 {
		t.Errorf("refs cleared %v, %d cut failure rows; want none cleared and the one timed-out cut's row", st.clearedRefs, rows)
	}
	rn.hang.Store(false)
	orphanSweepBound = time.Minute // the hang is off: a long bound, so three cuts and their rows cannot run it out
	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range st.runs {
		if !rn.verdicts[r.ID] {
			t.Errorf("run %s: the next sweep left it; want its failed cut decided as an orphan", r.ID)
		}
	}
	if rn.cuts() != 1+len(st.runs) {
		t.Errorf("%d cuts over two sweeps; want %d: every run cut in the second", rn.cuts(), 1+len(st.runs))
	}
}

// slowRowAudit is deadCtxRefusingAudit whose cut failure rows are slow but honour their context: each waits slow, or
// until its context ends, and the row's context deadline and the time the write returned are noted.
type slowRowAudit struct {
	*deadCtxRefusingAudit
	slow               time.Duration
	mu                 sync.Mutex
	deadline, returned time.Time
}

func (a *slowRowAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == "sandbox.orphan.sweep" && ev.Outcome == "failure" {
		dl, _ := ctx.Deadline()
		select {
		case <-ctx.Done():
		case <-time.After(a.slow):
		}
		a.mu.Lock()
		a.deadline, a.returned = dl, time.Now()
		a.mu.Unlock()
	}
	return a.deadCtxRefusingAudit.Record(ctx, ev)
}

func (a *slowRowAudit) noted() (deadline, returned time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deadline, a.returned
}

// The failure row of the cut the sweep's bound ended is the one write allowed past that bound: on an audit log that
// is slow but honours its context, the row's context ends no later than the sweep's end plus one runTeardownBudget,
// the row is written, and the sweep returns once that write does. It pins that allowance on the cuts and their row,
// not a bound on the whole sweep (whose own reads here are a fake's). Catches: a row on a context that outlives the
// budget (so the stated allowance is wrong), a row on the spent sweep context (lost), and a wait after the row.
func TestOrphanSweep_ASlowFailureRowTakesOneTeardownBudgetPastTheBound(t *testing.T) {
	const bound, slow, slack = 100 * time.Millisecond, 300 * time.Millisecond, time.Second
	srv, st, rn, audit := keptSweepRig(t, 1, bound)
	sa := &slowRowAudit{deadCtxRefusingAudit: audit, slow: slow}
	srv.cfg.Audit = sa
	rn.hang.Store(true)
	start := time.Now()
	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	took, end := time.Since(start), time.Now()
	cutEnd := rn.deadline(0)
	dl, returned := sa.noted()
	if cutEnd.IsZero() || dl.IsZero() || returned.IsZero() {
		t.Fatalf("cut deadline %v, row deadline %v, row returned %v; want a timed-out cut and its row", cutEnd, dl, returned)
	}
	if !dl.After(cutEnd) || dl.After(start.Add(bound+runTeardownBudget+slack)) {
		t.Errorf("the row's context ends %v after the cut's and %v after the sweep began; want past the cut and within "+
			"the bound %v plus one runTeardownBudget %v", dl.Sub(cutEnd), dl.Sub(start), bound, runTeardownBudget)
	}
	if n := cutFailureRows(audit, st.runs[0].ID); n != 1 {
		t.Errorf("%d cut failure rows; want the timed-out cut's one row", n)
	}
	if end.Sub(returned) > slack || took > bound+runTeardownBudget+slack {
		t.Errorf("the sweep took %v and returned %v after the row's write; want it back once the write is, within %v",
			took, end.Sub(returned), bound+runTeardownBudget+slack)
	}
}

// ctxWaitingLocker is a Locker whose TryLock never takes the lock: it waits until its context ends, as a lock
// connection that never answers does, and notes that context's deadline. Lock is the wrapped Locker's.
type ctxWaitingLocker struct {
	db.Locker
	mu        sync.Mutex
	deadlines []time.Time
}

func (l *ctxWaitingLocker) TryLock(ctx context.Context, _ db.LockKey) (context.Context, func(), bool, error) {
	dl, _ := ctx.Deadline()
	l.mu.Lock()
	l.deadlines = append(l.deadlines, dl)
	l.mu.Unlock()
	<-ctx.Done()
	return ctx, nil, false, ctx.Err()
}

// The run's lock wait is inside the label sweep's bound: a lock whose acquisition never answers is given up at that
// bound (the sweep call returns within it plus slack; its other reads are a fake's here), and the run is left owed:
// no cut sent, no failure row, not called an orphan, its ref kept. The next sweep, with the lock free, decides it.
// The caller's context carries a long deadline, so a wait outside the bound fails by assertion, not by the test
// timeout. Catches: the lock taken on the caller's context, which waits past the bound for as long as that lasts.
func TestOrphanSweep_ALockThatNeverAnswersEndsAtTheSweepsBound(t *testing.T) {
	const bound, slack = 100 * time.Millisecond, time.Second
	srv, st, rn, audit := keptSweepRig(t, 1, bound)
	lk := &ctxWaitingLocker{Locker: db.NewLocalLocker()}
	srv.locks.override = lk
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // only a lock taken outside the bound waits this out
	defer cancel()
	start := time.Now()
	err := srv.sweepOrphanedSandboxes(ctx)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took > bound+slack {
		t.Errorf("the sweep took %v with a lock that never answers; want it back within the bound %v", took, bound)
	}
	lk.mu.Lock()
	dls := append([]time.Time(nil), lk.deadlines...)
	lk.mu.Unlock()
	if len(dls) != 1 || dls[0].IsZero() || dls[0].After(start.Add(bound+slack)) {
		t.Errorf("lock attempts with deadlines %v (sweep began %v); want one, ending within the bound %v", dls, start, bound)
	}
	r := st.runs[0]
	if rn.cuts() != 0 || rn.verdicts[r.ID] || r.SandboxRef == "" || len(st.clearedRefs) != 0 || cutFailureRows(audit, r.ID) != 0 {
		t.Errorf("%d cuts, verdict %v, ref %q, refs cleared %v, %d cut failure rows; want no cut, not an orphan, its ref "+
			"kept, no row", rn.cuts(), rn.verdicts[r.ID], r.SandboxRef, st.clearedRefs, cutFailureRows(audit, r.ID))
	}
	if kept, err := srv.keepsSandboxForFolder(context.Background(), r); !kept || err != nil {
		t.Errorf("still kept for its folder = %v, %v; want it owed", kept, err)
	}
	srv.locks.override = nil       // the lock is free again
	orphanSweepBound = time.Minute // a long bound for the deciding sweep; the rig restores it
	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rn.cuts() != 1 || !rn.verdicts[r.ID] {
		t.Errorf("the next sweep: %d cuts, verdict %v; want its cut sent and the run decided", rn.cuts(), rn.verdicts[r.ID])
	}
}

// The failure row of a cut that timed out is written past the cut's own bound, on a context of its own: an audit log
// with no spool refuses a row on the ended verdict context. Catches: the row written on the expired context and lost.
func TestOrphanPasses_ATimedOutCutsFailureRowIsWrittenOnALiveContext(t *testing.T) {
	old := reconcileCutBound
	reconcileCutBound = 50 * time.Millisecond
	t.Cleanup(func() { reconcileCutBound = old })
	srv, st, rn, audit := keptSweepRig(t, 1, time.Minute)
	rn.hang.Store(true)
	id, ref := st.runs[0].ID, st.runs[0].SandboxRef
	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := cutFailureRows(audit, id); n != 1 || rn.cuts() != 1 {
		t.Errorf("%d cut failure rows after %d cuts; want the timed-out cut's one row", n, rn.cuts())
	}
	if n := rn.stopCount(ref); n != 1 {
		t.Errorf("the sandbox was stopped %d times; want 1: its cut timed out", n)
	}
}

// Neither orphan pass tears down a sandbox that its computer folder's pass back keeps for
// the tail owner; another terminal run's sandbox still goes (#3466).
func TestOrphanPasses_LeaveASandboxKeptForAComputerFolderResume(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	kept, ended := uuid.New(), uuid.New()
	run := func(id uuid.UUID, ref string) types.AgentRun {
		return types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com", Agent: "claude-code",
			ConfinementClass: types.CC1, State: types.RunCompleted, RunnerTarget: "docker", SandboxRef: ref}
	}
	st := &keptFolderStore{orphanSweepStore: &orphanSweepStore{runs: []types.AgentRun{run(kept, "kept"), run(ended, "ended")}},
		kept: map[uuid.UUID]bool{kept: true}}
	rn := &orphanVerdictRunner{orphanSweepRunner: &orphanSweepRunner{fakeRunner: &fakeRunner{}}, verdicts: map[uuid.UUID]bool{kept: true, ended: false}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)
	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k, e := rn.stopCount("kept"), rn.stopCount("ended"); k != 0 || e != 1 {
		t.Errorf("boot orphan pass stopped the kept sandbox %d times and the other %d; want 0 and 1", k, e)
	}
	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rn.verdicts[kept] || !rn.verdicts[ended] {
		t.Errorf("orphan sweep verdicts: kept %v, ended %v; want false and true", rn.verdicts[kept], rn.verdicts[ended])
	}
}

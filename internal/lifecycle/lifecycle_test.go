// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── Fakes ───────────────────────────────────────────────────────────────────

// fakeClock is a deterministic clock whose value is set by the test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// fakeStore holds RunSummary rows; satisfies lifecycle.Store.
type fakeStore struct {
	mu   sync.Mutex
	rows []lifecycle.RunSummary
	err  error // if non-nil, ListRunningWithPolicy returns this
}

func (f *fakeStore) ListRunningWithPolicy(_ context.Context) ([]lifecycle.RunSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]lifecycle.RunSummary, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// fakeStopper records which run IDs were stopped; satisfies lifecycle.Stopper.
type fakeStopper struct {
	mu      sync.Mutex
	stopped []uuid.UUID
	errOn   map[uuid.UUID]error // per-run error overrides
	// noApplyOn marks runs whose StopRun returns ({Applied:false}, nil): the stop
	// was a no-op because the run had already moved to a terminal state
	// (kill/complete won the race) or was touched after the snapshot (active
	// attach). The reaper must NOT emit run.autostop for these (findings #1 / N3).
	noApplyOn map[uuid.UUID]bool
	// revokeErrOn marks runs whose stop APPLIES but whose teardown/revocation
	// fails: StopRun returns ({Applied:true, Errors: <this>}, nil). The reaper must
	// then emit a distinct run.revoke/failure event (finding N1).
	revokeErrOn map[uuid.UUID]map[string]string
	// notAfterSeen records the notAfter arg the reaper threaded in, per run, so a
	// test can assert the snapshot's updated_at was passed through (finding N3).
	notAfterSeen map[uuid.UUID]time.Time
}

func newFakeStopper() *fakeStopper {
	return &fakeStopper{
		errOn:        make(map[uuid.UUID]error),
		noApplyOn:    make(map[uuid.UUID]bool),
		revokeErrOn:  make(map[uuid.UUID]map[string]string),
		notAfterSeen: make(map[uuid.UUID]time.Time),
	}
}

func (f *fakeStopper) StopRun(_ context.Context, id uuid.UUID, notAfter time.Time) (lifecycle.StopOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notAfterSeen[id] = notAfter
	if err, ok := f.errOn[id]; ok {
		return lifecycle.StopOutcome{}, err
	}
	if f.noApplyOn[id] {
		// The idle-guarded RUNNING->STOPPED transition did not apply: the run was
		// already terminal or touched after the snapshot. Record nothing.
		return lifecycle.StopOutcome{Applied: false}, nil
	}
	f.stopped = append(f.stopped, id)
	out := lifecycle.StopOutcome{Applied: true}
	if errs, ok := f.revokeErrOn[id]; ok {
		out.Errors = errs
	}
	return out, nil
}

func (f *fakeStopper) wasStopped(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.stopped {
		if s == id {
			return true
		}
	}
	return false
}

// fakeRecorder captures audit events; satisfies lifecycle.Recorder.
type fakeRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (f *fakeRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeRecorder) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *fakeRecorder) last() (types.AuditEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return types.AuditEvent{}, false
	}
	return f.events[len(f.events)-1], true
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// makeReaper creates a Reaper wired to the provided fakes with a deliberately
// short interval (never fires during unit tests — we call tick directly).
func makeReaper(store *fakeStore, stopper *fakeStopper, rec *fakeRecorder, clk *fakeClock) *lifecycle.Reaper {
	return lifecycle.New(store, stopper, rec, lifecycle.Config{
		Interval: time.Hour, // effectively never fires in unit tests
		Clock:    clk,
	})
}

// ─── Tests ───────────────────────────────────────────────────────────────────

// TestIdleRunIsStopped: a run whose updated_at is older than the default
// threshold must be stopped and an audit event emitted.
func TestIdleRunIsStopped(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	runID := uuid.New()

	// Run has been idle for 35 minutes — 5 minutes past its 30-minute policy
	// threshold. (A positive policy value is required: AutoStopAfterSec=0 now
	// means DISABLED, not "use default" — see finding #2 / TestAutoStopZeroMeansDisabled.)
	store.rows = []lifecycle.RunSummary{
		{
			ID:                     runID,
			UpdatedAt:              base.Add(-35 * time.Minute),
			PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds()),
		},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	if !stopper.wasStopped(runID) {
		t.Error("idle run should have been stopped")
	}
	if rec.len() != 1 {
		t.Errorf("expected 1 audit event, got %d", rec.len())
	}
	ev, _ := rec.last()
	if ev.Action != "run.autostop" {
		t.Errorf("want action=run.autostop, got %s", ev.Action)
	}
	if ev.ActorType != types.ActorSystem {
		t.Errorf("want actor_type=system, got %s", ev.ActorType)
	}
	if ev.Outcome != "success" {
		t.Errorf("want outcome=success, got %s", ev.Outcome)
	}
	if ev.RunID == nil || *ev.RunID != runID {
		t.Errorf("want run_id=%s in audit event", runID)
	}
}

// TestActiveRunUntouched: a run whose updated_at is within the default
// threshold must NOT be stopped.
func TestActiveRunUntouched(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	runID := uuid.New()

	// Run was active 10 minutes ago — still within its 30-minute policy window.
	// (Positive policy value: 0 now means DISABLED — see finding #2.)
	store.rows = []lifecycle.RunSummary{
		{
			ID:                     runID,
			UpdatedAt:              base.Add(-10 * time.Minute),
			PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds()),
		},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	if stopper.wasStopped(runID) {
		t.Error("active run should NOT have been stopped")
	}
	if rec.len() != 0 {
		t.Errorf("expected 0 audit events, got %d", rec.len())
	}
}

// TestPerPolicyOverrideRespected: a run with PolicyAutoStopAfterSec set uses
// that value instead of the platform default.
func TestPerPolicyOverrideRespected(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	// Platform default is 30 minutes; this run's policy says 5 minutes.
	policyStop := 5 * time.Minute

	idleRunID := uuid.New()   // idle 7m > 5m policy threshold
	activeRunID := uuid.New() // idle 3m < 5m policy threshold

	store.rows = []lifecycle.RunSummary{
		{
			ID:                     idleRunID,
			UpdatedAt:              base.Add(-7 * time.Minute),
			PolicyAutoStopAfterSec: int(policyStop.Seconds()),
		},
		{
			ID:                     activeRunID,
			UpdatedAt:              base.Add(-3 * time.Minute),
			PolicyAutoStopAfterSec: int(policyStop.Seconds()),
		},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	if !stopper.wasStopped(idleRunID) {
		t.Error("idle run (7m > 5m policy) should have been stopped")
	}
	if stopper.wasStopped(activeRunID) {
		t.Error("active run (3m < 5m policy) should NOT have been stopped")
	}
	if rec.len() != 1 {
		t.Errorf("expected 1 audit event, got %d", rec.len())
	}
}

// TestNeverReapWhenPolicyNegative: a run whose policy AutoStopAfterSec is
// negative is exempt from idle auto-stop entirely, no matter how long it has
// been idle. This is the never-reap escape hatch for unbounded interactive
// attach sessions.
func TestNeverReapWhenPolicyNegative(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	neverRunID := uuid.New()  // negative policy => never reaped
	normalRunID := uuid.New() // positive policy => idle past it, reaped

	store.rows = []lifecycle.RunSummary{
		{
			ID:                     neverRunID,
			UpdatedAt:              base.Add(-24 * time.Hour), // idle for a full day
			PolicyAutoStopAfterSec: -1,
		},
		{
			ID:                     normalRunID,
			UpdatedAt:              base.Add(-60 * time.Minute),
			PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds()),
		},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	if stopper.wasStopped(neverRunID) {
		t.Error("run with negative AutoStopAfterSec must NEVER be reaped, even when idle for a day")
	}
	if !stopper.wasStopped(normalRunID) {
		t.Error("normal idle run should still be stopped (never-reap is per-run, not global)")
	}
	if rec.len() != 1 {
		t.Errorf("expected exactly 1 audit event (for the normal run), got %d", rec.len())
	}
}

// TestStopperErrorDoesNotKillLoop: if StopRun returns an error for one run,
// the loop continues and stops the remaining eligible runs.
func TestStopperErrorDoesNotKillLoop(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	errorRunID := uuid.New()  // stopper returns an error for this one
	normalRunID := uuid.New() // stopper succeeds for this one

	stopper.errOn[errorRunID] = errors.New("container already gone")

	// Both runs are idle past their threshold. (Positive policy value: 0 now
	// means DISABLED — see finding #2.)
	store.rows = []lifecycle.RunSummary{
		{
			ID:                     errorRunID,
			UpdatedAt:              base.Add(-60 * time.Minute),
			PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds()),
		},
		{
			ID:                     normalRunID,
			UpdatedAt:              base.Add(-60 * time.Minute),
			PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds()),
		},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	// The error run was not recorded as stopped (stopper returned an error).
	if stopper.wasStopped(errorRunID) {
		t.Error("error run should not appear in stopped list")
	}
	// The normal run must still be stopped despite the earlier error.
	if !stopper.wasStopped(normalRunID) {
		t.Error("normal run should have been stopped even after error on other run")
	}
	// Only one audit event: for the run that succeeded.
	if rec.len() != 1 {
		t.Errorf("expected 1 audit event, got %d", rec.len())
	}
}

// TestStoreErrorIsLogged: if ListRunningWithPolicy fails, the tick returns
// without panicking and emits no spurious stops.
func TestStoreErrorIsLogged(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{err: errors.New("postgres unavailable")}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	r := makeReaper(store, stopper, rec, clk)
	// Must not panic.
	r.Tick(context.Background())

	if len(stopper.stopped) != 0 {
		t.Error("no runs should be stopped when store errors")
	}
	if rec.len() != 0 {
		t.Error("no audit events should be emitted when store errors")
	}
}

// TestPolicyThresholdBoundary: a run is stopped exactly when idle >= its policy
// threshold. A run idle for threshold-1s must NOT be stopped; a run idle for
// exactly threshold must be stopped. (Since 0 now means DISABLED — finding #2 —
// this boundary is exercised with a positive policy value rather than the
// platform default.)
func TestPolicyThresholdBoundary(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	policyStop := 30 * time.Minute
	safeRunID := uuid.New()
	expiredRunID := uuid.New()

	// safeRun is idle for exactly policyStop - 1s: must NOT be stopped.
	// expiredRun is idle for exactly policyStop: must be stopped (>= threshold).
	store.rows = []lifecycle.RunSummary{
		{
			ID:                     safeRunID,
			UpdatedAt:              base.Add(-(policyStop - time.Second)),
			PolicyAutoStopAfterSec: int(policyStop.Seconds()),
		},
		{
			ID:                     expiredRunID,
			UpdatedAt:              base.Add(-policyStop),
			PolicyAutoStopAfterSec: int(policyStop.Seconds()),
		},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	if stopper.wasStopped(safeRunID) {
		t.Error("run idle for threshold-1s should NOT be stopped")
	}
	if !stopper.wasStopped(expiredRunID) {
		t.Error("run idle for exactly the threshold should be stopped")
	}
}

// TestConditionalStopDoesNotEmitAutostopWhenTerminal covers finding #1: when a
// run was concurrently moved to a terminal state (kill/complete) before the
// reaper's stop applied, the conditional RUNNING->STOPPED transition is a no-op
// (StopRun returns applied=false). The reaper must NOT emit a spurious
// run.autostop audit event in that case — only the run whose stop actually
// applied should be audited.
func TestConditionalStopDoesNotEmitAutostopWhenTerminal(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	racedRunID := uuid.New()  // already terminal: StopRun applies nothing
	normalRunID := uuid.New() // genuinely stopped by the reaper

	// racedRunID lost the race: its conditional transition does not apply.
	stopper.noApplyOn[racedRunID] = true

	// Both runs look idle past their threshold to the reaper. (Positive policy
	// value: 0 now means DISABLED — see finding #2.)
	store.rows = []lifecycle.RunSummary{
		{ID: racedRunID, UpdatedAt: base.Add(-60 * time.Minute), PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds())},
		{ID: normalRunID, UpdatedAt: base.Add(-60 * time.Minute), PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds())},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	// Exactly one audit event: for the run whose stop actually applied. The raced
	// run must NOT produce a spurious run.autostop.
	if rec.len() != 1 {
		t.Fatalf("expected exactly 1 audit event (only the applied stop), got %d", rec.len())
	}
	ev, _ := rec.last()
	if ev.RunID == nil || *ev.RunID != normalRunID {
		t.Errorf("the single autostop event must be for the run whose stop applied (%s), got %v", normalRunID, ev.RunID)
	}
}

// TestAutoStopZeroMeansDisabled covers finding #2: a policy AutoStopAfterSec of
// 0 means DISABLED (never reap), NOT "use the platform default". A run with a
// zero policy must never be stopped no matter how long it has been idle, while a
// run with no policy override... is also 0, so it too is disabled. (The platform
// default now applies only when the reaper is given a positive default for runs
// whose policy is genuinely unset is out of scope here — 0 is unambiguously
// disabled per docs/policies.)
func TestAutoStopZeroMeansDisabled(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	disabledRunID := uuid.New() // policy 0 => disabled, never reaped
	activeRunID := uuid.New()   // positive policy, idle past it => reaped

	store.rows = []lifecycle.RunSummary{
		{
			ID:                     disabledRunID,
			UpdatedAt:              base.Add(-24 * time.Hour), // idle for a full day
			PolicyAutoStopAfterSec: 0,
		},
		{
			ID:                     activeRunID,
			UpdatedAt:              base.Add(-10 * time.Minute),
			PolicyAutoStopAfterSec: int((5 * time.Minute).Seconds()),
		},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	if stopper.wasStopped(disabledRunID) {
		t.Error("run with AutoStopAfterSec=0 must be DISABLED (never reaped), even when idle for a day")
	}
	if !stopper.wasStopped(activeRunID) {
		t.Error("run with a positive policy idle past its threshold should still be stopped")
	}
	if rec.len() != 1 {
		t.Errorf("expected exactly 1 audit event (for the positive-policy run), got %d", rec.len())
	}
}

// TestRevokeFailureEmitsRevokeFailureEvent covers finding N1: when an idle stop
// APPLIES (RUNNING->STOPPED) but the revoke cascade fails, the reaper must emit a
// DISTINCT run.revoke/failure audit event in addition to run.autostop, so the
// live-credential window is visible. A run.autostop/success alone would
// dishonestly read as a fully-contained stop.
func TestRevokeFailureEmitsRevokeFailureEvent(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	runID := uuid.New()

	// The stop applies but its identity revoke fails: the run token may still be
	// usable until its TTL even though the run is STOPPED.
	stopper.revokeErrOn[runID] = map[string]string{"identity_error": "revocation store unavailable"}

	store.rows = []lifecycle.RunSummary{
		{ID: runID, UpdatedAt: base.Add(-60 * time.Minute), PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds())},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	if !stopper.wasStopped(runID) {
		t.Fatal("run should have been stopped (the transition applied)")
	}
	// Exactly two events: run.autostop/success AND run.revoke/failure.
	if rec.len() != 2 {
		t.Fatalf("expected 2 audit events (autostop + revoke failure), got %d", rec.len())
	}
	var autostop, revoke *types.AuditEvent
	for i := range rec.events {
		switch rec.events[i].Action {
		case "run.autostop":
			autostop = &rec.events[i]
		case "run.revoke":
			revoke = &rec.events[i]
		}
	}
	if autostop == nil || autostop.Outcome != "success" {
		t.Errorf("want a run.autostop/success event, got %+v", autostop)
	}
	if revoke == nil {
		t.Fatal("want a distinct run.revoke event for the failed revoke; none emitted")
	}
	if revoke.Outcome != "failure" {
		t.Errorf("run.revoke outcome = %q, want failure", revoke.Outcome)
	}
	if revoke.ActorType != types.ActorSystem {
		t.Errorf("run.revoke actor_type = %q, want system", revoke.ActorType)
	}
	if revoke.RunID == nil || *revoke.RunID != runID {
		t.Errorf("run.revoke run_id = %v, want %s", revoke.RunID, runID)
	}
	if !strings.Contains(string(revoke.Data), "identity_error") {
		t.Errorf("run.revoke data should carry identity_error, got %s", revoke.Data)
	}
}

// TestSnapshotUpdatedAtThreadedToStopper covers the reaper half of finding N3:
// the reaper must thread the run's snapshot updated_at into StopRun as the
// idleness guard (the store CAS then no-ops a run touched after the snapshot —
// exercised end-to-end in the store PG test).
func TestSnapshotUpdatedAtThreadedToStopper(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	store := &fakeStore{}
	stopper := newFakeStopper()
	rec := &fakeRecorder{}

	runID := uuid.New()
	snapshotAt := base.Add(-60 * time.Minute)
	store.rows = []lifecycle.RunSummary{
		{ID: runID, UpdatedAt: snapshotAt, PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds())},
	}

	r := makeReaper(store, stopper, rec, clk)
	r.Tick(context.Background())

	stopper.mu.Lock()
	got, ok := stopper.notAfterSeen[runID]
	stopper.mu.Unlock()
	if !ok {
		t.Fatal("StopRun was not called for the idle run")
	}
	if !got.Equal(snapshotAt) {
		t.Errorf("notAfter threaded to StopRun = %v, want the snapshot updated_at %v", got, snapshotAt)
	}
}

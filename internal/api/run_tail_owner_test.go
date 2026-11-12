// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tailRow is one ended run in the tail owner's fake store.
type tailRow struct {
	run    types.AgentRun
	status types.RunLocalFolderStatus
	folder *types.RunLocalFolder
	seeded bool
	forced *time.Time
	fenced *time.Time
}

// tailLab is the store of the tail owner's multi-run pins: rows in memory with the listing's keyset order, the
// folder record and seed reads, the give-up statement and the audit mark. Any other store call panics on the nil
// embed.
type tailLab struct {
	store.Store
	mu        sync.Mutex
	rows      map[uuid.UUID]*tailRow
	runner    types.Runner
	unfenced  []bool // the unfencedOnly argument of each listing
	statusErr error
	seedErr   error
	giveUpOK  bool
	giveUpErr error
	giveUps   int
	markFails func(id uuid.UUID) bool
	markCalls map[uuid.UUID]int
}

func (l *tailLab) add(r *tailRow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows[r.run.ID] = r
}

func (l *tailLab) clearRef(id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows[id].run.SandboxRef = ""
}

func (l *tailLab) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.rows[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return r.run, nil
}

func (l *tailLab) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if id != l.runner.ID {
		return types.Runner{}, store.ErrNotFound
	}
	return l.runner, nil
}

func (l *tailLab) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

func (l *tailLab) ListTerminalSandboxPage(ctx context.Context, after *store.TerminalSandboxCursor, limit int, unfencedOnly bool) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error) {
	l.mu.Lock()
	l.unfenced = append(l.unfenced, unfencedOnly)
	listing := newContainStore()
	for _, r := range l.rows {
		listing.add(store.TerminalSandboxRow{ID: r.run.ID, State: r.run.State, SandboxRef: r.run.SandboxRef, RunnerID: r.run.RunnerID,
			MachineKind: r.run.MachineKind, EndedAt: r.run.EndedAt, UpdatedAt: r.run.UpdatedAt, ForcedStopAt: r.forced, FencedAt: r.fenced})
	}
	l.mu.Unlock()
	return listing.ListTerminalSandboxPage(ctx, after, limit, unfencedOnly)
}

func (l *tailLab) RunLocalFolderSeed(_ context.Context, id uuid.UUID) (*types.RunLocalFolder, store.LocalFolderSeed, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seedErr != nil {
		return nil, store.LocalFolderSeed{}, l.seedErr
	}
	r := l.rows[id]
	if r == nil {
		return nil, store.LocalFolderSeed{}, store.ErrNotFound
	}
	var seed store.LocalFolderSeed
	if r.seeded {
		at := r.run.UpdatedAt
		seed.SeededAt = &at
	}
	return r.folder, seed, nil
}

func (l *tailLab) SetRunLocalFolderSeed(context.Context, uuid.UUID, string) (bool, error) {
	return false, nil
}

func (l *tailLab) RunLocalFolderStatus(_ context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.statusErr != nil {
		return types.RunLocalFolderStatus{}, l.statusErr
	}
	r := l.rows[id]
	if r == nil {
		return types.RunLocalFolderStatus{}, store.ErrNotFound
	}
	return r.status, nil
}

func (l *tailLab) SetRunLocalFolderStatusIf(context.Context, uuid.UUID, store.FolderStatusGuard, types.RunLocalFolderStatus) (bool, error) {
	return false, nil
}

func (l *tailLab) BumpRunLocalFolderGen(context.Context, uuid.UUID, store.FolderStatusGuard, time.Time) (types.RunLocalFolderStatus, bool, error) {
	return types.RunLocalFolderStatus{}, false, nil
}

// GiveUpRunLocalFolder applies as the statement does for a run that is due when giveUpOK, and applies to nothing
// otherwise; giveUpErr fails it.
func (l *tailLab) GiveUpRunLocalFolder(_ context.Context, id uuid.UUID, p store.GiveUpParams) (store.GiveUpOutcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.giveUps++
	if l.giveUpErr != nil {
		return store.GiveUpOutcome{}, l.giveUpErr
	}
	r := l.rows[id]
	if !l.giveUpOK || r == nil {
		return store.GiveUpOutcome{}, nil
	}
	back := p.Back
	r.status.Resume, r.status.Back = nil, &back
	r.status.Gen++
	return store.GiveUpOutcome{Applied: true, Gen: r.status.Gen}, nil
}

func (l *tailLab) MarkGivenUpAudited(_ context.Context, id uuid.UUID, gen int) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.markCalls[id]++
	if l.markFails != nil && l.markFails(id) {
		return false, errors.New("the mark could not be written")
	}
	r := l.rows[id]
	if r == nil || r.status.Gen != gen || r.status.Back == nil || r.status.Back.GivenUpAt == nil {
		return false, nil
	}
	back := *r.status.Back
	back.GivenUpAudited = true
	r.status.Back = &back
	return true, nil
}

// ListGivenUpUnaudited pages by id with PG's cursor: the last id of a full page, nil after a short one.
func (l *tailLab) ListGivenUpUnaudited(_ context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, *uuid.UUID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ids []uuid.UUID
	for id, r := range l.rows {
		b := r.status.Back
		if b == nil || b.GivenUpAt == nil || b.GivenUpAudited {
			continue
		}
		if after != nil && bytes.Compare(id[:], after[:]) <= 0 {
			continue
		}
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	if len(ids) < limit {
		return ids, nil, nil
	}
	last := ids[len(ids)-1]
	return ids, &last, nil
}

func (l *tailLab) markTotal() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.markCalls {
		n += c
	}
	return n
}

func (l *tailLab) giveUpCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.giveUps
}

func (l *tailLab) audited(id uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.rows[id].status.Back
	return b != nil && b.GivenUpAudited
}

// tailCall is one tail the owner gave a run, as the rig's stand-in for finalizeRunEffects saw it.
type tailCall struct {
	id   uuid.UUID
	step tailStep
}

// tailRig is a Server over a tailLab, with a clock to move, a counting locker and a finalizeRunEffects stand-in
// that records the tail and clears the run's ref, as a stop that succeeded does. The folder runner is connected
// and online.
type tailRig struct {
	s      *Server
	lab    *tailLab
	audit  *recRecorder
	locks  *countingLocker
	clock  *keptEndClock
	online atomic.Bool
	mu     sync.Mutex
	calls  []tailCall
	during func(run types.AgentRun, step tailStep)
	stuck  map[uuid.UUID]bool // runs whose stop keeps failing: their tail runs and the ref stays
}

func newTailRig(t *testing.T) *tailRig {
	t.Helper()
	g := &tailRig{audit: &recRecorder{}, clock: &keptEndClock{now: time.Now()}, locks: &countingLocker{next: db.NewLocalLocker()},
		stuck: map[uuid.UUID]bool{}}
	g.online.Store(true)
	g.lab = &tailLab{rows: map[uuid.UUID]*tailRow{}, markCalls: map[uuid.UUID]int{},
		runner: types.Runner{ID: uuid.New(), Owner: "alice@example.com", Name: "laptop", State: types.RunnerClaimed}}
	g.s = New(Config{Store: g.lab, Audit: g.audit, Runner: containBare{}, ComputerFolders: true, Now: g.clock.Now,
		RunnerOnline: func(uuid.UUID) bool { return g.online.Load() }})
	g.s.locks.override = g.locks
	seamConnect(t, g.s, g.lab.runner.ID, runnerwire.Caps{Folders: true, FolderPassOnce: true})
	g.s.tail.finalizeFn = func(_ context.Context, run types.AgentRun, step tailStep) {
		g.mu.Lock()
		g.calls = append(g.calls, tailCall{id: run.ID, step: step})
		during, stuck := g.during, g.stuck[run.ID]
		g.mu.Unlock()
		if during != nil {
			during(run, step)
		}
		if step != tailFolder && !stuck { // a pass back keeps the sandbox; every other tail ends in a stop
			g.lab.clearRef(run.ID)
		}
	}
	return g
}

func (g *tailRig) tick() { g.s.folderCleanupTick(context.Background()) }

// count is how many tails of step were given.
func (g *tailRig) count(step tailStep) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if c.step == step {
			n++
		}
	}
	return n
}

// countOf is how many tails run id was given.
func (g *tailRig) countOf(id uuid.UUID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if c.id == id {
			n++
		}
	}
	return n
}

func (g *tailRig) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// historyRow is a stopped run that ended at ended, holds a sandbox and owes nothing: its record is final.
func (g *tailRig) historyRow(ended time.Time) *tailRow {
	at := ended
	r := &tailRow{run: types.AgentRun{ID: uuid.New(), State: types.RunStopped, CreatedBy: g.lab.runner.Owner, EndedAt: &at, UpdatedAt: at}}
	r.run.SandboxRef = "sbx-" + r.run.ID.String()
	r.status = types.RunLocalFolderStatus{Gen: 2, Settled: 2, Back: &types.FolderPassRecord{At: at, Refused: []types.FolderRefusal{}}}
	g.lab.add(r)
	return r
}

// owedRow is a stopped run that ended at ended with a both-ways seeded folder on the connected runner and a pass
// back owed (a resume).
func (g *tailRig) owedRow(ended time.Time) *tailRow {
	r := g.historyRow(ended)
	rid := g.lab.runner.ID
	r.folder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &rid, Direction: types.ComputerFolderBoth}
	r.seeded = true
	r.status = types.RunLocalFolderStatus{Gen: 3, Settled: 3, Resume: &types.FolderResume{Unsynced: 1},
		Back: &types.FolderPassRecord{At: ended, Refused: []types.FolderRefusal{}, Error: "the sync was cut short"}}
	return r
}

// givenUpRow is an ended run whose record was given up and never audited, its ref already cleared.
func (g *tailRig) givenUpRow() *tailRow {
	r := g.historyRow(g.clock.now.Add(-9 * 24 * time.Hour))
	r.run.SandboxRef = ""
	at := g.clock.now
	n := 2
	r.folder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", Direction: types.ComputerFolderBoth}
	r.status = types.RunLocalFolderStatus{Gen: 8, Settled: 8,
		Back: &types.FolderPassRecord{At: at, Refused: []types.FolderRefusal{}, Error: "given up", GivenUpAt: &at, GivenUpCount: &n}}
	return r
}

// transferAudit counts the run.folder.transfer rows written.
func transferAudit(audit *recRecorder) int {
	n := 0
	for _, ev := range audit.snapshot() {
		if ev.Action == "run.folder.transfer" {
			n++
		}
	}
	return n
}

func tailTestLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// A run the forced end owns is left to sweepForcedStops until its mark clears: the tail owner takes no lock and
// gives no tail, where it would otherwise tail the same run twice. Catches: dropping the forced-mark skip.
func TestTailOwner_SkipsForcedMarkedRows(t *testing.T) {
	g := newTailRig(t)
	marked := g.historyRow(g.clock.now.Add(-time.Hour))
	at := g.clock.now
	marked.forced = &at
	g.tick()
	if g.total() != 0 || g.locks.n.Load() != 0 {
		t.Fatalf("%d tails, %d lock attempts for a forced-marked run; want none", g.total(), g.locks.n.Load())
	}
	marked.forced = nil
	g.tick()
	if g.count(tailHistory) != 1 {
		t.Errorf("%d history tails once the mark cleared; want 1", g.count(tailHistory))
	}
}

// A run that ended less than containmentLag ago is the ordinary tail's: no lock, no tail. One whose due is exactly
// now, and one a second past it, are taken. Catches: dropping the age gate, which races the ordinary tail, and an
// inclusive gate turned exclusive.
func TestTailOwner_AgeGateSkipsAFreshEnd(t *testing.T) {
	g := newTailRig(t)
	fresh := g.historyRow(g.clock.now.Add(-containmentLag + time.Second))
	exact := g.historyRow(g.clock.now.Add(-containmentLag)) // due == now: taken
	due := g.historyRow(g.clock.now.Add(-containmentLag - time.Second))
	g.tick()
	if g.countOf(fresh.run.ID) != 0 || g.countOf(exact.run.ID) != 1 || g.countOf(due.run.ID) != 1 || g.total() != 2 {
		t.Fatalf("tails: fresh %d, due exactly now %d, past due %d, total %d; want 0, 1, 1, 2",
			g.countOf(fresh.run.ID), g.countOf(exact.run.ID), g.countOf(due.run.ID), g.total())
	}
	if n := g.locks.n.Load(); n != 2 {
		t.Errorf("%d lock attempts, want 2: the fresh end takes none", n)
	}
}

// 500 runs with nothing owed get 50 history tails a tick, and a pass back owed on the oldest of them still runs on
// the first tick. Catches: a history cap that ends the walk, or a step order that spends the tick on history.
func TestTailOwner_HistoryBacklogIsRateLimited(t *testing.T) {
	g := newTailRig(t)
	for i := range 500 {
		g.historyRow(g.clock.now.Add(-time.Hour - time.Duration(i)*time.Second))
	}
	g.owedRow(g.clock.now.Add(-24 * time.Hour))
	g.tick()
	if h, f := g.count(tailHistory), g.count(tailFolder); h != tailHistoryPerTick || f != 1 {
		t.Fatalf("tick 1: %d history tails and %d folder passes; want %d and 1", h, f, tailHistoryPerTick)
	}
	g.tick()
	if h := g.count(tailHistory); h != 2*tailHistoryPerTick {
		t.Errorf("after tick 2: %d history tails, want %d", h, 2*tailHistoryPerTick)
	}
}

// An owed pass whose keep ran out is given up and then stopped, in that order, with the one given_up row.
func TestTailOwner_ExpiredOwedGivesUpThenStops(t *testing.T) {
	g := newTailRig(t)
	g.lab.giveUpOK = true
	row := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
	var givenUpBefore bool
	g.during = func(types.AgentRun, tailStep) { givenUpBefore = g.lab.giveUpCount() == 1 }
	g.tick()
	if g.count(tailGiveUp) != 1 || !givenUpBefore {
		t.Fatalf("%d give-up tails, give-up statement before the stop %v; want 1 and true", g.count(tailGiveUp), givenUpBefore)
	}
	if st, _ := g.lab.RunLocalFolderStatus(context.Background(), row.run.ID); st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil {
		t.Errorf("record %+v; want given up with no resume", st)
	}
	if n := transferAudit(g.audit); n != 1 || !g.lab.audited(row.run.ID) {
		t.Errorf("%d given_up rows, audited %v; want 1 and true", n, g.lab.audited(row.run.ID))
	}
}

// A give-up that applied to nothing, or failed, is not final: the sandbox is not stopped, since the record may
// still be open and its changes not yet back. Catches: stopping on the call's return alone.
func TestTailOwner_GiveUpNotFinalDoesNotStop(t *testing.T) {
	for name, set := range map[string]func(*tailLab){
		"applied to nothing":  func(l *tailLab) { l.giveUpOK = false },
		"the statement fails": func(l *tailLab) { l.giveUpErr = errors.New("the database did not answer") },
	} {
		t.Run(name, func(t *testing.T) {
			g := newTailRig(t)
			set(g.lab)
			row := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
			g.tick()
			if g.lab.giveUpCount() != 1 {
				t.Fatalf("%d give-up statements, want 1", g.lab.giveUpCount())
			}
			if g.total() != 0 || row.run.SandboxRef == "" {
				t.Errorf("%d tails, ref %q; want no stop for a give-up that is not final", g.total(), row.run.SandboxRef)
			}
		})
	}
}

// A runner run whose runner is not connected here is not touched: no lock, no tail, no row, over 20 ticks.
func TestCleanup_OfflineRunnerTakesNoLockAndNoTail(t *testing.T) {
	g := newTailRig(t)
	away := uuid.New()
	for _, mk := range []func(time.Time) *tailRow{g.historyRow, g.owedRow} {
		mk(g.clock.now.Add(-9 * 24 * time.Hour)).run.RunnerID = &away
	}
	for range 20 {
		g.clock.now = g.clock.now.Add(time.Minute)
		g.tick()
	}
	if g.locks.n.Load() != 0 || g.total() != 0 || len(g.audit.snapshot()) != 0 || g.lab.giveUpCount() != 0 {
		t.Errorf("%d lock attempts, %d tails, %d rows, %d give-ups; want none",
			g.locks.n.Load(), g.total(), len(g.audit.snapshot()), g.lab.giveUpCount())
	}
}

// A run on a runner connected to another replica reads as not connected here, and is skipped.
func TestTailOwner_RunnerElsewhereSkipped(t *testing.T) {
	g := newTailRig(t)
	elsewhere := uuid.New()
	g.historyRow(g.clock.now.Add(-time.Hour)).run.RunnerID = &elsewhere
	g.tick()
	if g.locks.n.Load() != 0 || g.total() != 0 {
		t.Errorf("%d lock attempts, %d tails for a run whose runner is on another replica; want none", g.locks.n.Load(), g.total())
	}
}

// The runner's caps were seen once and it is no longer online: every step skips its runs before any lock or call,
// and takes them again when it is back. Catches: reading runnerLastCaps alone.
func TestTailOwner_StaleCapsOfAnOfflineRunnerTakeNoLock(t *testing.T) {
	g := newTailRig(t)
	g.lab.giveUpOK = true
	id := g.lab.runner.ID
	expired := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
	history := g.historyRow(g.clock.now.Add(-time.Hour))
	keep := g.owedRow(g.clock.now.Add(-2 * time.Hour))
	for _, r := range []*tailRow{expired, history, keep} {
		r.run.RunnerID = &id
	}
	g.online.Store(false)
	if _, ok := g.s.runnerLastCaps(id); !ok {
		t.Fatal("the last caps are gone; the pin would not tell stale caps apart")
	}
	for range 3 {
		g.tick()
	}
	if g.locks.n.Load() != 0 || g.total() != 0 || g.lab.giveUpCount() != 0 || len(g.audit.snapshot()) != 0 {
		t.Fatalf("%d lock attempts, %d tails, %d give-ups, %d rows with the runner offline; want none",
			g.locks.n.Load(), g.total(), g.lab.giveUpCount(), len(g.audit.snapshot()))
	}
	g.online.Store(true)
	g.tick()
	if g.count(tailGiveUp) != 1 || g.count(tailHistory) != 1 || g.count(tailFolder) != 1 {
		t.Errorf("back online: %d give-up, %d history, %d folder tails; want one each",
			g.count(tailGiveUp), g.count(tailHistory), g.count(tailFolder))
	}
}

// A fenced run still owes its tail: the listing includes it, and its owed pass runs. Catches: the unfenced-only
// listing.
func TestTailOwner_FencedOwedRunIsStillVisited(t *testing.T) {
	g := newTailRig(t)
	row := g.owedRow(g.clock.now.Add(-time.Hour))
	at := g.clock.now
	row.fenced = &at
	g.tick()
	if g.count(tailFolder) != 1 {
		t.Fatalf("%d folder passes for a fenced run with a pass owed; want 1", g.count(tailFolder))
	}
	g.lab.mu.Lock()
	defer g.lab.mu.Unlock()
	if len(g.lab.unfenced) == 0 || slices.Contains(g.lab.unfenced, true) {
		t.Errorf("listings asked unfencedOnly = %v; want every one false", g.lab.unfenced)
	}
}

// A folder record or seed that cannot be read skips the row for the tick: no lock, no give-up, no stop, a warn
// log; the next tick with good reads runs the step. Catches: a read error read as "not owed", which stops the
// sandbox at once.
func TestTailOwner_StatusReadErrorSkipsTheRow(t *testing.T) {
	g := newTailRig(t)
	g.lab.giveUpOK = true
	expired := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
	past := g.historyRow(g.clock.now.Add(-time.Hour))
	past.status = types.RunLocalFolderStatus{} // no pass has run: owed unless no folder says otherwise
	logs := tailTestLogs(t)
	for _, tc := range []struct {
		name string
		set  func()
	}{
		{"the status read fails", func() { g.lab.statusErr = errors.New("status down") }},
		{"the seed read fails", func() { g.lab.statusErr, g.lab.seedErr = nil, errors.New("seed down") }},
	} {
		logs.Reset() // each case must warn by itself, not on the one before's line
		tc.set()
		g.tick()
		if g.locks.n.Load() != 0 || g.total() != 0 || g.lab.giveUpCount() != 0 {
			t.Fatalf("%s: %d lock attempts, %d tails, %d give-ups; want none", tc.name, g.locks.n.Load(), g.total(), g.lab.giveUpCount())
		}
		if !strings.Contains(logs.String(), "could not read") || !strings.Contains(logs.String(), expired.run.ID.String()) {
			t.Errorf("%s: no warn names the run: %q", tc.name, logs.String())
		}
	}
	g.lab.seedErr = nil
	g.tick()
	if g.count(tailGiveUp) != 1 || g.count(tailHistory) != 1 || g.lab.giveUpCount() != 1 {
		t.Errorf("good reads: %d give-up tails, %d history tails, %d give-ups; want 1, 1, 1",
			g.count(tailGiveUp), g.count(tailHistory), g.lab.giveUpCount())
	}
}

// A folder pass that moved nothing waits tailBackoffMin, then twice that, up to tailBackoffMax; one that moved data
// clears the wait.
func TestCleanup_BackoffDoublesAndResets(t *testing.T) {
	g := newTailRig(t)
	row := g.owedRow(g.clock.now.Add(-time.Hour))
	now := g.clock.now
	attempts := 0
	for _, wait := range []time.Duration{tailBackoffMin, 2 * tailBackoffMin, 4 * tailBackoffMin, 8 * tailBackoffMin, tailBackoffMax, tailBackoffMax} {
		g.clock.now = now
		g.tick()
		attempts++
		if got := g.count(tailFolder); got != attempts {
			t.Fatalf("at the end of a %v wait: %d passes, want %d", wait, got, attempts)
		}
		g.clock.now = now.Add(wait - time.Second)
		g.tick()
		if got := g.count(tailFolder); got != attempts {
			t.Fatalf("a second before a %v wait ends: %d passes, want %d: the wait was not held", wait, got, attempts)
		}
		now = now.Add(wait)
	}
	g.during = func(types.AgentRun, tailStep) {
		g.lab.mu.Lock()
		defer g.lab.mu.Unlock()
		row.status.Gen++
		row.status.Back = &types.FolderPassRecord{At: now, Files: 3, Refused: []types.FolderRefusal{}}
	}
	g.clock.now = now
	g.tick()
	g.clock.now = now.Add(time.Second)
	g.tick()
	if got := g.count(tailFolder); got != attempts+2 {
		t.Errorf("after a pass that moved data: %d passes, want %d: the wait was not cleared", got, attempts+2)
	}
}

// A given-up record whose row was never accepted gets it, once, with its ref already cleared; a row the audit log
// did not take is written on a later pass with no mark before; a mark that failed after an accepted row is retried
// alone. Catches: a retry keyed on the ref, a mark on a rejected row, and a second row for an accepted one.
func TestTailOwner_GivenUpRowWrittenOnceWhenMissing(t *testing.T) {
	t.Run("a missing row", func(t *testing.T) {
		g := newTailRig(t)
		row := g.givenUpRow()
		g.tick()
		if n := transferAudit(g.audit); n != 1 || !g.lab.audited(row.run.ID) {
			t.Fatalf("tick 1: %d rows, audited %v; want 1 and true", n, g.lab.audited(row.run.ID))
		}
		g.tick()
		if n := transferAudit(g.audit); n != 1 {
			t.Errorf("tick 2: %d rows, want still 1", n)
		}
	})
	t.Run("the audit log takes nothing", func(t *testing.T) {
		g := newTailRig(t)
		row := g.givenUpRow()
		g.s.cfg.Audit = failingAudit{errors.New("audit down")}
		g.tick()
		if g.lab.markTotal() != 0 || g.lab.audited(row.run.ID) {
			t.Fatalf("a rejected row was marked: %d mark calls, audited %v", g.lab.markTotal(), g.lab.audited(row.run.ID))
		}
		g.s.cfg.Audit = g.audit
		g.tick()
		if n := transferAudit(g.audit); n != 1 || !g.lab.audited(row.run.ID) {
			t.Errorf("tick 2: %d rows, audited %v; want 1 and true", n, g.lab.audited(row.run.ID))
		}
	})
	t.Run("the mark fails after an accepted row", func(t *testing.T) {
		g := newTailRig(t)
		row := g.givenUpRow()
		g.lab.markFails = func(uuid.UUID) bool { return true }
		g.tick()
		g.tick()
		if n := transferAudit(g.audit); n != 1 || g.lab.markTotal() != 2 {
			t.Fatalf("two ticks with the mark failing: %d rows, %d mark calls; want 1 and 2: the mark alone is retried", n, g.lab.markTotal())
		}
		g.lab.markFails = nil
		g.tick()
		if n := transferAudit(g.audit); n != 1 || !g.lab.audited(row.run.ID) {
			t.Errorf("tick 3: %d rows, audited %v; want 1 and true", n, g.lab.audited(row.run.ID))
		}
	})
}

// 51 given-up records, refs cleared: the audit write is accepted for the 50 lowest ids and their marks fail. The
// cursor reaches the 51st on tick 2, then retries only the 50 marks; no row is written twice. Catches: a listing
// with no cursor (the 51st never written), a cursor that skips a failed id after the wrap, and a second row for an
// accepted one.
func TestTailOwner_GivenUpAuditCursorReachesRow51(t *testing.T) {
	g := newTailRig(t)
	var ids []uuid.UUID
	for range 51 {
		ids = append(ids, g.givenUpRow().run.ID)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	low := map[uuid.UUID]bool{}
	for _, id := range ids[:50] {
		low[id] = true
	}
	failing := true
	g.lab.markFails = func(id uuid.UUID) bool { return failing && low[id] }
	for i, want := range []struct{ rows, marks int }{{50, 50}, {51, 51}, {51, 101}} {
		g.tick()
		if n, m := transferAudit(g.audit), g.lab.markTotal(); n != want.rows || m != want.marks {
			t.Fatalf("tick %d: %d rows, %d mark calls; want %d and %d", i+1, n, m, want.rows, want.marks)
		}
	}
	if !g.lab.audited(ids[50]) {
		t.Fatal("the 51st record is not marked after tick 2")
	}
	failing = false
	g.tick()
	g.tick()
	for _, id := range ids {
		if !g.lab.audited(id) {
			t.Fatalf("record %s is not marked by tick 5", id)
		}
	}
	if n := transferAudit(g.audit); n != 51 {
		t.Errorf("%d given_up rows, want exactly 51", n)
	}
}

// tailOneStore is a single-run fixture store that also lists its run (ended, holding a ref) for the tail owner.
type tailOneStore struct {
	*computerFolderStore
	id uuid.UUID
}

func (o *tailOneStore) ListTerminalSandboxPage(ctx context.Context, after *store.TerminalSandboxCursor, _ int, _ bool) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error) {
	return onePage(ctx, o.computerFolderStore, o.id, after)
}

// onePage lists the one run of a fixture store when it is terminal and holds a ref.
func onePage(ctx context.Context, st interface {
	GetRun(context.Context, uuid.UUID) (types.AgentRun, error)
}, id uuid.UUID, after *store.TerminalSandboxCursor) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error) {
	run, err := st.GetRun(ctx, id)
	if err != nil || after != nil || !run.State.IsTerminal() || run.SandboxRef == "" {
		return nil, nil, err
	}
	return []store.TerminalSandboxRow{{ID: run.ID, State: run.State, SandboxRef: run.SandboxRef, RunnerID: run.RunnerID,
		MachineKind: run.MachineKind, EndedAt: run.EndedAt, UpdatedAt: run.UpdatedAt}}, nil, nil
}

// An owed record past the cap is given up, with no pass, and the sandbox stopped.
func TestResume_CapGivesUpThenStops(t *testing.T) {
	s, cs, audit, run := giveUpFixture(t)
	cs.giveUpApplies = true
	s.cfg.Store = &tailOneStore{computerFolderStore: cs, id: run.ID}
	s.folderCleanupTick(context.Background())
	st := cs.folderStatus()
	if st.Resume != nil || st.Back == nil || st.Back.GivenUpAt == nil {
		t.Fatalf("record %+v; want given up with no resume", st)
	}
	if rows := transferRows(audit, run); len(rows) != 1 {
		t.Errorf("%d transfer rows, want only the give-up's: no pass ran", len(rows))
	}
	if stops := s.cfg.Runner.(*folderSandboxRunner).stopCount(); stops != 1 {
		t.Errorf("%d sandbox stops, want 1", stops)
	}
}

// tailForcedStore is the forced fixture's store that also lists its run for the tail owner, and fails the folder
// record's read while failStatus is set.
type tailForcedStore struct {
	markedStore
	id         uuid.UUID
	failStatus atomic.Bool
}

func (w *tailForcedStore) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	if w.failStatus.Load() {
		return types.RunLocalFolderStatus{}, errors.New("the folder record could not be read")
	}
	return w.markedStore.RunLocalFolderStatus(ctx, id)
}

func (w *tailForcedStore) ListTerminalSandboxPage(ctx context.Context, after *store.TerminalSandboxCursor, _ int, _ bool) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error) {
	return onePage(ctx, w.markedStore, w.id, after)
}

// tailOwnedForced is f with its store listing the run for the tail owner.
func tailOwnedForced(f *forced) *tailForcedStore {
	w := &tailForcedStore{markedStore: f.s.cfg.Store.(markedStore), id: f.run.ID}
	f.s.cfg.Store = w
	return w
}

// A forced end kept the sandbox because the folder record could not be read: the ref stays and the mark clears.
// The record reads again and shows nothing owed, so the tail owner gives the run its tail: it stops the sandbox
// and clears the ref.
func TestTailOwner_ForcedRunWithoutFolderGetsItsTail(t *testing.T) {
	f := newForced(t)
	w := tailOwnedForced(f)
	w.failStatus.Store(true)
	f.end(f.s)
	f.release()
	f.tick(t, f.s)
	if run := f.terminalRun(t); f.sb.stopCount() != 0 || run.SandboxRef == "" || f.leaser.markCount() != 0 {
		t.Fatalf("stops %d, ref %q, marks %d; want the sandbox kept with its ref and the mark cleared", f.sb.stopCount(), run.SandboxRef, f.leaser.markCount())
	}
	w.failStatus.Store(false)
	f.withoutOwedPass()
	f.s.folderCleanupTick(context.Background())
	if run := f.terminalRun(t); f.sb.stopCount() != 1 || run.SandboxRef != "" {
		t.Errorf("stops %d, ref %q; want the tail owner to stop the sandbox and clear the ref", f.sb.stopCount(), run.SandboxRef)
	}
}

// A forced end under a held lock keeps the sandbox for an owed pass: no stop, a resume, and the mark stays while
// the lock is held. With the lock free the retry keeps the sandbox and clears the mark. The tail owner's next tick
// runs the pass under the lock and then stops the sandbox. A hosted container and a connected fenced runner.
func TestForcedEnd_OwedPassKeptThenBroughtBackByTheTailOwner(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *forced){
		"a hosted container": func(*testing.T, *forced) {},
		"a fenced runner": func(t *testing.T, f *forced) {
			f.onFencedRunner(t)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newForced(t)
			setup(t, f)
			tailOwnedForced(f)
			f.setRecord(owedRecord)
			f.end(f.s)
			if rec := f.cs.folderStatus(); f.sb.stopCount() != 0 || rec.Resume == nil || f.leaser.markCount() != 1 {
				t.Fatalf("stops %d, record %+v, marks %d; want no stop, a resume, and the mark held with the lock", f.sb.stopCount(), rec, f.leaser.markCount())
			}
			f.release()
			f.tick(t, f.s)
			if f.sb.stopCount() != 0 || f.leaser.markCount() != 0 {
				t.Fatalf("with the lock free: %d stops, %d marks; want the sandbox kept and the mark cleared", f.sb.stopCount(), f.leaser.markCount())
			}
			if id := f.cs.runner.ID; f.terminalRun(t).RunnerID != nil {
				f.shareFoldersFrom(t, id)
			}
			locks := &countingLocker{next: db.NewLocalLocker()}
			f.s.locks.override = locks
			var passes, lockFree, attemptsAtPass atomic.Int32
			f.sb.onPass = func() { // the runner's side of the pass: the run's lock must be held while it runs
				if passes.Add(1) == 1 {
					attemptsAtPass.Store(locks.n.Load())
				}
				if _, unlock, free := f.s.tryLockRunOp(context.Background(), f.run.ID); free {
					unlock()
					lockFree.Add(1)
				}
			}
			f.s.folderCleanupTick(context.Background())
			rec := f.cs.folderStatus()
			if f.sb.stopCount() != 1 || rec.Resume != nil || rec.Back == nil {
				t.Fatalf("tail owner tick: %d stops, record %+v; want the pass run, then one stop", f.sb.stopCount(), rec)
			}
			if passes.Load() == 0 || lockFree.Load() != 0 || attemptsAtPass.Load() == 0 {
				t.Errorf("the runner saw %d passes, %d with the run's lock free, %d lock attempts before the first; want a pass under a lock the tail owner took",
					passes.Load(), lockFree.Load(), attemptsAtPass.Load())
			}
			if rec.Back.Error != "" || rec.Back.Files == 0 || rec.Back.GivenUpAt != nil {
				t.Errorf("Back = %+v; want a pass with files and no error, not given up", rec.Back)
			}
			if got, _ := os.ReadFile(filepath.Join(f.cs.folder.Path, "README.md")); string(got) != "edited by the agent" {
				t.Errorf("README.md on the computer = %q; want the agent's edit brought back", got)
			}
		})
	}
}

// shareFoldersFrom makes the runner id the forced fixture's laptop runner again, with the fence and the folders
// it shares, rooted at the home the fixture's folder lives under.
func (f *forced) shareFoldersFrom(t *testing.T, id uuid.UUID) {
	t.Helper()
	home := filepath.Dir(filepath.Dir(f.cs.folder.Path))
	ep := folderRunnerEndpointCaps(t, id, runnerwire.Caps{Folders: true, FolderPassOnce: true, ProxyFence: true, MachineName: "alice-laptop"}, home)
	f.s.runnerStreams.subsMu.Lock()
	defer f.s.runnerStreams.subsMu.Unlock()
	f.s.runnerStreams.subs[id] = ep
}

// owedMarkedStore is the forced fixture's store with the revoke-owed mark, whose read answers set.
type owedMarkedStore struct {
	markedStore
	set bool
}

func (o owedMarkedStore) MarkRevokeOwed(context.Context, uuid.UUID) error  { return nil }
func (o owedMarkedStore) ClearRevokeOwed(context.Context, uuid.UUID) error { return nil }
func (o owedMarkedStore) ListRevokeOwedAfter(context.Context, time.Duration, *store.RevokeOwedCursor, int) ([]uuid.UUID, *store.RevokeOwedCursor, error) {
	return nil, nil, nil
}
func (o owedMarkedStore) RevokeOwedSet(context.Context, uuid.UUID) (bool, error) { return o.set, nil }

// A contained forced run whose revoke fails clears its mark when the run's revoke is owed to the revoke worker,
// and keeps it when it is not. Catches: the mark held forever by a revoke the worker retries.
func TestForcedMark_ClearsOnContainmentWhenRevokeOwed(t *testing.T) {
	for name, tc := range map[string]struct {
		set       bool
		wantMarks int
	}{
		"the revoke is owed":     {set: true, wantMarks: 0},
		"the revoke is not owed": {set: false, wantMarks: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newForced(t)
			f.withoutOwedPass()
			f.failingRevoke()
			f.s.cfg.Store = owedMarkedStore{markedStore: f.s.cfg.Store.(markedStore), set: tc.set}
			f.end(f.s)
			if got := f.leaser.markCount(); got != tc.wantMarks {
				t.Errorf("%d marks after a stop that succeeded and a revoke that failed; want %d", got, tc.wantMarks)
			}
		})
	}
}

// A hosted run whose folder runner is offline is not visited: over 20 ticks no lock, no tail, no row; the tick the
// runner is online again runs exactly one pass. Catches: a visitable check that is always true (a lock and a tail
// every tick against a runner that cannot answer), and one that is never true.
func TestTailOwner_OfflineFolderRunnerOfAHostedRunIsNotVisited(t *testing.T) {
	g := newTailRig(t)
	g.owedRow(g.clock.now.Add(-time.Hour)) // RunnerID nil: a hosted sandbox, owed to the laptop's folder runner
	g.online.Store(false)
	for range 20 {
		g.clock.now = g.clock.now.Add(time.Second)
		g.tick()
	}
	if g.locks.n.Load() != 0 || g.total() != 0 || len(g.audit.snapshot()) != 0 {
		t.Fatalf("%d lock attempts, %d tails, %d rows with the folder runner offline; want none", g.locks.n.Load(), g.total(), len(g.audit.snapshot()))
	}
	g.online.Store(true)
	g.tick()
	if g.count(tailFolder) != 1 || g.total() != 1 {
		t.Errorf("online again: %d folder passes of %d tails; want exactly one", g.count(tailFolder), g.total())
	}
}

// A folder runner refused for good (folders switched off, the runner gone, the runner no longer the run's
// creator's) is visited, so the pass records a stall and gives up after computerFolderStalls visits (rc.19's path:
// computerFolderPass's refusal, folderBackResume). One that is only away is not (the pin above). Catches: a
// visitable check that treats every refusal as away, which waits out the whole cap.
func TestTailOwner_PermanentFolderRunnerRefusalIsVisited(t *testing.T) {
	for name, refuse := range map[string]func(*tailRig, *tailRow){
		"folders switched off": func(g *tailRig, _ *tailRow) { g.s.cfg.ComputerFolders = false },
		"the runner is gone": func(_ *tailRig, row *tailRow) {
			gone := uuid.New()
			row.folder.RunnerID = &gone
		},
		"the runner is another person's": func(g *tailRig, _ *tailRow) { g.lab.runner.Owner = "mallory@example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			g := newTailRig(t)
			row := g.owedRow(g.clock.now.Add(-time.Hour))
			refuse(g, row)
			g.tick()
			if g.count(tailFolder) != 1 {
				t.Errorf("%d folder passes for a runner refused for good; want 1", g.count(tailFolder))
			}
		})
	}
}

// 50 runs whose stop keeps failing, newest ended, and one older run that is healthy: the older run gets its tail on
// tick 2 (tick 1 takes the 50, the position moves past them), and not before. Catches: a history walk that starts
// from the newest row every tick, which gives the stuck 50 the whole quota for ever.
func TestTailOwner_FailingStopsDoNotStarveAnOlderRow(t *testing.T) {
	g := newTailRig(t)
	for i := range tailHistoryPerTick {
		g.stuck[g.historyRow(g.clock.now.Add(-time.Hour-time.Duration(i)*time.Second)).run.ID] = true
	}
	healthy := g.historyRow(g.clock.now.Add(-2 * time.Hour))
	g.tick()
	if g.count(tailHistory) != tailHistoryPerTick || g.countOf(healthy.run.ID) != 0 {
		t.Fatalf("tick 1: %d history tails, %d for the healthy run; want %d and 0", g.count(tailHistory), g.countOf(healthy.run.ID), tailHistoryPerTick)
	}
	g.tick()
	if g.countOf(healthy.run.ID) != 1 || g.lab.rows[healthy.run.ID].run.SandboxRef != "" {
		t.Errorf("tick 2: %d tails for the healthy run, ref %q; want 1 and its ref cleared", g.countOf(healthy.run.ID), g.lab.rows[healthy.run.ID].run.SandboxRef)
	}
	g.tick() // the position wrapped to the newest: the stuck 50 are tried again, the healthy run is done
	if g.countOf(healthy.run.ID) != 1 || g.count(tailHistory) != 2*tailHistoryPerTick+1 {
		t.Errorf("tick 3: %d tails for the healthy run, %d history tails; want 1 and %d", g.countOf(healthy.run.ID), g.count(tailHistory), 2*tailHistoryPerTick+1)
	}
}

// A pass counts as moved data only when it is a new pass record: a stale Back that held files, with the generation
// moved on and no new record, is not, and the wait keeps doubling. Catches: a predicate that reads Files > 0 on the
// old Back, which resets the wait to a minute for ever.
func TestCleanup_StaleBackWithFilesDoesNotResetTheBackoff(t *testing.T) {
	g := newTailRig(t)
	row := g.owedRow(g.clock.now.Add(-time.Hour))
	row.status.Back.Files = 5
	g.during = func(types.AgentRun, tailStep) {
		g.lab.mu.Lock()
		defer g.lab.mu.Unlock()
		row.status.Gen++ // the generation moved, the record (its At) did not
	}
	now := g.clock.now
	attempts := 0
	for _, wait := range []time.Duration{tailBackoffMin, 2 * tailBackoffMin, 4 * tailBackoffMin} {
		g.clock.now = now
		g.tick()
		attempts++
		if got := g.count(tailFolder); got != attempts {
			t.Fatalf("at the end of a %v wait: %d passes, want %d", wait, got, attempts)
		}
		g.clock.now = now.Add(wait - time.Second)
		g.tick()
		if got := g.count(tailFolder); got != attempts {
			t.Fatalf("a second before a %v wait ends: %d passes, want %d: a stale Back reset the wait", wait, got, attempts)
		}
		now = now.Add(wait)
	}
}

// An expired owed run whose first give-up is accepted by the audit log but whose mark fails writes one given_up
// row across later ticks: only the mark is retried. Catches: the accepted set filled only by the later retry, which
// writes the row again at the second tick.
func TestTailOwner_FirstGiveUpWithAFailedMarkWritesOneRow(t *testing.T) {
	g := newTailRig(t)
	g.lab.giveUpOK = true
	g.lab.markFails = func(uuid.UUID) bool { return true }
	row := g.owedRow(g.clock.now.Add(-g.s.pendingCap() - time.Hour))
	for range 3 {
		g.tick()
	}
	if n := transferAudit(g.audit); n != 1 || g.lab.markTotal() < 3 {
		t.Fatalf("three ticks: %d given_up rows, %d mark calls; want 1 row and the mark retried", n, g.lab.markTotal())
	}
	g.lab.markFails = nil
	g.tick()
	if n := transferAudit(g.audit); n != 1 || !g.lab.audited(row.run.ID) {
		t.Errorf("mark healthy: %d rows, audited %v; want 1 and true", n, g.lab.audited(row.run.ID))
	}
}

// crashedBeforeItsTail is the forced fixture's run at the crash boundary of an ordinary end: the folder was seeded
// for real (holdForLocalFolder), and the run is committed terminal by the lease's own transition (StopKeptRunIf, the
// first half of stopKeptRun, run_lease.go:461), with the tail never called (:473) and the run's lock free.
func (f *forced) crashedBeforeItsTail(t *testing.T, to types.RunState) types.AgentRun {
	t.Helper()
	f.release()
	if applied, err := f.leaser.StopKeptRunIf(context.Background(), f.run.ID, to, f.run.LostAt, f.run.LostReason, f.run.EndsAt); err != nil || !applied {
		t.Fatalf("terminal transition = %v, %v; want applied", applied, err)
	}
	run, rec := f.terminalRun(t), f.cs.folderStatus()
	if run.State != to || run.SandboxRef == "" || rec.Seed == nil || rec.Back != nil || rec.Resume != nil {
		t.Fatalf("after the crash: state %s, ref %q, record %+v; want a terminal run with its ref and a seeded record with no Back and no Resume", run.State, run.SandboxRef, rec)
	}
	return run
}

// sweepPaths are the three destructive sweeps, each run on its own against run: the boot orphan pass
// (reconcileOrphanedRun), the label orphan sweep (reconcile.go isOrphan) and the terminal-sandbox sweep
// (stopTerminalSandbox). A sweep reports whether it tore the sandbox down.
var sweepPaths = map[string]func(*testing.T, *forced, types.AgentRun) bool{
	"the boot orphan pass": func(t *testing.T, f *forced, _ types.AgentRun) bool {
		before := f.sb.stopCount()
		if err := f.s.reconcileOrphanedSandbox(context.Background()); err != nil {
			t.Error(err)
		}
		return f.sb.stopCount() > before
	},
	"the label orphan sweep": func(t *testing.T, f *forced, _ types.AgentRun) bool {
		if err := f.s.sweepOrphanedSandboxes(context.Background()); err != nil {
			t.Error(err)
		}
		return f.s.cfg.Runner.(*proxyCounted).orphan
	},
	"the terminal-sandbox sweep": func(_ *testing.T, f *forced, run types.AgentRun) bool {
		before := f.sb.stopCount()
		f.s.sweepTerminalSandboxRun(context.Background(), run)
		return f.sb.stopCount() > before
	},
}

func newCrashed(t *testing.T) *forced {
	t.Helper()
	f := newForced(t)
	tailOwnedForced(f)
	f.s.cfg.Runner = &proxyCounted{passWindowSandbox: f.s.cfg.Runner.(*passWindowSandbox)}
	return f
}

// A seeded run committed terminal whose process died before its tail has no Back and no Resume. Each sweep that wins
// the free lock before the tail owner keeps its sandbox: no stop, no ref clear, and the terminal-sandbox sweep
// still cuts the proxy. The tail owner then brings the folder back: a pass the runner saw under the run's lock, a
// Back with files and no error, the agent's edit on the computer, then one stop. Catches: a sweep keep decided
// on Resume alone, which stops the sandbox before any copy back and loses the agent's changes.
func TestSweeps_KeepASeededRunThatCrashedBeforeItsTail(t *testing.T) {
	for name, sweep := range sweepPaths {
		t.Run(name, func(t *testing.T) {
			f := newCrashed(t)
			run := f.crashedBeforeItsTail(t, types.RunStopped)
			if sweep(t, f, run) {
				t.Fatalf("%s tore the sandbox down", name)
			}
			cur := f.terminalRun(t)
			if f.sb.stopCount() != 0 || cur.SandboxRef == "" {
				t.Fatalf("after %s: %d stops, ref %q; want no stop and the ref kept", name, f.sb.stopCount(), cur.SandboxRef)
			}
			if rn := f.s.cfg.Runner.(*proxyCounted); name == "the terminal-sandbox sweep" && rn.proxyStops.Load() == 0 {
				t.Error("the terminal-sandbox sweep kept the sandbox without cutting its proxy")
			}
			f.tailPassAfterCrash(t)
		})
	}
	t.Run("all three before the tail owner", func(t *testing.T) {
		f := newCrashed(t)
		run := f.crashedBeforeItsTail(t, types.RunStopped)
		for name, sweep := range sweepPaths {
			if sweep(t, f, run) {
				t.Fatalf("%s tore the sandbox down", name)
			}
		}
		if cur := f.terminalRun(t); f.sb.stopCount() != 0 || cur.SandboxRef == "" {
			t.Fatalf("after all three: %d stops, ref %q; want no stop and the ref kept", f.sb.stopCount(), cur.SandboxRef)
		}
		f.tailPassAfterCrash(t)
	})
}

// tailPassAfterCrash is the tail owner's tick over the crashed run: it brings the folder back under the run's lock
// and then stops the sandbox.
func (f *forced) tailPassAfterCrash(t *testing.T) {
	t.Helper()
	var passes, lockFree atomic.Int32
	f.sb.onPass = func() {
		passes.Add(1)
		if _, unlock, free := f.s.tryLockRunOp(context.Background(), f.run.ID); free {
			unlock()
			lockFree.Add(1)
		}
	}
	f.s.folderCleanupTick(context.Background())
	rec := f.cs.folderStatus()
	if f.sb.stopCount() != 1 || rec.Resume != nil || rec.Back == nil || rec.Back.Error != "" || rec.Back.Files == 0 {
		t.Fatalf("tail owner: %d stops, record %+v; want one pass back with files and no error, then one stop", f.sb.stopCount(), rec)
	}
	if passes.Load() == 0 || lockFree.Load() != 0 {
		t.Errorf("the runner saw %d passes, %d with the run's lock free; want a pass under the tail owner's lock", passes.Load(), lockFree.Load())
	}
	if got, _ := os.ReadFile(filepath.Join(f.cs.folder.Path, "README.md")); string(got) != "edited by the agent" {
		t.Errorf("README.md on the computer = %q; want the agent's edit brought back", got)
	}
	if run := f.terminalRun(t); run.SandboxRef != "" {
		t.Errorf("ref %q after the stop; want it cleared", run.SandboxRef)
	}
}

// The same crashed shape that owes nothing is still stopped by every sweep: a run with no pass back to make (its
// folder is one way), a killed run (never copied back) and a record given up. Catches: a keep broadened to every
// seeded row with no Back.
func TestSweeps_StillStopWhatOwesNoPassBack(t *testing.T) {
	givenUp := time.Now()
	for name, tc := range map[string]struct {
		to    types.RunState
		setup func(*forced)
	}{
		"a folder that is not copied back": {types.RunStopped, (*forced).withoutOwedPass},
		"a killed run":                     {types.RunKilled, func(*forced) {}},
		"a record given up": {types.RunStopped, func(f *forced) {
			f.setRecord(types.RunLocalFolderStatus{Gen: 4, Settled: 4, Seed: &types.FolderPassRecord{},
				Back: &types.FolderPassRecord{GivenUpAt: &givenUp, Error: "given up"}})
		}},
	} {
		for path, sweep := range sweepPaths {
			if tc.to == types.RunKilled && path == "the terminal-sandbox sweep" {
				continue // a killed run is first recovered by its own kill-tail check (recoverAbandonedKillTail)
			}
			t.Run(name+"/"+path, func(t *testing.T) {
				f := newCrashed(t)
				run := f.crashedBeforeItsTail(t, tc.to)
				tc.setup(f)
				if !sweep(t, f, run) {
					t.Errorf("%s left the sandbox of %s", path, name)
				}
			})
		}
	}
}

// reloseEnded reads a kept sandbox as proof that its stop was observed: a crashed seeded run with no Back and no
// Resume is not one, so it stays "ended, stop not confirmed"; one holding a resume is. Catches: reloseEnded moved to
// the sweeps' keep, which would claim a stop nobody saw.
func TestReloseEnded_ProofIsUnchangedForANoBackNoResumeRow(t *testing.T) {
	f := newCrashed(t)
	f.crashedBeforeItsTail(t, types.RunStopped)
	if fate, err := f.s.reloseEnded(context.Background(), f.run.ID); fate != relostEnded || err == nil {
		t.Fatalf("no Back, no Resume: fate %v, err %v; want ended with the stop not confirmed", fate, err)
	}
	f.setRecord(owedRecord)
	if fate, err := f.s.reloseEnded(context.Background(), f.run.ID); fate != relostEnded || err != nil {
		t.Errorf("a resume: fate %v, err %v; want ended with the stop confirmed", fate, err)
	}
}

// keepsSandboxForFolder's value list: what keeps a terminal run's sandbox from a sweep, what stops it, and what
// is no answer (an error, which every caller keeps on).
func TestKeepsSandboxForFolder_ValueList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(*tailRig, *tailRow)
		wantKeep bool
		wantErr  bool
	}{
		{"a pass back owed, nothing recorded yet", func(*tailRig, *tailRow) {}, true, false},
		{"a resume", func(_ *tailRig, r *tailRow) { r.status.Resume = &types.FolderResume{Unsynced: 1} }, true, false},
		{"owed past the cap (the tail owner gives up)", func(g *tailRig, r *tailRow) {
			end := g.clock.now.Add(-g.s.pendingCap() - time.Hour)
			r.run.EndedAt = &end
		}, true, false},
		{"a killed run", func(_ *tailRig, r *tailRow) { r.run.State = types.RunKilled }, false, false},
		{"a run still running", func(_ *tailRig, r *tailRow) { r.run.State = types.RunRunning }, false, false},
		{"no sandbox ref", func(_ *tailRig, r *tailRow) { r.run.SandboxRef = "" }, false, false},
		{"a record given up", func(g *tailRig, r *tailRow) {
			at := g.clock.now
			r.status.Back = &types.FolderPassRecord{GivenUpAt: &at}
		}, false, false},
		{"a pass back already made", func(_ *tailRig, r *tailRow) { r.status.Back = &types.FolderPassRecord{} }, false, false},
		{"a folder not copied back", func(_ *tailRig, r *tailRow) { r.folder.Direction = types.ComputerFolderIn }, false, false},
		{"a folder never seeded", func(_ *tailRig, r *tailRow) { r.seeded = false }, false, false},
		{"no folder", func(_ *tailRig, r *tailRow) { r.folder = nil }, false, false},
		{"no record row", func(g *tailRig, r *tailRow) { delete(g.lab.rows, r.run.ID) }, false, false},
		{"a record that cannot be read", func(g *tailRig, _ *tailRow) { g.lab.statusErr = errors.New("status down") }, false, true},
		{"a seed that cannot be read", func(g *tailRig, _ *tailRow) { g.lab.seedErr = errors.New("seed down") }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTailRig(t)
			row := g.owedRow(g.clock.now.Add(-time.Hour))
			row.status = types.RunLocalFolderStatus{Gen: 2, Settled: 2, Seed: &types.FolderPassRecord{At: row.run.UpdatedAt}}
			tc.change(g, row)
			kept, err := g.s.keepsSandboxForFolder(context.Background(), row.run)
			if kept != tc.wantKeep || (err != nil) != tc.wantErr {
				t.Errorf("keep = %v, err = %v; want keep %v, error %v", kept, err, tc.wantKeep, tc.wantErr)
			}
			if tc.wantErr && !g.s.sweepKeepsSandbox(context.Background(), row.run) {
				t.Error("a sweep stops a sandbox whose record it could not read")
			}
		})
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
}

func newTailRig(t *testing.T) *tailRig {
	t.Helper()
	g := &tailRig{audit: &recRecorder{}, clock: &keptEndClock{now: time.Now()}, locks: &countingLocker{next: db.NewLocalLocker()}}
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
		during := g.during
		g.mu.Unlock()
		if during != nil {
			during(run, step)
		}
		if step != tailFolder { // a pass back keeps the sandbox; every other tail ends in a stop
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

// A run that ended less than containmentLag ago is the ordinary tail's: no lock, no tail. One a second past it is
// taken. Catches: dropping the age gate, which races the ordinary tail.
func TestTailOwner_AgeGateSkipsAFreshEnd(t *testing.T) {
	g := newTailRig(t)
	fresh := g.historyRow(g.clock.now.Add(-containmentLag + time.Second))
	due := g.historyRow(g.clock.now.Add(-containmentLag - time.Second))
	g.tick()
	g.mu.Lock()
	calls := slices.Clone(g.calls)
	g.mu.Unlock()
	if len(calls) != 1 || calls[0].id != due.run.ID || calls[0].step != tailHistory {
		t.Fatalf("tails %+v; want one history tail, of the run past its due (fresh run %s)", calls, fresh.run.ID)
	}
	if n := g.locks.n.Load(); n != 1 {
		t.Errorf("%d lock attempts, want 1: the fresh end takes none", n)
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
			f.s.folderCleanupTick(context.Background())
			rec := f.cs.folderStatus()
			if f.sb.stopCount() != 1 || rec.Resume != nil || rec.Back == nil || locks.n.Load() == 0 {
				t.Errorf("tail owner tick: %d stops, record %+v, %d lock attempts; want the pass run under the lock, then one stop", f.sb.stopCount(), rec, locks.n.Load())
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

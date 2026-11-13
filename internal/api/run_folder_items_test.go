// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A pass back's record puts its outcome first and cuts only the reason: a 2000-rune runner reason cannot push
// how the sandbox ended (kept for a later sweep, or stopped with the count left behind) out of the record.
func TestFolderBackText_OutcomeSurvivesALongReason(t *testing.T) {
	long := strings.Repeat("é", 2000)
	bounded := strings.Repeat("é", maxFolderReason) + "..."

	_, kept := folderBackResume(nil, &runnerwire.FolderPass{}, long)
	const keptHead = "the sandbox is kept with its agent stopped and no egress, and a later sweep, about hourly, tries the sync again: "
	if kept != keptHead+bounded {
		t.Errorf("kept text = %d runes, want the outcome, then the reason cut to %d runes", len([]rune(kept)), maxFolderReason)
	}

	stopped := folderBackGivenUp(long, "2 passes in a row brought nothing back", 3)
	const stoppedHead = "the sandbox was stopped with changed files not brought back: 3, because 2 passes in a row brought nothing back: "
	if stopped != stoppedHead+bounded {
		t.Errorf("stopped text = %d runes, want the outcome, then the reason cut to %d runes", len([]rune(stopped)), maxFolderReason)
	}

	notPreserved := folderNotPreservedText(1, long, strings.Repeat("b", 2000))
	if n := len([]rune(notPreserved)); n > 120+2*(maxFolderReason+3)+4 || !strings.Contains(notPreserved, ": "+bounded) || !strings.HasSuffix(notPreserved, "; "+strings.Repeat("b", maxFolderReason)+"...") {
		t.Errorf("not-preserved text is %d runes, want each reason cut to %d runes", n, maxFolderReason)
	}

	for name, text := range map[string]string{"kept": kept, "stopped": stopped} {
		if got := folderPassRecord(time.Now(), runnerwire.FolderPass{}, text, true).Error; got != text {
			t.Errorf("%s: the record cut the text to %d runes; want all %d, outcome included", name, len([]rune(got)), len([]rune(text)))
		}
	}
}

// The count of what a stopped sandbox leaves behind comes first, before the cause.
func TestFolderBackText_GivenUpCountFirst(t *testing.T) {
	for _, tc := range []struct {
		unsynced int
		want     string
	}{
		{0, "the sandbox was stopped with its changes not brought back yet, uncounted, because why: reason"},
		{1, "the sandbox was stopped with changed files not brought back: 1, because why: reason"},
	} {
		if got := folderBackGivenUp("reason", "why", tc.unsynced); got != tc.want {
			t.Errorf("unsynced %d: got %q, want %q", tc.unsynced, got, tc.want)
		}
	}
}

// boundReason cuts at maxFolderReason runes and marks the cut: 0 runes is the empty-message line, 300 is kept
// whole, 301 and 2000 are cut to 300 and the marker.
func TestBoundReason_Boundaries(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "the sync reported a failure without a message"},
		{maxFolderReason, strings.Repeat("x", maxFolderReason)},
		{maxFolderReason + 1, strings.Repeat("x", maxFolderReason) + "..."},
		{2000, strings.Repeat("x", maxFolderReason) + "..."},
	} {
		if got := boundReason(strings.Repeat("x", tc.n)); got != tc.want {
			t.Errorf("%d runes: got %d runes %q, want %q", tc.n, len([]rune(got)), got, tc.want)
		}
	}
}

// bumpFailStore is computerFolderStore whose bump fails without landing (bumpErr), counting the record writes.
type bumpFailStore struct {
	*computerFolderStore
	bumpErr error
	sets    atomic.Int32
}

func (s *bumpFailStore) BumpRunLocalFolderGen(context.Context, uuid.UUID, store.FolderStatusGuard, time.Time) (types.RunLocalFolderStatus, bool, error) {
	return types.RunLocalFolderStatus{}, false, s.bumpErr
}

// SetSandboxRef keeps the ref the tail clears, so a test can tell a confirmed teardown (as refStore).
func (s *bumpFailStore) SetSandboxRef(_ context.Context, _ uuid.UUID, ref string) error {
	s.dispatchTestStore.mu.Lock()
	defer s.dispatchTestStore.mu.Unlock()
	s.run.SandboxRef = ref
	return nil
}

func (s *bumpFailStore) SetRunLocalFolderStatusIf(ctx context.Context, id uuid.UUID, guard store.FolderStatusGuard, st types.RunLocalFolderStatus) (bool, error) {
	s.sets.Add(1)
	return s.computerFolderStore.SetRunLocalFolderStatusIf(ctx, id, guard, st)
}

// bumpMissedText is what a pass back records when its store could not move the folder record before its pass.
const bumpMissedText = "no pass ran; the folder record could not be moved before its pass: its store could not write it; the changes were not copied back by this attempt, uncounted"

// A pass back whose record cannot be moved to the pass's generation stops the sandbox, and says why: one failure
// row and one try at the record, at the generation it was read at. A silent stop leaves the run saying nothing.
func TestFolderBack_BumpStoreErrorRecordsAndAudits(t *testing.T) {
	f := newPassFixture(t, 2, 2, nil)
	bs := &bumpFailStore{computerFolderStore: f.cs, bumpErr: errors.New("the bump failed")}
	f.s.cfg.Store = bs
	if err := f.finalizeWithin(30 * time.Second); errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the sandbox left to its stop", err)
	}
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, bumpMissedText)
	// The text is written before the stop, so it claims none.
	if st := f.cs.folderStatus(); st.Back == nil || strings.Contains(st.Back.Error, "stopped") {
		t.Errorf("back record %+v, want the pre-stop text with no stop claim", st.Back)
	}
	if ev := findAuditData(f.audit.snapshot(), f.run.ID, "run.folder.transfer", bumpMissedText); ev == nil || strings.Contains(string(ev.Data), "stopped") {
		t.Errorf("audit row %v, want the pre-stop text with no stop claim", ev)
	}
	if n := f.backRows(); n != 1 {
		t.Errorf("%d audit rows of passes back, want 1", n)
	}
	if n := bs.sets.Load(); n != 1 {
		t.Errorf("%d record writes, want 1", n)
	}
	if st := f.cs.folderStatus(); st.Gen != 2 || st.Settled != 2 {
		t.Errorf("record %+v, want it at gen 2, settled as before: the pass did not own a generation", st)
	}
}

// failingStopRunner is the folder sandbox runner whose stop fails.
type failingStopRunner struct {
	*folderSandboxRunner
	stops atomic.Int32
}

func (r *failingStopRunner) StopSandbox(context.Context, string) error {
	r.stops.Add(1)
	return errors.New("daemon unreachable")
}

// A bump fault is recorded before the stop, so the record and its audit row cannot claim a stop that then
// fails: the sandbox is still there, its ref stays, and neither text says it was stopped.
func TestFolderBack_BumpFaultThenFailedTeardownClaimsNoStop(t *testing.T) {
	f := newPassFixture(t, 2, 2, nil)
	bs := &bumpFailStore{computerFolderStore: f.cs, bumpErr: errors.New("the bump failed")}
	f.s.cfg.Store = bs
	stopper := &failingStopRunner{folderSandboxRunner: f.sb}
	f.s.cfg.Runner = stopper
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.run.SandboxRef = "sbx"
	ds.mu.Unlock()
	_ = f.finalizeWithin(30 * time.Second)
	if n := stopper.stops.Load(); n == 0 {
		t.Fatal("the stop was never tried")
	}
	st := f.cs.folderStatus()
	if st.Back == nil || !strings.Contains(st.Back.Error, bumpMissedText) || strings.Contains(st.Back.Error, "stopped") {
		t.Errorf("back record %+v, want the bump text and no stop claim", st.Back)
	}
	if ev := findAuditData(f.audit.snapshot(), f.run.ID, "run.folder.transfer", bumpMissedText); ev == nil || ev.Outcome != "failure" || strings.Contains(string(ev.Data), "stopped") {
		t.Errorf("audit row %v, want a failure with the bump text and no stop claim", ev)
	}
	if got, _ := bs.GetRun(context.Background(), f.run.ID); got.SandboxRef != "sbx" {
		t.Errorf("sandbox ref %q after a failed stop, want it kept", got.SandboxRef)
	}
}

// bumpComputerFolderGen's log tells a store error from a clean miss: only the miss "moved" before its pass.
func TestBumpLog_StoreErrorIsNotMoved(t *testing.T) {
	f := newPassFixture(t, 2, 2, nil)
	var logged lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(prev)

	f.s.cfg.Store = &bumpFailStore{computerFolderStore: f.cs, bumpErr: errors.New("the bump failed")}
	if _, ok, lost := f.s.bumpComputerFolderGen(context.Background(), f.run.ID, 2); ok || lost {
		t.Errorf("a store error answered ok %v lost %v, want neither", ok, lost)
	}
	out := logged.String()
	if !strings.Contains(out, "could not be moved before its pass") || !strings.Contains(out, "the bump failed") || strings.Contains(out, "pass record moved before its pass") {
		t.Errorf("a store error logged %q, want it as not moved, with the error", out)
	}

	f.s.cfg.Store = f.cs
	if _, ok, lost := f.s.bumpComputerFolderGen(context.Background(), f.run.ID, 5); ok || !lost {
		t.Errorf("a miss answered ok %v lost %v, want a clean miss", ok, lost)
	}
	if out := logged.String(); !strings.Contains(out[strings.LastIndex(out, "level=WARN"):], "pass record moved before its pass") {
		t.Errorf("a miss logged %q, want it as moved", out)
	}
}

// A store fault while recording a busy pass keeps the sandbox: the runner's busy reply proves another pass for
// the run is live. The fault is logged apart, naming the keep; a record another owner moved on is not a fault.
func TestBusy_StoreFaultKeepsAndLogs(t *testing.T) {
	s, cs, audit, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	rec, ok, _ := s.bumpComputerFolderGen(context.Background(), run.ID, 1)
	if !ok {
		t.Fatalf("bump = %+v %v", rec, ok)
	}
	var logged lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(prev)

	s.cfg.Store = &folderFaultStore{computerFolderStore: cs, setErr: errors.New("the write failed")}
	if !s.recordFolderPassBusy(context.Background(), run.ID, *cs.folder, rec.Gen) {
		t.Error("a store fault stopped the sandbox of a pass the runner still runs")
	}
	out := logged.String()
	if !strings.Contains(out, "recording a busy folder pass failed; the sandbox is kept because the runner is still running a pass for it") || !strings.Contains(out, "the write failed") {
		t.Errorf("the fault logged %q, want the keep and the error", out)
	}
	if st := cs.folderStatus(); st.Resume != nil {
		t.Errorf("record %+v, want the failed write to leave no resume", st)
	}
	if findAudit(audit.snapshot(), run.ID, "run.folder.transfer", "failure") != nil {
		t.Error("a busy pass whose record did not land was audited")
	}

	// A record another owner moved on is a miss, not a fault: kept, and not logged as one.
	s.cfg.Store = cs
	if !s.recordFolderPassBusy(context.Background(), run.ID, *cs.folder, rec.Gen+5) {
		t.Error("a record moved by another owner stopped the sandbox")
	}
	if more := strings.TrimPrefix(logged.String(), out); strings.Contains(more, "recording a busy folder pass failed") {
		t.Errorf("a miss logged as a fault: %q", more)
	}
}

// scriptedFolderRunner is a folder runner whose pass has already ended when the server comes for its stream:
// sync_folder answers a stream id no call opened, and sync_folder_wait answers account. sent is the last
// sync_folder call's arguments.
type scriptedFolderRunner struct {
	account runnerwire.FolderPass
	mu      sync.Mutex
	sent    runnerwire.SyncFolderArgs
}

func (r *scriptedFolderRunner) args() runnerwire.SyncFolderArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

// endpoint connects r as the folder runner id, sharing folders.
func (r *scriptedFolderRunner) endpoint(t *testing.T, id uuid.UUID) runnerEndpoint {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	org, run := runnerwire.Loopback()
	link := &remote.Link{}
	sub := remote.New(id.String(), link, remote.Options{})
	orgPeer := link.Attach(ctx, org)
	peer := runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(_ context.Context, _ uint32, method string, raw json.RawMessage) (any, error) {
		switch method {
		case runnerwire.MethodSyncFolder:
			r.mu.Lock()
			defer r.mu.Unlock()
			if err := json.Unmarshal(raw, &r.sent); err != nil {
				return nil, err
			}
			return runnerwire.StreamResult{Stream: 4242}, nil
		case runnerwire.MethodSyncFolderWait:
			return r.account, nil
		}
		return nil, runnerwire.Refuse("scripted folder runner")
	}})
	go func() { _ = peer.Run(ctx) }()
	t.Cleanup(func() { orgPeer.Close(); peer.Close() })
	data, _ := json.Marshal(runnerwire.Caps{Folders: true, FolderPassOnce: true, MachineName: "alice-laptop"})
	if err := peer.Event(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := sub.Caps(); ok {
			return runnerEndpoint{sub: sub, link: link}
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's caps never arrived")
		}
	}
}

// scriptedFolderFixture is a computer folder run whose runner is a scriptedFolderRunner.
func scriptedFolderFixture(t *testing.T, account runnerwire.FolderPass) (*Server, *computerFolderStore, types.AgentRun, *scriptedFolderRunner) {
	t.Helper()
	s, cs, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	r := &scriptedFolderRunner{account: account}
	id := cs.runner.ID
	s.runnerStreams.subsMu.Lock()
	s.runnerStreams.subs[id] = r.endpoint(t, id)
	s.runnerStreams.subsMu.Unlock()
	run.SandboxRef = "sbx"
	return s, cs, run, r
}

// A pass that ended before the server took its stream is settled, with the runner's own account: its reason is
// the pass's error and its counts stand, so a copy back is recorded for what it did, not for a missing stream.
func TestComputerFolderPass_PassEndedIsSettled(t *testing.T) {
	account := runnerwire.FolderPass{Pulled: 1, Unsynced: 2, Error: "the pass ran out of time"}
	s, cs, run, _ := scriptedFolderFixture(t, account)
	got, cut, settled, err := s.computerFolderPass(context.Background(), context.Background(), run, *cs.folder, true, passGuard{})
	if err != nil || cut || !settled {
		t.Fatalf("computerFolderPass = cut %v settled %v err %v, want the pass settled, not cut, no error", cut, settled, err)
	}
	if got.Error != account.Error || got.Pulled != 1 || got.Unsynced != 2 {
		t.Errorf("the pass = %+v, want the runner's account %+v", got, account)
	}
}

// The server sends each pass the deadline of the pass ctx it runs on, unchanged, for a seed and a pass back
// alike, and none without one: computerFolderPassWithin has already ended that ctx one account window before the
// pass's end, so a second subtraction here would take the window off twice.
func TestFolderPass_SendsItsDeadline(t *testing.T) {
	dl := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name     string
		pull     bool
		deadline bool
		want     int64
	}{
		{"back", true, true, dl.UnixMilli()},
		{"seed", false, true, dl.UnixMilli()},
		{"back without one", true, false, 0},
		{"seed without one", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cs, run, r := scriptedFolderFixture(t, runnerwire.FolderPass{})
			ctx := context.Background()
			if tc.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, dl)
				defer cancel()
			}
			if _, _, settled, err := s.computerFolderPass(ctx, ctx, run, *cs.folder, tc.pull, passGuard{}); err != nil || !settled {
				t.Fatalf("computerFolderPass: settled %v, err %v", settled, err)
			}
			if got := r.args(); got.DeadlineUnixMs != tc.want || got.Pull != tc.pull {
				t.Errorf("the runner was sent pull %v, deadline %d; want pull %v, deadline %d", got.Pull, got.DeadlineUnixMs, tc.pull, tc.want)
			}
		})
	}
}

// Through computerFolderPassWithin, an 8 s pass back is sent the pass end less one account window and no less:
// the window comes off once, in the pass ctx, and not again when the deadline is sent.
func TestFolderPassWithin_RunnerGetsTheComposedDeadline(t *testing.T) {
	if computerFolderAccountTime != 5*time.Second {
		t.Fatalf("computerFolderAccountTime = %v: this pin names 5 s", computerFolderAccountTime)
	}
	s, cs, run, r := scriptedFolderFixture(t, runnerwire.FolderPass{})
	before := time.Now()
	out := s.computerFolderPassWithin(context.Background(), 8*time.Second, run, *cs.folder, true, passGuard{})
	after := time.Now()
	if !out.settled || out.errText != "" {
		t.Fatalf("the pass = settled %v, error %q", out.settled, out.errText)
	}
	got := r.args().DeadlineUnixMs
	lo := before.Add(8*time.Second - computerFolderAccountTime).UnixMilli()
	hi := after.Add(8*time.Second - computerFolderAccountTime).UnixMilli()
	if got < lo || got > hi {
		t.Errorf("the runner was sent deadline %d, want the pass end less one account window: between %d and %d", got, lo, hi)
	}
}

// A teardown on a ctx whose run lock the database no longer holds is not called, and the run says nothing
// is confirmed gone: the runners' stops do not all refuse a ctx that has ended.
func TestTeardownOrAudit_LostLockDoesNotCall(t *testing.T) {
	was := lockLost
	defer func() { lockLost = was }()
	s := &Server{cfg: Config{Now: time.Now}}
	var calls int
	teardown := func(context.Context, string) error { calls++; return nil }
	lockLost = func(context.Context) bool { return true }
	if s.teardownOrAudit(context.Background(), uuid.New(), "sbx", "run.complete", teardown) || calls != 0 {
		t.Errorf("lost lock: teardown called %d times, or reported gone; want 0 calls and not gone", calls)
	}
	lockLost = func(context.Context) bool { return false }
	if !s.teardownOrAudit(context.Background(), uuid.New(), "sbx", "run.complete", teardown) || calls != 1 {
		t.Errorf("held lock: %d calls; want the teardown made once and the sandbox gone", calls)
	}
}

// heldRunLock takes a real run-op lock for run on a throwaway Postgres and returns the ctx that holds it, with
// the parent it was taken under, and a kill that ends the lock's session on the server.
func heldRunLock(t *testing.T, pool *pgxpool.Pool, run uuid.UUID) (hctx context.Context, parent context.CancelFunc, kill func()) {
	t.Helper()
	old := db.LockWatchInterval
	db.LockWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { db.LockWatchInterval = old })
	k := db.NewLockKey(db.RunOpLockClass, run.String())
	pctx, cancel := context.WithCancel(context.Background())
	hctx, release, err := db.NewPGLocker(pool, 2).Lock(pctx, k, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release(); cancel() })
	return hctx, cancel, func() {
		if _, err := pool.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks
			WHERE locktype = 'advisory' AND objsubid = 2 AND classid::bigint = $1 AND objid::bigint = $2
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
			int64(uint32(k.Class)), int64(uint32(k.Obj))); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); db.LockHeld(hctx) == nil; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the lock's loss was never seen")
			}
		}
	}
}

// A finalize whose run lock the database lost does not stop the sandbox, though the runner's stop would act on any
// ctx: nothing is called, the ref stays, and the caller hears the lock was lost. Also when the lock's parent ctx
// was cancelled first, which hides a later loss from the ctx's cause.
func TestFinalize_LostLockDoesNotStop(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "lock lost", true: "parent cancelled first"}[parentFirst], func(t *testing.T) {
			pool, cfg, _, run, _, probe := runnerExitAPIFixture(t)
			srv := New(cfg)
			hctx, cancelParent, kill := heldRunLock(t, pool, run.ID)
			if parentFirst {
				cancelParent()
			}
			kill()
			if parentFirst && !errors.Is(context.Cause(hctx), context.Canceled) {
				t.Fatalf("cause after the loss = %v, want the parent's cancel to mask it", context.Cause(hctx))
			}
			ref := "runner:" + run.ID.String()
			if got, err := cfg.Store.GetRun(t.Context(), run.ID); err == nil {
				ref = got.SandboxRef
			}
			err := srv.finalizeRunEffects(hctx, run.ID, ref, "run.complete", nil, false, false)
			if !errors.Is(err, db.ErrLockLost) {
				t.Errorf("finalize = %v, want it to say the lock was lost", err)
			}
			if n := probe.calls.Load(); n != 0 {
				t.Errorf("StopSandbox reached %d times, want 0", n)
			}
			if got, _ := cfg.Store.GetRun(t.Context(), run.ID); got.SandboxRef != ref {
				t.Errorf("sandbox ref %q, want it kept as %q", got.SandboxRef, ref)
			}
		})
	}
}

// A finalize whose caller's ctx was only cancelled, its lock still held, still stops the sandbox and clears its
// ref: the guard asks the database, not the ctx.
func TestFinalize_PlainCancelStillStops(t *testing.T) {
	pool, cfg, _, run, _, probe := runnerExitAPIFixture(t)
	srv := New(cfg)
	hctx, cancelParent, _ := heldRunLock(t, pool, run.ID)
	got, err := cfg.Store.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelParent()
	if err := srv.finalizeRunEffects(hctx, run.ID, got.SandboxRef, "run.complete", nil, false, false); errors.Is(err, db.ErrLockLost) {
		t.Errorf("finalize = %v, want a held lock not read as lost", err)
	}
	if n := probe.calls.Load(); n != 1 {
		t.Errorf("StopSandbox reached %d times, want 1", n)
	}
	if after, _ := cfg.Store.GetRun(t.Context(), run.ID); after.SandboxRef != "" {
		t.Errorf("sandbox ref %q after a stop, want it cleared", after.SandboxRef)
	}
}

// A copy back whose computer's runner is offline at the end sends no pass and keeps a sandbox that can be held
// (here a container whose quiesce worked): a resume is written, nothing is counted or stopped, and the record
// and its audit row say the copy back is tried again when the runner is back. A refusal that still settled it
// would stop the sandbox with the changes lost.
func TestFolderBack_OfflineFolderRunnerKeepsAHoldableSandbox(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	f.runnerAs(t, nil)
	seen := passSeen(f.sb)
	err := f.finalizeWithin(30 * time.Second)
	if !errors.Is(err, errComputerFolderResumes) || f.sb.stopCount() != 0 {
		t.Fatalf("finalize = %v, stops %d; want the sandbox kept", err, f.sb.stopCount())
	}
	if seen.Load() {
		t.Error("a pass was sent to a runner that is offline")
	}
	st := f.cs.folderStatus()
	if st.Resume == nil || !reflect.DeepEqual(*st.Resume, types.FolderResume{}) || st.Gen != 2 || st.Settled != 2 || st.Back == nil || st.Back.Error != folderPassAwayMsg {
		t.Errorf("record %+v, want a resume with nothing counted, settled at gen 2, with the runner-offline text", st)
	}
	if ev := findAuditData(f.audit.snapshot(), f.run.ID, "run.folder.transfer", folderPassAwayMsg); ev == nil || ev.Outcome != "failure" {
		t.Errorf("audit row %v, want a failure row with the runner-offline text", ev)
	}
}

// offlineExecRunner is a runner whose agent cannot be asked about: the exec that reads PID 1 fails, as it does
// on a sandbox on the very laptop that is offline.
type offlineExecRunner struct{ *agentSandbox }

func (r *offlineExecRunner) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if len(spec.Argv) == 2 && spec.Argv[1] == "/proc/1/cmdline" {
		return nil, runner.ErrRunnerOffline
	}
	return r.agentSandbox.ExecStream(ctx, ref, spec)
}

// A sandbox that cannot be held is stopped as before with its changes not brought back: a virtual machine, and a
// sandbox on the offline laptop itself, whose agent cannot be reached to stop it alone.
func TestFolderBack_OfflineFolderRunnerStopsAnUnholdableSandbox(t *testing.T) {
	for name, setup := range map[string]func(*passFixture, *testing.T) string{
		"a virtual machine": func(f *passFixture, _ *testing.T) string {
			ds := f.cs.localFolderStore.dispatchTestStore
			ds.mu.Lock()
			ds.run.MachineKind = types.MachineLinuxVM
			ds.mu.Unlock()
			return "a virtual machine's agent cannot be stopped alone"
		},
		"a sandbox on the offline laptop": func(f *passFixture, t *testing.T) string {
			ag := f.onARunner(t, false)
			f.s.cfg.Runner = &offlineExecRunner{ag}
			return "its agent is not one that can be stopped alone"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPassFixture(t, 1, 1, nil)
			f.runnerAs(t, nil)
			why := setup(f, t)
			if err := f.finalizeWithin(30 * time.Second); errors.Is(err, errComputerFolderResumes) {
				t.Errorf("finalize = %v, want the sandbox left to its stop", err)
			}
			for _, want := range []string{"no pass ran; the computer's runner was offline at the end", why, "its changes not brought back yet, uncounted"} {
				assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, want)
			}
			if st := f.cs.folderStatus(); st.Resume != nil || st.Settled != 2 {
				t.Errorf("record %+v, want no resume, settled at gen 2", st)
			}
		})
	}
}

// seedReadAfterFirst is computerFolderStore whose folder seed reads after the first answer no folder and err: an
// end's eligibility read answers, and its own read of the folder just after it does not, or finds none.
type seedReadAfterFirst struct {
	*computerFolderStore
	err   error
	reads atomic.Int32
}

func (s *seedReadAfterFirst) RunLocalFolderSeed(ctx context.Context, id uuid.UUID) (*types.RunLocalFolder, store.LocalFolderSeed, error) {
	if s.reads.Add(1) > 1 {
		return nil, store.LocalFolderSeed{}, s.err
	}
	return s.computerFolderStore.RunLocalFolderSeed(ctx, id)
}

// statusReadFails is computerFolderStore whose pass record never reads.
type statusReadFails struct{ *computerFolderStore }

func (s statusReadFails) RunLocalFolderStatus(context.Context, uuid.UUID) (types.RunLocalFolderStatus, error) {
	return types.RunLocalFolderStatus{}, errors.New("the record does not decode")
}

// An end that cannot read its computer folder's settings or its run's data_out rule has no answer on whether a pass
// back is owed. It keeps the sandbox as the sweeps would (folderKeepVerdict): on a confirmed cut, with no pass run and
// the record left as it was, so the tail owner runs the pass once the reads answer. Past the keep it is still kept: the
// tail owner gives the changes up durably before anything stops it. A virtual machine, a sandbox whose cut fails
// (audited under the end's own action), a killed run, a record whose pass already ran and a second read that finds no
// folder are stopped as before. Catches the unreadable read taken as "no folder" at either of the end's two reads,
// which stops the sandbox with the changes in it and nothing recorded.
func TestFolderBack_UnreadableFolderSettingsKeepOnAConfirmedCut(t *testing.T) {
	down := errors.New("database down")
	for name, tc := range map[string]struct {
		setup func(*passFixture)
		kept  bool
	}{
		"the folder seed cannot be read":  {func(f *passFixture) { f.cs.seedErr = down }, true},
		"the data out rule is unreadable": {func(f *passFixture) { f.cs.policyErr = down }, true},
		"the second seed read fails": {func(f *passFixture) {
			f.s.cfg.Store = &seedReadAfterFirst{computerFolderStore: f.cs, err: down}
		}, true},
		"the second seed read finds no folder": {func(f *passFixture) {
			f.s.cfg.Store = &seedReadAfterFirst{computerFolderStore: f.cs}
		}, false},
		"a virtual machine": {func(f *passFixture) {
			f.cs.policyErr = down
			ds := f.cs.localFolderStore.dispatchTestStore
			ds.mu.Lock()
			ds.run.MachineKind = types.MachineLinuxVM
			ds.mu.Unlock()
		}, false},
		"a cut that fails": {func(f *passFixture) {
			f.cs.policyErr = down
			f.sb.proxyErr = errors.New("the proxy did not stop")
		}, false},
		"a killed run": {func(f *passFixture) {
			f.cs.seedErr = down
			ds := f.cs.localFolderStore.dispatchTestStore
			ds.mu.Lock()
			ds.state = types.RunKilled
			ds.mu.Unlock()
		}, false},
		"past the keep": {func(f *passFixture) {
			f.cs.seedErr = down
			f.advance(f.s.pendingCap() + time.Minute)
		}, true},
		"a pass already recorded": {func(f *passFixture) {
			f.cs.seedErr = down
			f.set(func(st *types.RunLocalFolderStatus) {
				st.Back = folderPassRecord(f.s.cfg.Now(), runnerwire.FolderPass{}, "an earlier pass's error", true)
			})
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPassFixture(t, 1, 1, nil)
			ds := f.cs.localFolderStore.dispatchTestStore
			ds.mu.Lock()
			ds.state = types.RunCompleted
			ds.mu.Unlock()
			tc.setup(f)
			before := f.cs.folderStatus()
			seen := passSeen(f.sb)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := f.s.finalizeRunEffects(ctx, f.run.ID, "sbx", "sandbox.orphan.sweep", nil, false, false)
			if kept := errors.Is(err, errComputerFolderResumes); kept != tc.kept {
				t.Errorf("finalize = %v; want kept %v", err, tc.kept)
			}
			wantStops := 1
			if tc.kept {
				wantStops = 0
			}
			if n := f.sb.stopCount(); n != wantStops {
				t.Errorf("StopSandbox called %d times, want %d", n, wantStops)
			}
			if tc.kept && !f.sb.proxyStopped.Load() {
				t.Error("kept with its proxy not stopped: the cut was not confirmed")
			}
			if seen.Load() {
				t.Error("a pass was sent with the folder's settings unread")
			}
			if st := f.cs.folderStatus(); !reflect.DeepEqual(st, before) {
				t.Errorf("record %+v, want it unchanged from %+v", st, before)
			}
			if f.sb.proxyErr != nil {
				ev := findAudit(f.audit.snapshot(), f.run.ID, "sandbox.orphan.sweep", "failure")
				if ev == nil || !strings.Contains(string(ev.Data), "the proxy did not stop") {
					t.Errorf("cut failure row %v, want one under the end's own action naming the proxy's error", ev)
				}
			}
		})
	}
}

// The tail owner's give-up tail, which runs only once giveUpFolderNoted has made the record final, stops the sandbox
// and sends no pass, through the real finalizeRunEffects, even while the record and the folder's settings still
// cannot be read. Any other tail with the same reads keeps the sandbox on its confirmed cut for the tail owner, which
// gives the changes up durably before it stops it. Catches the give-up's finalize deciding the sandbox again from
// reads that fail, which keeps it on each confirmed cut, past the give-up, for ever.
func TestTailFinalize_AGiveUpStopsASandboxWhoseFolderCannotBeRead(t *testing.T) {
	for name, tc := range map[string]struct {
		step tailStep
		kept bool
	}{
		"the give-up's tail":    {tailGiveUp, false},
		"a folder visit's tail": {tailFolder, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPassFixture(t, 1, 1, nil)
			ds := f.cs.localFolderStore.dispatchTestStore
			ds.mu.Lock()
			ds.state = types.RunCompleted
			ds.mu.Unlock()
			f.cs.seedErr = errors.New("database down")
			f.s.cfg.Store = statusReadFails{f.cs}
			seen := passSeen(f.sb)
			run := f.run
			run.State, run.SandboxRef = types.RunCompleted, "sbx"
			f.s.tailFinalize(context.Background(), run, tc.step)
			wantStops := 1
			if tc.kept {
				wantStops = 0
			}
			if n := f.sb.stopCount(); n != wantStops {
				t.Errorf("StopSandbox called %d times, want %d", n, wantStops)
			}
			if seen.Load() {
				t.Error("a pass was sent from a tail whose folder cannot be read")
			}
		})
	}
}

// A pass back with no time left to start, because the caller's deadline was too near or its ctx was cancelled, sends
// nothing and keeps a sandbox that can be held (here a container whose quiesce worked), as a runner that is away
// does: a resume with nothing counted, settled at its generation, and the record and its audit row say Wardyn tries
// again. Catches a no-time pass recorded as done with an error, which stops the sandbox with the changes in it, and
// one counted as a stall on a resume visit, which gives the changes up at the second.
func TestFolderBack_NoTimeLeftKeepsAHoldableSandbox(t *testing.T) {
	t.Run("the caller's deadline leaves no time", func(t *testing.T) {
		f := newPassFixture(t, 1, 1, nil)
		seen := passSeen(f.sb)
		err := f.finalizeWithin(10 * time.Second)
		if !errors.Is(err, errComputerFolderResumes) || f.sb.stopCount() != 0 {
			t.Fatalf("finalize = %v, stops %d; want the sandbox kept", err, f.sb.stopCount())
		}
		if seen.Load() {
			t.Error("a pass was sent with no time left")
		}
		st := f.cs.folderStatus()
		if st.Resume == nil || !reflect.DeepEqual(*st.Resume, types.FolderResume{}) || st.Gen != 2 || st.Settled != 2 || st.Back == nil || st.Back.Error != folderPassNoTimeMsg {
			t.Errorf("record %+v, want a resume with nothing counted, settled at gen 2, with the no-time text", st)
		}
		if ev := findAuditData(f.audit.snapshot(), f.run.ID, "run.folder.transfer", folderPassNoTimeMsg); ev == nil || ev.Outcome != "failure" {
			t.Errorf("audit row %v, want a failure row with the no-time text", ev)
		}
		if v := f.s.computerFolderRunView(context.Background(), f.run.ID); v == nil || v.Back == nil || v.Back.Error != folderPassNoTimeMsg {
			t.Errorf("GET /runs/{id} computer_folder %+v, want back.error to be the no-time text verbatim", v)
		}
	})
	t.Run("a cancelled caller on a resume visit", func(t *testing.T) {
		f := newPassFixture(t, 2, 2, &types.FolderResume{Stalled: 1})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := f.s.finalizeRunEffects(ctx, f.run.ID, "sbx", "run.complete", nil, false, false)
		if !errors.Is(err, errComputerFolderResumes) || f.sb.stopCount() != 0 {
			t.Fatalf("finalize = %v, stops %d; want the sandbox kept", err, f.sb.stopCount())
		}
		st := f.cs.folderStatus()
		if st.Resume == nil || st.Resume.Stalled != 1 || st.Gen != 3 || st.Settled != 3 || st.Back == nil || st.Back.Error != folderPassNoTimeMsg {
			t.Errorf("record %+v, want the resume's one stall unchanged, settled at gen 3, with the no-time text", st)
		}
	})
}

// A pass back with no time left on a sandbox that cannot be held (a virtual machine) is stopped as before, with its
// changes not brought back, and the record says no pass ran for want of time.
func TestFolderBack_NoTimeLeftStopsAnUnholdableSandbox(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	ds := f.cs.localFolderStore.dispatchTestStore
	ds.mu.Lock()
	ds.run.MachineKind = types.MachineLinuxVM
	ds.mu.Unlock()
	if err := f.finalizeWithin(10 * time.Second); errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the sandbox left to its stop", err)
	}
	for _, want := range []string{"no pass ran; no time was left for it when the run ended", "a virtual machine's agent cannot be stopped alone", "its changes not brought back yet, uncounted"} {
		assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, want)
	}
	if st := f.cs.folderStatus(); st.Resume != nil || st.Settled != 2 {
		t.Errorf("record %+v, want no resume, settled at gen 2", st)
	}
}

// ctxExecSandbox is agentSandbox whose execs fail on a done ctx, as a runner's do.
type ctxExecSandbox struct{ *agentSandbox }

func (r ctxExecSandbox) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.agentSandbox.ExecStream(ctx, ref, spec)
}

// The keeps for a runner still busy with the last pass, and for one that may still run it, stop the agent of a
// sandbox on a runner on a teardown ctx: a cancelled caller (a daemon shutdown) still gets the agent stopped and the
// sandbox kept. Catches the agent stop asked on the caller's ctx, whose exec then fails, which stops the sandbox with
// its changes in it.
func TestFolderBack_BusyAndUnconfirmedKeepsStopTheAgentForACancelledCaller(t *testing.T) {
	for name, keep := range map[string]func(*passFixture, context.Context, types.AgentRun) bool{
		"busy": func(f *passFixture, ctx context.Context, run types.AgentRun) bool {
			return f.s.keepBusy(ctx, run, *f.cs.folder, 1, notQuiescable("on a runner"))
		},
		"unconfirmed": func(f *passFixture, ctx context.Context, run types.AgentRun) bool {
			rec := f.cs.folderStatus()
			return f.s.keepUnconfirmed(ctx, run, *f.cs.folder, 1, rec, rec.GenAt, notQuiescable("on a runner"))
		},
		"no time": func(f *passFixture, ctx context.Context, run types.AgentRun) bool {
			return f.s.keepNoTime(ctx, run, *f.cs.folder, 1, notQuiescable("on a runner"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPassFixture(t, 1, 1, nil)
			ag := f.onARunner(t, false)
			f.s.cfg.Runner = ctxExecSandbox{ag}
			run, err := f.s.cfg.Store.GetRun(context.Background(), f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			run.SandboxRef = "sbx"
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if !keep(f, ctx, run) {
				t.Error("not kept: the agent stop ran on the cancelled caller's ctx")
			}
			if ag.stops != 1 || f.sb.stopCount() != 0 {
				t.Errorf("agent stops %d, sandbox stops %d; want the agent stopped once and the sandbox kept", ag.stops, f.sb.stopCount())
			}
		})
	}
}

// recordCtxStore is computerFolderStore noting each guarded record write's ctx deadline.
type recordCtxStore struct {
	*computerFolderStore
	mu        sync.Mutex
	deadlines []time.Time
}

func (s *recordCtxStore) SetRunLocalFolderStatusIf(ctx context.Context, id uuid.UUID, guard store.FolderStatusGuard, st types.RunLocalFolderStatus) (bool, error) {
	dl, _ := ctx.Deadline()
	s.mu.Lock()
	s.deadlines = append(s.deadlines, dl)
	s.mu.Unlock()
	return s.computerFolderStore.SetRunLocalFolderStatusIf(ctx, id, guard, st)
}

// A pass back's record is written on a ctx made after the pass, not before its quiesce: for a caller with no deadline
// (the completion watcher, a person's bring-back) that ctx is reconcileFinalizeTimeout, and a pass of up to
// computerFolderBackTimeout would find a ctx made before it already done, losing the record and stopping the sandbox.
// The record's deadline must be at least reconcileFinalizeTimeout after the pass started, compared on the monotonic
// clock with no margin. Catches the record's ctx made before the pass.
func TestFolderBack_RecordCtxStartsAfterThePass(t *testing.T) {
	f := newPassFixture(t, 1, 1, nil)
	rs := &recordCtxStore{computerFolderStore: f.cs}
	f.s.cfg.Store = rs
	var passAt time.Time
	f.sb.onPass = func() {
		rs.mu.Lock()
		passAt = time.Now()
		rs.mu.Unlock()
	}
	_ = f.back()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if passAt.IsZero() || len(rs.deadlines) == 0 {
		t.Fatalf("pass at %v, record deadlines %v: want a pass and its record", passAt, rs.deadlines)
	}
	if last := rs.deadlines[len(rs.deadlines)-1]; last.Before(passAt.Add(reconcileFinalizeTimeout)) {
		t.Errorf("record deadline %v is before the pass's start %v plus %s: its ctx was made before the pass", last, passAt, reconcileFinalizeTimeout)
	}
}

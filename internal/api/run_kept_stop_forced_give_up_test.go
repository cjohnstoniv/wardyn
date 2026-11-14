// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refAtGiveUpStore is the forced fixture's store noting the run's ref as each give-up is made.
type refAtGiveUpStore struct {
	markedStore
	refAt *[]string
}

func (s refAtGiveUpStore) GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, p store.GiveUpParams) (store.GiveUpOutcome, error) {
	cs := s.forcedMarks.cs
	cs.dispatchTestStore.mu.Lock()
	*s.refAt = append(*s.refAt, cs.run.SandboxRef)
	cs.dispatchTestStore.mu.Unlock()
	return s.markedStore.GiveUpRunLocalFolder(ctx, id, p)
}

// The forced stop's retry that stops a sandbox kept for its computer folder, on a cut it could not confirm (its
// proxy stop fails), gives the owed copy back up before it clears the run's ref, on the locked path (the lock
// free) and on the path that runs while another operation holds the lock: the give-up is made while the ref is
// still set, due at once (a zero cap), its final record keeps the count and names of the changes left behind, and
// its run.folder.transfer given_up row is written; then the ref is cleared with one success row and the mark
// clears. A give-up that leaves the record open keeps the ref and the mark, with no given_up row and no success
// row, and still runs the run's revoke cascade; the next tick, the cut still not confirmed and the give-up applying,
// finishes it. Catches: a ref cleared while the record still shows the changes owed, with no given_up row; a mark
// cleared on a record not final, which leaves nothing to try again; a record not final that skips the revoke.
func TestSweepForcedStops_GivesUpAnOwedCopyBackBeforeClearingTheRef(t *testing.T) {
	owed := types.RunLocalFolderStatus{Gen: 3, Settled: 3, Resume: &types.FolderResume{Unsynced: 3, Counted: true, Paths: []string{"a.txt", "b.txt"}}}
	for _, held := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			rec     types.RunLocalFolderStatus
			applies bool
			giveUps int
		}{
			{"applied", owed, true, 1},
			{"left open", owed, false, 1},
		} {
			t.Run(map[bool]string{false: "the lock free", true: "the lock held"}[held]+"/"+tc.name, func(t *testing.T) {
				f := newForced(t)
				b := &togglingRevokeBroker{}
				f.s.cfg.Broker = b
				var refAt []string
				f.s.cfg.Store = refAtGiveUpStore{markedStore: f.s.cfg.Store.(markedStore), refAt: &refAt}
				f.claimOnly(t)
				if !held {
					f.release()
				}
				f.setRecord(tc.rec)
				f.cs.giveUpApplies = tc.applies
				f.sb.proxyErr = errors.New("daemon unavailable") // the cut is not confirmed, so the sandbox is stopped
				ref := f.terminalRun(t).SandboxRef
				f.tick(t, f.s)
				if f.sb.stopCount() != 1 {
					t.Fatalf("%d sandbox stops; want 1", f.sb.stopCount())
				}
				if len(f.cs.giveUps) != tc.giveUps || slices.ContainsFunc(refAt, func(r string) bool { return r != ref }) {
					t.Fatalf("%d give-ups, refs at give-up %v; want %d, each made while the ref was %q", len(f.cs.giveUps), refAt, tc.giveUps, ref)
				}
				if tc.giveUps == 1 {
					p := f.cs.giveUps[0]
					if p.Cap != 0 || p.D1 || p.Back.GivenUpAt == nil || p.Back.GivenUpCount == nil || *p.Back.GivenUpCount != 3 ||
						!slices.Equal(p.Back.GivenUpPaths, []string{"a.txt", "b.txt"}) || p.Back.GivenUpPathsMore != 1 {
						t.Errorf("give-up %+v; want a zero cap, no containment terms, and the record given up with 3 changes, 2 named and 1 more", p)
					}
				}
				final := tc.applies
				check := func(when string, final bool) {
					t.Helper()
					rows := 0
					for _, ev := range f.audit.snapshot() {
						if ev.Action == "run.folder.transfer" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), "changed files not brought back: 3") {
							rows++
						}
					}
					want := map[bool]int{false: 0, true: 1}[final]
					cleared := f.terminalRun(t).SandboxRef == ""
					if rows != want || cleared != final || f.leaser.markCount() != 1-want || len(sweepRows(f.audit, "success", "sandbox_stopped")) != want {
						t.Errorf("%s: %d given_up rows, ref cleared %v, %d marks, %d success rows; want %d, %v, %d, %d", when, rows, cleared,
							f.leaser.markCount(), len(sweepRows(f.audit, "success", "sandbox_stopped")), want, final, 1-want, want)
					}
				}
				check("the first tick", final)
				if b.calls.Load() == 0 {
					t.Error("no revoke on the first tick; the run's revoke cascade runs whether or not the record is final")
				}
				if tc.name != "left open" {
					return
				}
				f.cs.giveUpApplies = true
				f.tick(t, f.s)
				check("the next tick, the give-up applying", true)
			})
		}
	}
}

// The forced stop's retry that stops a sandbox it cannot keep (its cut is not confirmed), for a run whose copy back
// never ran and whose folder seed, data_out rule or folder record never reads, gives the copy back up only once its
// keep has run out: each give-up carries the keep as its cap, for the database to decide, and the note of the read
// that failed. Inside the keep it applies nothing, and the ref and the mark stay; past it the record is given up, the
// ref is cleared with one success row, the mark clears, and the next tick stops nothing. Its given_up row is written
// with the give-up, and names no folder when the seed is the read that fails. Catches: a read that never answers
// keeping the ref and the mark for good, with the sandbox stopped again on every tick; a give-up inside the keep,
// which throws away a copy back that a later read could still bring; a given_up row that waits for a seed that
// never reads.
func TestSweepForcedStops_GivesUpAFolderThatNeverReadsOnceItsKeepHasRunOut(t *testing.T) {
	down := errors.New("the database did not answer")
	for _, tc := range []struct {
		name, note string
		row        string // the key its given_up row carries
		named      bool   // the row names the folder
		set        func(f *forced)
	}{
		{"its folder seed", folderRuleUnreadable, "rule_unreadable", false, func(f *forced) { f.cs.seedErr = down }},
		{"its data out rule", folderRuleUnreadable, "rule_unreadable", true, func(f *forced) { f.cs.policyErr = down }},
		{"its folder record", folderStatusUnreadable, "status_unreadable", true, func(f *forced) {
			f.s.cfg.Store = unreadableRecordStore{f.s.cfg.Store.(markedStore)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForced(t)
			f.claimOnly(t)
			f.release()
			f.setRecord(types.RunLocalFolderStatus{Gen: 3, Settled: 3})
			tc.set(f)
			f.sb.proxyErr = errors.New("daemon unavailable") // the cut is not confirmed, so the sandbox is stopped
			check := func(when string, stops int, past bool) {
				t.Helper()
				for _, p := range f.cs.giveUps {
					if p.Cap != f.s.pendingCap() || p.D1 || !strings.HasSuffix(p.Back.Error, tc.note) {
						t.Errorf("%s: give-up %+v; want the keep as its cap, no containment terms, and the failed read noted", when, p)
					}
				}
				want := map[bool]int{false: 0, true: 1}[past]
				rows := 0
				for _, ev := range f.audit.snapshot() {
					if ev.Action == "run.folder.transfer" && strings.Contains(string(ev.Data), `"`+tc.row+`":true`) &&
						strings.Contains(string(ev.Data), `"folder":`) == tc.named {
						rows++
					}
				}
				cleared := f.terminalRun(t).SandboxRef == ""
				success := len(sweepRows(f.audit, "success", "sandbox_stopped"))
				if f.sb.stopCount() != stops || len(f.cs.giveUps) != 1+want || cleared != past || f.leaser.markCount() != 1-want ||
					success != want || rows != want {
					t.Errorf("%s: %d stops, %d give-ups, ref cleared %v, %d marks, %d success rows, %d given_up rows; want %d, %d, %v, %d, %d, %d",
						when, f.sb.stopCount(), len(f.cs.giveUps), cleared, f.leaser.markCount(), success, rows, stops, 1+want, past, 1-want, want, want)
				}
			}
			f.tick(t, f.s) // inside the keep: the statement's cap has not passed, so it applies nothing
			check("inside the keep", 1, false)
			f.cs.giveUpApplies = true // past the keep
			f.tick(t, f.s)
			check("past the keep", 2, true)
			f.tick(t, f.s)
			check("the next tick", 2, true)
		})
	}
}

// owedCounted is a copy back still owed (a resume, its generation settled) whose pass counted 3 changes and named 2.
func owedCounted() types.RunLocalFolderStatus {
	return types.RunLocalFolderStatus{Gen: 3, Settled: 3, Resume: &types.FolderResume{Unsynced: 3, Counted: true, Paths: []string{"a.txt", "b.txt"}}}
}

// pastTheKeep moves the fake database's clock to the run's end plus its keep: the next tick is past it.
func (f *forced) pastTheKeep() { f.clock.now = f.due.Add(f.s.pendingCap()) }

// forcedGiveUpState is what the forced retry has done about the copy back so far: the give-up statements made, the
// run.folder.transfer failure rows, the marks, the sandbox.sweep success rows, and whether the ref is cleared.
type forcedGiveUpState struct {
	giveUps, rows, marks, success int
	cleared                       bool
}

func (f *forced) giveUpState(t *testing.T) forcedGiveUpState {
	t.Helper()
	f.cs.smu.Lock()
	n := len(f.cs.giveUps)
	f.cs.smu.Unlock()
	rows := 0
	for _, ev := range f.audit.snapshot() {
		if ev.Action == "run.folder.transfer" && ev.Outcome == "failure" {
			rows++
		}
	}
	return forcedGiveUpState{giveUps: n, rows: rows, marks: f.leaser.markCount(),
		success: len(sweepRows(f.audit, "success", "sandbox_stopped")), cleared: f.terminalRun(t).SandboxRef == ""}
}

// checkKeepGiveUp checks that the first give-up was the tail owner's, with the keep as its cap and no containment
// terms, and that the record it left is given up with the count and the names of the changes left behind.
func (f *forced) checkKeepGiveUp(t *testing.T) {
	t.Helper()
	f.cs.smu.Lock()
	p := f.cs.giveUps[0]
	f.cs.smu.Unlock()
	if p.Cap != f.s.pendingCap() || p.D1 || p.Back.GivenUpAt == nil || p.Back.GivenUpCount == nil || *p.Back.GivenUpCount != 3 ||
		!slices.Equal(p.Back.GivenUpPaths, []string{"a.txt", "b.txt"}) || p.Back.GivenUpPathsMore != 1 {
		t.Errorf("give-up %+v; want the keep as its cap, no containment terms, and 3 changes given up, 2 named and 1 more", p)
	}
	if rec := f.cs.folderStatus(); rec.Resume != nil || !folderGivenUp(rec) {
		t.Errorf("record %+v; want it given up, with no resume", rec)
	}
}

// A forced-marked run whose copy back is still owed and whose sandbox stop keeps failing (its cut fails and so does
// the stop) has its copy back given up once its keep has run out, by the forced retry: inside the keep nothing is
// given up; on the first tick past it the record is given up with the keep as the statement's cap, its count and
// names, and one given_up row, while the ref and the mark stay and no success row is written; the next tick adds
// nothing; once the stop succeeds the ref is cleared with one success row and the mark clears. Catches: a run that
// shows its changes kept for good, past the give-up time it showed, with no given_up row; a give-up inside the keep
// or with no keep as its cap; a give-up made only after a confirmed stop; a mark or ref cleared by the give-up.
func TestSweepForcedStops_GivesUpAnOwedCopyBackPastItsKeepWhileTheStopFails(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.release()
	f.setRecord(owedCounted())
	f.cs.giveUpApplies = true // the fake statement has no cap of its own: the retry's keep test is what is pinned
	f.sb.proxyErr = errors.New("daemon unavailable")
	f.sb.stopErr = errors.New("the daemon did not answer the stop")
	for _, step := range []struct {
		when string
		set  func()
		want forcedGiveUpState
	}{
		{"inside the keep", func() {}, forcedGiveUpState{marks: 1}},
		{"past the keep", f.pastTheKeep, forcedGiveUpState{giveUps: 1, rows: 1, marks: 1}},
		{"the next tick", func() {}, forcedGiveUpState{giveUps: 1, rows: 1, marks: 1}},
		{"the stop succeeding", func() { f.sb.stopErr = nil }, forcedGiveUpState{giveUps: 1, rows: 1, success: 1, cleared: true}},
	} {
		step.set()
		f.tick(t, f.s)
		if got := f.giveUpState(t); got != step.want {
			t.Fatalf("%s: %+v; want %+v", step.when, got, step.want)
		}
		if step.want.giveUps == 1 {
			f.checkKeepGiveUp(t)
		}
	}
}

// The same run whose folder seed, data_out rule or folder record never reads, with its stop failing, is given up
// past its keep with the note of the read that failed, as the tail owner decides it: one given_up row that names the
// read (and no folder when the seed is the read that fails), the ref and the mark kept, and nothing given up inside
// the keep. Catches: a give-up that waits on a read that never answers; one that drops the tail owner's notes.
func TestSweepForcedStops_GivesUpAFolderThatNeverReadsPastItsKeepWhileTheStopFails(t *testing.T) {
	down := errors.New("the database did not answer")
	for _, tc := range []struct {
		name, note, row string
		named           bool
		set             func(f *forced)
	}{
		{"its folder seed", folderRuleUnreadable, "rule_unreadable", false, func(f *forced) { f.cs.seedErr = down }},
		{"its data out rule", folderRuleUnreadable, "rule_unreadable", true, func(f *forced) { f.cs.policyErr = down }},
		{"its folder record", folderStatusUnreadable, "status_unreadable", true, func(f *forced) {
			f.s.cfg.Store = unreadableRecordStore{f.s.cfg.Store.(markedStore)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForced(t)
			f.claimOnly(t)
			f.release()
			f.setRecord(types.RunLocalFolderStatus{Gen: 3, Settled: 3})
			f.cs.giveUpApplies = true
			tc.set(f)
			f.sb.proxyErr = errors.New("daemon unavailable")
			f.sb.stopErr = errors.New("the daemon did not answer the stop")
			noted := func() int {
				n := 0
				for _, ev := range f.audit.snapshot() {
					if ev.Action == "run.folder.transfer" && strings.Contains(string(ev.Data), `"`+tc.row+`":true`) &&
						strings.Contains(string(ev.Data), `"folder":`) == tc.named {
						n++
					}
				}
				return n
			}
			f.tick(t, f.s)
			if got := f.giveUpState(t); got != (forcedGiveUpState{marks: 1}) {
				t.Fatalf("inside the keep: %+v; want nothing given up and the mark kept", got)
			}
			f.pastTheKeep()
			f.tick(t, f.s)
			got := f.giveUpState(t)
			if got.giveUps == 0 || got.rows != 1 || noted() != 1 || got.marks != 1 || got.cleared || got.success != 0 {
				t.Fatalf("past the keep: %+v, %d rows noting %s; want a give-up, one row that notes it, the ref and the mark kept", got, noted(), tc.row)
			}
			if p := f.cs.giveUps[0]; p.Cap != f.s.pendingCap() || p.D1 || !strings.HasSuffix(p.Back.Error, tc.note) {
				t.Errorf("give-up %+v; want the keep as its cap, no containment terms, and the failed read noted", p)
			}
			f.tick(t, f.s)
			if got := f.giveUpState(t); got.rows != 1 || noted() != 1 {
				t.Errorf("the next tick: %+v; want still one given_up row", got)
			}
		})
	}
}

// A kept sandbox whose run's lock is never let go (its cut is confirmed and its copy back owed, so the retry keeps
// it and the mark waits for the lock) is given up once its keep has run out: inside the keep nothing is given up or
// stopped; on the first tick past it the record is given up, and in the same tick the sandbox, no longer kept, is
// stopped without the lock, the ref is cleared with one success row and the mark clears. Catches: a give-up only on
// the path where the stop failed, which never runs while the lock is held.
func TestSweepForcedStops_GivesUpAKeptSandboxsCopyBackPastItsKeepWhileTheLockIsHeld(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t) // the test's hold on the lock is never let go
	f.setRecord(owedCounted())
	f.cs.giveUpApplies = true
	f.tick(t, f.s)
	if got := f.giveUpState(t); got != (forcedGiveUpState{marks: 1}) || f.sb.stopCount() != 0 {
		t.Fatalf("inside the keep: %+v, %d stops; want the sandbox kept, nothing given up and the mark kept", got, f.sb.stopCount())
	}
	f.pastTheKeep()
	f.tick(t, f.s)
	if got, want := f.giveUpState(t), (forcedGiveUpState{giveUps: 1, rows: 1, success: 1, cleared: true}); got != want || f.sb.stopCount() != 1 {
		t.Fatalf("past the keep: %+v, %d stops; want %+v and one stop", got, f.sb.stopCount(), want)
	}
	f.checkKeepGiveUp(t)
}

// A marked run on a runner that is away is not given up by the forced retry, past the keep too: the tail owner on a
// replica it is away from owns that give-up. Once the runner is back here, the retry gives it up on its next tick.
// Catches: a give-up made before the away return, which would give up and write for a runner the retry does not
// reach; and no give-up once the runner is here.
func TestSweepForcedStops_AnAwayRunnersCopyBackIsGivenUpOnlyOnceItIsHere(t *testing.T) {
	f := newForced(t)
	_, online := f.onFencedRunner(t)
	f.claimOnly(t)
	f.release()
	f.setRecord(owedCounted())
	f.cs.giveUpApplies = true
	online.Store(false)
	f.pastTheKeep()
	f.tick(t, f.s)
	if got := f.giveUpState(t); got != (forcedGiveUpState{marks: 1}) {
		t.Fatalf("away, past the keep: %+v; want nothing given up and the mark kept", got)
	}
	online.Store(true)
	f.tick(t, f.s)
	if got := f.giveUpState(t); got.giveUps != 1 || got.rows != 1 {
		t.Fatalf("here again: %+v; want one give-up and one given_up row", got)
	}
	f.checkKeepGiveUp(t)
}

// giveUpBarrierStore holds each give-up statement until n have arrived, so two give-ups decided on the same owed
// record are both in the statement before either applies.
type giveUpBarrierStore struct {
	markedStore
	n       int32
	arrived *atomic.Int32
}

func (s giveUpBarrierStore) GiveUpRunLocalFolder(ctx context.Context, id uuid.UUID, p store.GiveUpParams) (store.GiveUpOutcome, error) {
	s.arrived.Add(1)
	for deadline := time.Now().Add(10 * time.Second); s.arrived.Load() < s.n && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	return s.markedStore.GiveUpRunLocalFolder(ctx, id, p)
}

// Two replicas' forced retries of a run whose lock is held for good, past its keep, both decide the give-up and race
// into the statement: one applies and writes the one given_up row; the other finds the record final and writes
// none. The stop fails, so the ref and the mark stay. Catches: a given_up row written for a statement that applied
// nothing, which writes two rows for one give-up.
func TestSweepForcedStops_TwoReplicasGivingUpOneCopyBackWriteOneRow(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t) // the test's hold on the lock is never let go
	f.setRecord(owedCounted())
	f.cs.giveUpApplies = true
	f.sb.stopErr = errors.New("the daemon did not answer the stop")
	var arrived atomic.Int32
	f.s.cfg.Store = giveUpBarrierStore{markedStore: f.s.cfg.Store.(markedStore), n: 2, arrived: &arrived}
	other := New(f.s.cfg)
	other.locks.override = f.s.locker() // one lock for both replicas, held by the test
	f.pastTheKeep()
	f.clock.now = f.clock.now.Add(time.Minute)
	marks, err := f.leaser.ListForcedStops(context.Background())
	if err != nil || len(marks) != 1 {
		t.Fatalf("marks %v, %v; want one", marks, err)
	}
	fs := f.s.cfg.Store.(store.ForcedStopper)
	var wg sync.WaitGroup
	for _, srv := range []*Server{f.s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.retryForcedStop(context.Background(), fs, marks[0])
		}()
	}
	wg.Wait()
	if arrived.Load() != 2 {
		t.Fatalf("%d give-up statements; want both replicas to decide the give-up", arrived.Load())
	}
	if got, want := f.giveUpState(t), (forcedGiveUpState{giveUps: 2, rows: 1, marks: 1}); got != want {
		t.Fatalf("%+v; want %+v: one row for one give-up", got, want)
	}
	f.checkKeepGiveUp(t)
}

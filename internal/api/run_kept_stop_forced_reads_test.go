// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// forcedReadLog is the forced fixture's store noting, in order, the reads the forced retry's give-up makes: "due"
// (RunDueFlags), "marks" (ListForcedStops), "run" (GetRun) and "record" (RunLocalFolderStatus).
type forcedReadLog struct {
	markedStore
	mu  *sync.Mutex
	log *[]string
}

func (s forcedReadLog) note(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.log = append(*s.log, what)
}

func (s forcedReadLog) RunDueFlags(ctx context.Context, id uuid.UUID, keep time.Duration) (store.RunDue, error) {
	s.note("due")
	return s.markedStore.RunDueFlags(ctx, id, keep)
}

func (s forcedReadLog) ListForcedStops(ctx context.Context) ([]store.ForcedStop, error) {
	s.note("marks")
	return s.markedStore.ListForcedStops(ctx)
}

func (s forcedReadLog) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.note("run")
	return s.markedStore.GetRun(ctx, id)
}

func (s forcedReadLog) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	s.note("record")
	return s.markedStore.RunLocalFolderStatus(ctx, id)
}

// logReads puts forcedReadLog over the fixture's store, and answers the reads it has noted so far.
func (f *forced) logReads() func() []string {
	var mu sync.Mutex
	var log []string
	f.s.cfg.Store = forcedReadLog{markedStore: f.s.cfg.Store.(markedStore), mu: &mu, log: &log}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(log)
	}
}

// The tick's one listing of marks can be stale for a later row: another replica cleared the mark since, so the tail
// owner owns the run again and may have brought its changes back. A retry handed that stale mark, past the keep, gives
// nothing up and leaves the record owed. With the mark still set it gives the copy back up, with its row. Catches: a
// give-up decided on the listing alone, which records changes as given up after they may have arrived.
func TestRetryForcedStop_AMarkClearedSinceTheListingIsNotGivenUp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cleared bool
		giveUps int
	}{
		{"the mark cleared since the listing", true, 0},
		{"the mark still set", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForced(t)
			f.claimOnly(t) // the test's hold on the lock is never let go
			f.setRecord(owedCounted())
			f.cs.giveUpApplies = true
			f.pastTheKeep()
			f.clock.now = f.clock.now.Add(time.Minute)
			ctx := context.Background()
			marks, err := f.leaser.ListForcedStops(ctx) // the tick's listing
			if err != nil || len(marks) != 1 {
				t.Fatalf("marks %v, %v; want one", marks, err)
			}
			if tc.cleared {
				if ok, err := f.leaser.ClearForcedStop(ctx, f.run.ID, marks[0].At); !ok || err != nil {
					t.Fatalf("clear = %v, %v; want applied", ok, err)
				}
			}
			f.s.retryForcedStop(ctx, f.s.cfg.Store.(store.ForcedStopper), marks[0])
			got := f.giveUpState(t)
			if got.giveUps != tc.giveUps || got.rows != tc.giveUps {
				t.Fatalf("%+v; want %d give-ups and %d given_up rows", got, tc.giveUps, tc.giveUps)
			}
			if rec := f.cs.folderStatus(); folderGivenUp(rec) != (tc.giveUps == 1) || (tc.cleared && rec.Resume == nil) {
				t.Errorf("record %+v; want it given up only while the mark is set, and still owed once it is cleared", rec)
			}
		})
	}
}

// Past the keep, the give-up reads the keep's answer first, then the mark, and only then the run and its record for
// the give-up itself. Catches: the mark read before the keep's answer, which leaves a window where a mark cleared
// between the two reads lets a pass start that the give-up then throws away; and no fresh mark read at all.
func TestGiveUpForcedPastKeep_ReadsTheMarkAfterTheKeepsAnswer(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.setRecord(owedCounted())
	f.cs.giveUpApplies = true
	f.pastTheKeep()
	ctx := context.Background()
	marks, err := f.leaser.ListForcedStops(ctx)
	if err != nil || len(marks) != 1 {
		t.Fatalf("marks %v, %v; want one", marks, err)
	}
	reads := f.logReads()
	f.s.giveUpForcedPastKeep(ctx, f.s.cfg.Store.(store.ForcedStopper), marks[0])
	got := reads()
	if len(got) < 3 || got[0] != "due" || got[1] != "marks" || len(f.cs.giveUps) != 1 {
		t.Errorf("reads %v, %d give-ups; want the keep's answer, then the mark, then the give-up's own reads, and one give-up", got, len(f.cs.giveUps))
	}
}

// Inside the keep the give-up asks the database whether the keep has run out, and reads nothing more: not the mark,
// the run or its record. Catches: the keep gate dropped or moved after other reads, which would read, and on a failed
// read log, on every tick for the whole keep.
func TestGiveUpForcedPastKeep_InsideTheKeepReadsOnlyTheKeepsAnswer(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.setRecord(owedCounted())
	f.cs.giveUpApplies = true
	ctx := context.Background()
	marks, err := f.leaser.ListForcedStops(ctx)
	if err != nil || len(marks) != 1 {
		t.Fatalf("marks %v, %v; want one", marks, err)
	}
	reads := f.logReads()
	f.s.giveUpForcedPastKeep(ctx, f.s.cfg.Store.(store.ForcedStopper), marks[0])
	if got := reads(); !slices.Equal(got, []string{"due"}) || len(f.cs.giveUps) != 0 {
		t.Errorf("reads %v, %d give-ups inside the keep; want only the keep's answer and no give-up", got, len(f.cs.giveUps))
	}
}

// The give-up statement answers Done for a record already final, as Postgres's does, and the give-up is then final
// with no read of the record again; a statement that applied nothing to a record still owed reads it once, and is not
// final. Catches: a fake whose losing statement answers Done=false for a final record, so a race between two give-ups
// pins the re-read and never the Done answer.
func TestGiveUpFolderWith_ADoneAnswerIsFinalWithNoReread(t *testing.T) {
	at := time.Now()
	givenUp := types.RunLocalFolderStatus{Gen: 4, Settled: 3, Back: &types.FolderPassRecord{At: at, Error: "given up", GivenUpAt: &at}}
	for _, tc := range []struct {
		name           string
		rec            types.RunLocalFolderStatus
		applies, final bool
		rereads        int
	}{
		{"a record already final", givenUp, true, true, 0},
		{"a record still owed that the statement does not apply to", owedCounted(), false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForced(t)
			f.claimOnly(t)
			f.setRecord(tc.rec)
			f.cs.giveUpApplies = tc.applies
			reads := f.logReads()
			final, err := f.s.giveUpFolderWith(context.Background(), f.terminalRun(t), tc.rec, "", store.GiveUpParams{})
			rereads := 0
			for _, r := range reads() {
				if r == "record" {
					rereads++
				}
			}
			if final != tc.final || err != nil || len(f.cs.giveUps) != 1 || rereads != tc.rereads {
				t.Errorf("final %v, err %v, %d statements, %d re-reads; want %v, nil, 1, %d", final, err, len(f.cs.giveUps), rereads, tc.final, tc.rereads)
			}
			if rec := f.cs.folderStatus(); rec.Gen != tc.rec.Gen {
				t.Errorf("record at generation %d; want %d: the statement applied nothing", rec.Gen, tc.rec.Gen)
			}
		})
	}
}

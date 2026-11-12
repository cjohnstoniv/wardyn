// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

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

func (s *bumpFailStore) SetRunLocalFolderStatusIf(ctx context.Context, id uuid.UUID, guard store.FolderStatusGuard, st types.RunLocalFolderStatus) (bool, error) {
	s.sets.Add(1)
	return s.computerFolderStore.SetRunLocalFolderStatusIf(ctx, id, guard, st)
}

// A pass back whose record cannot be moved to the pass's generation stops the sandbox, and says so: one failure
// row and one try at the record, at the generation it was read at. A silent stop leaves the run saying nothing.
func TestFolderBack_BumpStoreErrorRecordsAndAudits(t *testing.T) {
	f := newPassFixture(t, 2, 2, nil)
	bs := &bumpFailStore{computerFolderStore: f.cs, bumpErr: errors.New("the bump failed")}
	f.s.cfg.Store = bs
	if err := f.finalizeWithin(30 * time.Second); errors.Is(err, errComputerFolderResumes) {
		t.Errorf("finalize = %v, want the sandbox left to its stop", err)
	}
	const want = "no pass ran; the folder record could not be moved before its pass: its store could not write it; the sandbox was stopped with its changes not brought back yet, uncounted"
	assertFolderBackTornDown(t, f.cs, f.audit, f.run, f.sb, want)
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

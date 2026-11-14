// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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
// clears. A give-up that leaves the record open, or a folder seed that cannot be read, keeps the ref and the mark,
// with no given_up row and no success row, and still runs the run's revoke cascade; the next tick, the cut still not
// confirmed and the give-up applying, finishes it. Catches: a ref cleared while the record still shows the changes
// owed, with no given_up row; a mark cleared on a record not final, which leaves nothing to try again; a record not
// final that skips the revoke.
func TestSweepForcedStops_GivesUpAnOwedCopyBackBeforeClearingTheRef(t *testing.T) {
	owed := types.RunLocalFolderStatus{Gen: 3, Settled: 3, Resume: &types.FolderResume{Unsynced: 3, Counted: true, Paths: []string{"a.txt", "b.txt"}}}
	for _, held := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			rec     types.RunLocalFolderStatus
			applies bool
			seedErr error
			giveUps int
		}{
			{"applied", owed, true, nil, 1},
			{"left open", owed, false, nil, 1},
			{"a seed that cannot be read", types.RunLocalFolderStatus{Gen: 3, Settled: 3}, true, errors.New("the database did not answer"), 0},
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
				f.cs.giveUpApplies, f.cs.seedErr = tc.applies, tc.seedErr
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
				final := tc.applies && tc.seedErr == nil
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

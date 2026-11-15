// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// The boot orphan pass on a confirmed cut keeps a sandbox whose folder record cannot be read only inside the keep:
// past it the copy back is given up with its given_up row (the verdict's statement applies, the one after the stop
// finds it final), and the sandbox is stopped and its ref cleared. Catches: a record that never reads keeping its
// sandbox for good on every boot.
func TestOrphanPass_KeepsASandboxWhoseRecordNeverReadsOnlyInsideItsKeep(t *testing.T) {
	for _, past := range []bool{false, true} {
		t.Run(fmt.Sprintf("past the keep %v", past), func(t *testing.T) {
			rn := &orphanVerdictRunner{orphanSweepRunner: &orphanSweepRunner{fakeRunner: &fakeRunner{}}, verdicts: map[uuid.UUID]bool{}}
			s, st, audit, run := unreadFolderServer(t, types.RunCompleted, rn)
			st.unreadable, st.past = true, past
			if err := s.reconcileOrphanedSandbox(context.Background()); err != nil {
				t.Fatal(err)
			}
			gs, cleared := st.liveGiveUps(), slices.Contains(st.clearedRefs, run.ID)
			if !past {
				if rn.stopCount("sbx") != 0 || len(gs) != 0 || cleared || unreadRows(audit, run) != 0 {
					t.Fatalf("%d stops, %d give-ups, ref cleared %v, %d rows; want the sandbox kept and nothing given up",
						rn.stopCount("sbx"), len(gs), cleared, unreadRows(audit, run))
				}
				return
			}
			if rn.stopCount("sbx") != 1 || len(gs) != 2 || !cleared || unreadRows(audit, run) != 1 {
				t.Fatalf("%d stops, %d give-ups, ref cleared %v, %d rows; want 1 stop, 2 give-ups (applied, then found final), the ref cleared, 1 row",
					rn.stopCount("sbx"), len(gs), cleared, unreadRows(audit, run))
			}
			if p := gs[0]; p.Cap != s.pendingCap() || p.D1 || !strings.HasSuffix(p.Back.Error, folderStatusUnreadable) {
				t.Errorf("give-up %+v; want the keep as its cap, no containment terms, and the unreadable record noted", p)
			}
		})
	}
}

// The cadence and READY sweeps decide a sandbox on a fenced runner whose folder record cannot be read the same way:
// inside the keep it is kept (TestReadySweep_DecidesAnEndedRunAsTheCadenceSweepDoes pins that case too), and past it
// the copy back is given up once, with its given_up row, and the sandbox is an orphan. Catches: a record that never
// reads keeping its sandbox for good on every sweep.
func TestOrphanSweeps_GiveUpASandboxWhoseRecordNeverReadsPastItsKeep(t *testing.T) {
	sweeps := []struct {
		name  string
		sweep func(*forced, *testing.T) bool
	}{{"cadence", (*forced).cadenceSweep}, {"READY", (*forced).readySweep}}
	for _, sw := range sweeps {
		for _, past := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, past the keep %v", sw.name, past), func(t *testing.T) {
				f := newForced(t)
				f.onFencedRunner(t)
				f.unreadable()
				if past {
					f.pastTheKeep()
					f.cs.giveUpApplies = true
				}
				f.claimOnly(t)
				f.release()
				orphan := sw.sweep(f, t)
				g := f.giveUpState(t)
				want := 0
				if past {
					want = 1
				}
				if orphan != past || g.giveUps != want || g.rows != want {
					t.Fatalf("orphan %v, %d give-ups, %d rows; want orphan %v, %d, %d", orphan, g.giveUps, g.rows, past, want, want)
				}
				if past {
					f.cs.smu.Lock()
					p := f.cs.giveUps[0]
					f.cs.smu.Unlock()
					if p.Cap != f.s.pendingCap() || p.D1 || !strings.HasSuffix(p.Back.Error, folderStatusUnreadable) {
						t.Errorf("give-up %+v; want the keep as its cap, no containment terms, and the unreadable record noted", p)
					}
				}
			})
		}
	}
}

// readsOnTheSecondStore is the forced fixture's store whose first read of the folder record fails and whose later
// reads answer.
type readsOnTheSecondStore struct {
	markedStore
	failed atomic.Bool
}

func (s *readsOnTheSecondStore) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	if !s.failed.Swap(true) {
		return types.RunLocalFolderStatus{}, errors.New("the record could not be read")
	}
	return s.markedStore.RunLocalFolderStatus(ctx, id)
}

// Past the keep, the cadence and READY sweeps decide a sandbox whose folder record could not be read by the second
// read of it: a give-up that does not land keeps the sandbox with its one statement and no row, a run that owes
// nothing is removed with no statement, and a record that reads the second time and is still owed is given up
// plainly, not noted unreadable, and removed. Catches: a give-up that did not land removing the sandbox with its
// changes, a run that owes nothing given up with a row, and a record that reads the second time given up as
// unreadable.
func TestOrphanSweeps_PastTheKeepAnUnreadableRecordIsDecidedByItsSecondRead(t *testing.T) {
	cases := []struct {
		name          string
		setup         func(*forced)
		orphan        bool
		giveUps, rows int
		plain         bool
	}{
		{"a give-up that does not land", func(f *forced) { f.unreadable() }, false, 1, 0, false},
		{"nothing owed", func(f *forced) { f.unreadable(); f.cs.giveUpApplies = true; f.withoutOwedPass() }, true, 0, 0, false},
		{"a record that reads the second time and is owed", func(f *forced) {
			f.s.cfg.Store = &readsOnTheSecondStore{markedStore: f.s.cfg.Store.(markedStore)}
			f.cs.giveUpApplies = true
		}, true, 1, 1, true},
	}
	sweeps := []struct {
		name  string
		sweep func(*forced, *testing.T) bool
	}{{"cadence", (*forced).cadenceSweep}, {"READY", (*forced).readySweep}}
	for _, sw := range sweeps {
		for _, tc := range cases {
			t.Run(sw.name+", "+tc.name, func(t *testing.T) {
				f := newForced(t)
				f.onFencedRunner(t)
				f.claimOnly(t)
				f.release()
				tc.setup(f)
				f.pastTheKeep()
				orphan := sw.sweep(f, t)
				g := f.giveUpState(t)
				if orphan != tc.orphan || g.giveUps != tc.giveUps || g.rows != tc.rows {
					t.Fatalf("orphan %v, %d give-ups, %d rows; want orphan %v, %d, %d", orphan, g.giveUps, g.rows, tc.orphan, tc.giveUps, tc.rows)
				}
				if tc.plain {
					f.cs.smu.Lock()
					p := f.cs.giveUps[0]
					f.cs.smu.Unlock()
					if strings.HasSuffix(p.Back.Error, folderStatusUnreadable) || strings.HasSuffix(p.Back.Error, folderRuleUnreadable) {
						t.Errorf("give-up error %q; want the record given up plainly, as it read the second time", p.Back.Error)
					}
				}
			})
		}
	}
}

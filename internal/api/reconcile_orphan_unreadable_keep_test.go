// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
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

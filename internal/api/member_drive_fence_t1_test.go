// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fenceIssueCounter is the Mac rig's store counting the physical calls the delete fence has let through.
type fenceIssueCounter struct {
	*macDiskStore
	issues int
}

func (s *fenceIssueCounter) IssueMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time, n int) (int, error) {
	s.issues++
	return s.macDiskStore.IssueMemberDriveDelete(ctx, id, started, n)
}

// fenceMacRig is macDiskDeleteRig with its drive's delete asked age ago (zero: not asked), and the store counting issues.
func fenceMacRig(t *testing.T, age time.Duration) (*Server, *fenceIssueCounter, *diskMacTransport, types.UserDrive) {
	t.Helper()
	srv, st, tr, d := macDiskDeleteRig(t)
	if age > 0 {
		asked := st.now.Add(-age)
		d.DeleteStartedAt = &asked
		st.drives[d.ID] = d
	}
	counted := &fenceIssueCounter{macDiskStore: st}
	srv.cfg.Store = counted
	return srv, counted, tr, d
}

// fenceWantMacRow fails unless the drive's row is present with the mark and count given (wantMarked: a mark is set).
func fenceWantMacRow(t *testing.T, st *fenceIssueCounter, id uuid.UUID, wantMarked bool, wantN int) {
	t.Helper()
	got, kept := st.drives[id]
	if !kept || (got.DeleteStartedAt != nil) != wantMarked || got.DeleteIssues != wantN {
		t.Fatalf("row kept = %v, marked = %v, count = %d; want kept, marked %v, count %d",
			kept, got.DeleteStartedAt != nil, got.DeleteIssues, wantMarked, wantN)
	}
}

// Each row of the Mac outcome table that differs from before the delete fence, on the sweep's path (M) and the owner's
// (O): what is left on the row, how many physical calls were let through, and what the next pass then does.
func TestMacDiskDeleteOutcomesLeaveTheClaimTheTableSays(t *testing.T) {
	ctx := context.Background()

	t.Run("a deferral keeps the claim and makes no issue", func(t *testing.T) {
		srv, st, tr, d := fenceMacRig(t, 0)
		srv.cfg.RunnerOnline = func(uuid.UUID) bool { return false } // the Mac is away: deferred
		if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusAccepted {
			t.Fatalf("the owner's delete of a drive on a Mac that is away = %d %s, want 202", w.Code, w.Body)
		}
		fenceWantMacRow(t, st, d.ID, true, 0) // claimed, nothing issued: hidden until the Mac's sweep
		if tr.asked != 0 || st.issues != 0 {
			t.Fatalf("Mac asked %d times, %d issues; want neither", tr.asked, st.issues)
		}
		srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id == st.mac.ID }
		st.now = st.now.Add(2 * time.Minute)
		if err := srv.finishMacDiskDeletes(ctx); err != nil {
			t.Fatal(err)
		}
		if _, kept := st.drives[d.ID]; kept || tr.asked != 1 || st.issues != 1 {
			t.Fatalf("after the Mac returned: kept = %v, asked %d, issues %d; want deleted by one issued call", kept, tr.asked, st.issues)
		}
	})

	t.Run("a record-only delete removes the row and issues nothing", func(t *testing.T) {
		srv, st, tr, d := fenceMacRig(t, 0)
		revoked := st.now
		st.mac.RevokedAt = &revoked // the Mac is no longer registered: the image stays on it
		if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusOK {
			t.Fatalf("the owner's delete of a drive whose Mac is gone = %d %s, want 200", w.Code, w.Body)
		}
		if _, kept := st.drives[d.ID]; kept || tr.asked != 0 || st.issues != 0 {
			t.Fatalf("kept = %v, asked %d, issues %d; want the row gone with no call and no issue", kept, tr.asked, st.issues)
		}
	})

	t.Run("the sweep keeps its mark while the owner has a run starting", func(t *testing.T) {
		srv, st, tr, d := fenceMacRig(t, 2*time.Minute)
		st.starting = map[string]bool{aliceSub: true}
		if err := srv.finishMacDiskDeletes(ctx); err != nil {
			t.Fatal(err)
		}
		fenceWantMacRow(t, st, d.ID, true, 0) // the owner already asked for the delete: the next sweep meets it again
		if tr.asked != 0 || st.issues != 0 {
			t.Fatalf("Mac asked %d times, %d issues; want neither", tr.asked, st.issues)
		}
		st.starting = nil
		st.now = st.now.Add(2 * time.Minute)
		if err := srv.finishMacDiskDeletes(ctx); err != nil {
			t.Fatal(err)
		}
		if _, kept := st.drives[d.ID]; kept || tr.asked != 1 {
			t.Fatalf("a later sweep: kept = %v, asked %d; want deleted by one call", kept, tr.asked)
		}
	})

	t.Run("the sweep keeps its mark and an outstanding issue when the Mac says in use", func(t *testing.T) {
		srv, st, tr, d := fenceMacRig(t, 2*time.Minute)
		tr.inUse = true
		if err := srv.finishMacDiskDeletes(ctx); err != nil {
			t.Fatal(err)
		}
		fenceWantMacRow(t, st, d.ID, true, 1) // issued, so odd, and not cleared
		if tr.asked != 1 || st.issues != 1 {
			t.Fatalf("Mac asked %d times, %d issues; want one each", tr.asked, st.issues)
		}
		tr.inUse = false
		st.now = st.now.Add(2 * time.Minute)
		if err := srv.finishMacDiskDeletes(ctx); err != nil {
			t.Fatal(err)
		}
		if _, kept := st.drives[d.ID]; kept || tr.asked != 2 || st.issues != 2 {
			t.Fatalf("a later sweep: kept = %v, asked %d, issues %d; want deleted by a second issued call", kept, tr.asked, st.issues)
		}
	})

	t.Run("the owner's in-use answer clears a claim made over an even count", func(t *testing.T) {
		srv, st, tr, d := fenceMacRig(t, 0)
		tr.inUse = true
		if w := deleteMyDrive(srv, aliceSub, d.ID.String()); w.Code != http.StatusConflict {
			t.Fatalf("the owner's delete of a disk a run has = %d %s, want 409", w.Code, w.Body)
		}
		fenceWantMacRow(t, st, d.ID, false, 2) // claim 0, issue 1, clear 2: the count only grows
		if tr.asked != 1 || st.issues != 1 {
			t.Fatalf("Mac asked %d times, %d issues; want one each", tr.asked, st.issues)
		}
	})

	t.Run("a run of the owner starting after the claim clears it before any issue", func(t *testing.T) {
		srv, st, tr, d := fenceMacRig(t, 0)
		tomb, started, err := st.BeginMemberDriveDelete(ctx, aliceSub, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		st.starting = map[string]bool{aliceSub: true}
		srv.deleteMemberDrive(ctx, st, tomb, started, memberDriveByOwner)
		fenceWantMacRow(t, st, d.ID, false, 2) // nothing issued: cleared, moving the count by two
		if tr.asked != 0 || st.issues != 0 {
			t.Fatalf("Mac asked %d times, %d issues; want neither", tr.asked, st.issues)
		}
	})
}

// fenceMacListRaceStore lands a hook right after the sweep's list of tombstoned disks is read: the instant an owner's
// retry, and a run's end, can change a row the sweep still holds in its list.
type fenceMacListRaceStore struct {
	*fenceIssueCounter
	after func()
}

func (s *fenceMacListRaceStore) ListMacDiskDeletes(ctx context.Context) ([]types.UserDrive, error) {
	out, err := s.fenceIssueCounter.ListMacDiskDeletes(ctx)
	if hook := s.after; hook != nil {
		s.after = nil
		hook()
	}
	return out, err
}

// The sweep lists X (its delete was deferred). The owner retries and the Mac answers in use, which clears the claim.
// The run ends, or does not. The sweep then reaches X: its own claim takes only a mark already a minute old, so it
// takes nothing, issues nothing and calls nothing.
func TestMacSweepDoesNotClaimADriveItsOwnerClearedAfterTheList(t *testing.T) {
	for name, runEnds := range map[string]bool{"the owner cleared it": false, "the owner cleared it and a run then ended": true} {
		t.Run(name, func(t *testing.T) {
			srv, st, tr, d := fenceMacRig(t, 2*time.Minute)
			race := &fenceMacListRaceStore{fenceIssueCounter: st}
			srv.cfg.Store = race
			race.after = func() {
				ctx := context.Background()
				row := st.drives[d.ID]
				if err := st.ClearMemberDriveClaim(ctx, d.ID, *row.DeleteStartedAt, row.DeleteIssues); err != nil {
					t.Errorf("the owner's clear: %v", err)
				}
				if runEnds { // a run's end claims the drive afresh, at the instant it ended
					ended := st.now
					row = st.drives[d.ID]
					row.DeleteStartedAt = &ended
					st.drives[d.ID] = row
				}
			}
			if err := srv.finishMacDiskDeletes(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tr.asked != 0 || st.issues != 0 {
				t.Fatalf("Mac asked %d times, %d issues; want neither", tr.asked, st.issues)
			}
			fenceWantMacRow(t, st, d.ID, runEnds, 2)
		})
	}
}

var _ store.MemberDriveStore = (*fenceMacListRaceStore)(nil)

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// erasureDriveStore is the member-drive double with the two directory reads a
// person's name resolves through.
type erasureDriveStore struct{ *memberDriveStore }

func (erasureDriveStore) ListAPITokens(context.Context) ([]types.APIToken, error) { return nil, nil }
func (erasureDriveStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}

func erasureDriveServer(st *memberDriveStore, rn runner.Runner) (*Server, *recRecorder) {
	rec := &recRecorder{}
	return New(Config{BaseCtx: context.Background(), Store: erasureDriveStore{st}, Audit: rec, Runner: rn, RunnerTarget: "docker"}), rec
}

// eraseMemberDrivesAs runs POST /people/{person}/erasure for the member_drives
// scope as a security admin.
func eraseMemberDrivesAs(srv *Server, person string) (code int, body string) {
	ctx := operatorCtx("sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	w := memberCall(ctx, srv.handleErasePerson, http.MethodPost, "/api/v1/people/"+person+"/erasure",
		erasureBody("member_drives"), map[string]string{"principal": person})
	return w.Code, w.Body.String()
}

func erasureRowOf(t *testing.T, rec *recRecorder) (outcome string, data map[string]any) {
	t.Helper()
	for _, ev := range rec.snapshot() {
		if ev.Action == "person.erasure" {
			outcome = ev.Outcome
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
		}
	}
	return outcome, data
}

// TestMemberDriveErasureStep: erasing a person deletes every drive they
// created, each through the member delete path (the substrate is asked to
// destroy the owner's object before the row goes, and each delete is one
// drive.member.delete row by the admin), and no one else's drive is touched. A
// drive that cannot be deleted stays, usable, and fails the scope by name; the
// person's other drives are still deleted, and a retry finishes.
func TestMemberDriveErasureStep(t *testing.T) {
	t.Run("deletes every drive the person created and no one else's", func(t *testing.T) {
		st := newMemberDriveStore()
		a1, a2 := seedMemberDrive(st, aliceSub, "Notes"), seedMemberDrive(st, aliceSub, "Scratch")
		bobs := seedMemberDrive(st, bobSub, "Notes")
		rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
		srv, rec := erasureDriveServer(st, rn)

		code, body := eraseMemberDrivesAs(srv, aliceSub)
		if code != http.StatusOK {
			t.Fatalf("erasure = %d %s, want 200", code, body)
		}
		if got := st.owned(aliceSub); len(got) != 0 {
			t.Errorf("alice still owns %d drives after her erasure", len(got))
		}
		if _, ok := st.drives[bobs.ID]; !ok || st.drives[bobs.ID].DeleteStartedAt != nil {
			t.Errorf("bob's drive was touched by alice's erasure")
		}
		if len(rn.calls) != 2 {
			t.Fatalf("substrate calls = %d, want one per drive of alice's", len(rn.calls))
		}
		for _, d := range []types.UserDrive{a1, a2} {
			home, _ := types.DriveHomeName(d, aliceSub, "")
			want := types.DriveObjectName(d, home)
			if !hasMountObject(rn.calls, want) {
				t.Errorf("no reclaim of %s, calls %+v", want, rn.calls)
			}
		}
		if n := countAction(rec, "drive.member.delete"); n != 2 {
			t.Errorf("drive.member.delete rows = %d, want 2", n)
		}
		if data := driveAuditData(t, rec, "drive.member.delete"); data["by"] != "admin" || data["owner"] != aliceSub {
			t.Errorf("delete audit = %v, want by admin for alice's drive", data)
		}
		if outcome, data := erasureRowOf(t, rec); outcome != "success" || !strings.Contains(string(mustJSON(data)), `"drives":2`) {
			t.Errorf("person.erasure = %s %v, want success counting 2 drives", outcome, data)
		}
	})

	t.Run("a drive a run holds stays and fails the scope by name", func(t *testing.T) {
		st := newMemberDriveStore()
		held, other := seedMemberDrive(st, aliceSub, "Held"), seedMemberDrive(st, aliceSub, "Free")
		rn := &perObjectRunner{memberDriveRunner: memberDriveRunner{}, busy: map[string]bool{}}
		home, _ := types.DriveHomeName(held, aliceSub, "")
		rn.busy[types.DriveObjectName(held, home)] = true
		srv, rec := erasureDriveServer(st, rn)

		code, body := eraseMemberDrivesAs(srv, aliceSub)
		if code != http.StatusInternalServerError || !strings.Contains(body, reasonErasureIncomplete) || !strings.Contains(body, `"remaining":["member_drives"]`) {
			t.Fatalf("erasure = %d %s, want the incomplete 500 naming member_drives", code, body)
		}
		if d, ok := st.drives[held.ID]; !ok || d.DeleteStartedAt != nil {
			t.Errorf("the held drive is gone or still tombstoned: %+v", d)
		}
		if _, ok := st.drives[other.ID]; ok {
			t.Errorf("the free drive survived: one held drive must not keep the rest")
		}
		outcome, data := erasureRowOf(t, rec)
		if msg, _ := data["error"].(string); outcome != "failure" || !strings.Contains(msg, `"Held"`) {
			t.Errorf("person.erasure = %s %v, want a failure naming the drive Held", outcome, data)
		}
		rn.busy = map[string]bool{} // the run ended: a retry finishes
		if code, body := eraseMemberDrivesAs(srv, aliceSub); code != http.StatusOK {
			t.Fatalf("retry = %d %s, want 200", code, body)
		}
		if len(st.owned(aliceSub)) != 0 {
			t.Errorf("the retry left alice a drive")
		}
	})

	t.Run("a substrate that cannot delete keeps the drive and fails the scope", func(t *testing.T) {
		st := newMemberDriveStore()
		d := seedMemberDrive(st, aliceSub, "Notes")
		rn := &memberDriveRunner{lifecycleErr: runner.ErrDriveLifecycleUnavailable}
		srv, _ := erasureDriveServer(st, rn)
		if code, body := eraseMemberDrivesAs(srv, aliceSub); code != http.StatusInternalServerError {
			t.Fatalf("erasure = %d %s, want the incomplete 500", code, body)
		}
		if got, ok := st.drives[d.ID]; !ok || got.DeleteStartedAt != nil || len(rn.calls) != 0 {
			t.Errorf("kept = %v, tombstone = %v, reclaim calls = %d: the lifecycle check must gate an erasure's delete too", ok, got.DeleteStartedAt, len(rn.calls))
		}
	})

	t.Run("a delete another request is running fails the scope", func(t *testing.T) {
		st := newMemberDriveStore()
		d := seedMemberDrive(st, aliceSub, "Notes")
		at := st.now.Add(-5 * time.Second)
		d.DeleteStartedAt = &at
		st.drives[d.ID] = d
		rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
		srv, _ := erasureDriveServer(st, rn)
		if code, body := eraseMemberDrivesAs(srv, aliceSub); code != http.StatusInternalServerError {
			t.Fatalf("erasure = %d %s, want the incomplete 500", code, body)
		}
		if len(rn.calls) != 0 {
			t.Errorf("a drive mid-delete reached the substrate")
		}
	})

	t.Run("a person with no drives is a success", func(t *testing.T) {
		srv, _ := erasureDriveServer(newMemberDriveStore(), &memberDriveRunner{})
		if code, body := eraseMemberDrivesAs(srv, aliceSub); code != http.StatusOK {
			t.Fatalf("erasure = %d %s, want 200", code, body)
		}
	})
}

// perObjectRunner refuses, as in use, the objects in busy.
type perObjectRunner struct {
	memberDriveRunner
	busy map[string]bool
}

func (r *perObjectRunner) ReclaimDrive(_ context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	if r.busy[m.ObjectName] {
		return "", runner.ErrDriveInUse
	}
	return runner.DriveReclaimDeleted, nil
}

func hasMountObject(calls []types.DriveMount, object string) bool {
	for _, c := range calls {
		if c.ObjectName == object {
			return true
		}
	}
	return false
}

// TestMemberDriveErasureReachesTheStoredOwnerSpelling: create stores the
// canonical form of the signed-in sub (session and API token both publish it
// as the OIDC human), and the erasure matches the same form, so a sub with
// capitals or spaces is reached however the admin spells the person.
func TestMemberDriveErasureReachesTheStoredOwnerSpelling(t *testing.T) {
	const sub = "Sub-Mixed-Case"
	for _, typed := range []string{sub, "sub-mixed-case"} {
		t.Run(typed, func(t *testing.T) {
			st := newMemberDriveStore()
			rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
			srv, _ := erasureDriveServer(st, rn)
			if w := createMemberDrive(srv, personCtx(sub), "Notes"); w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body.String())
			}
			if got := st.owned(""); len(got) != 1 || got[0].OwnerSubject != types.CanonicalUserSubject(sub) {
				t.Fatalf("stored drives = %+v, want one owned by the canonical sub", got)
			}
			if code, body := eraseMemberDrivesAs(srv, typed); code != http.StatusOK {
				t.Fatalf("erasure of %q = %d %s, want 200", typed, code, body)
			}
			if len(st.owned("")) != 0 {
				t.Errorf("erasure of %q left the drive", typed)
			}
		})
	}
}

// TestMemberDriveErasureKeepsDrivesWhileARunStarts: while the person has a run
// in STARTING, erasure keeps their drives with no tombstone written, fails the
// member_drives scope by drive name, and a retry once the run has left STARTING
// finishes.
func TestMemberDriveErasureKeepsDrivesWhileARunStarts(t *testing.T) {
	st := newMemberDriveStore()
	d := seedMemberDrive(st, aliceSub, "Notes")
	st.starting = map[string]bool{aliceSub: true}
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	srv, rec := erasureDriveServer(st, rn)

	code, body := eraseMemberDrivesAs(srv, aliceSub)
	if code != http.StatusInternalServerError || !strings.Contains(body, reasonErasureIncomplete) || !strings.Contains(body, `"remaining":["member_drives"]`) {
		t.Fatalf("erasure = %d %s, want the incomplete 500 naming member_drives", code, body)
	}
	if got, ok := st.drives[d.ID]; !ok || got.DeleteStartedAt != nil || st.begins != 0 || len(rn.calls) != 0 {
		t.Fatalf("kept = %v (tombstone %v), tombstone writes = %d, substrate calls = %d; want kept, never tombstoned, no call",
			ok, got.DeleteStartedAt, st.begins, len(rn.calls))
	}
	if outcome, data := erasureRowOf(t, rec); outcome != "failure" || !strings.Contains(fmt.Sprint(data["error"]), `"Notes"`) {
		t.Errorf("person.erasure = %s %v, want a failure naming the drive Notes", outcome, data)
	}
	st.starting = nil // the run left STARTING: a retry finishes
	if code, body := eraseMemberDrivesAs(srv, aliceSub); code != http.StatusOK {
		t.Fatalf("retry = %d %s, want 200", code, body)
	}
	if len(st.owned(aliceSub)) != 0 || len(rn.calls) != 1 {
		t.Errorf("after the retry alice owns %d drives, substrate calls = %d; want none left after one reclaim", len(st.owned(aliceSub)), len(rn.calls))
	}
}

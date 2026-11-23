// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A person's own team drive (#3459) against a real Postgres: the super admin's write bit, the delete fence on the
// shared object, the count cap and the delete's wait for any starting run. Guarded by WARDYN_TEST_PG: skipped
// cleanly when unset.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

type teamDrivePGEnv struct {
	pool *pgxpool.Pool
	pg   store.PG
	srv  *Server
	h    *harness
	rn   *memberDriveRunner
	unit uuid.UUID
}

func newTeamDrivePGEnv(t *testing.T) teamDrivePGEnv {
	t.Helper()
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h := newHarness(t)
	rn := &memberDriveRunner{reclaimRunner: reclaimRunner{outcome: runner.DriveReclaimDeleted}}
	cfg := baseTestConfig(h, pg)
	cfg.Runner, cfg.RunnerTarget = rn, "docker"
	unit := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,'Platform','{}','{}','t')`, unit); err != nil {
		t.Fatalf("insert unit: %v", err)
	}
	return teamDrivePGEnv{pool: pool, pg: pg, srv: New(cfg), h: h, rn: rn, unit: unit}
}

func (e teamDrivePGEnv) teamDrive(t *testing.T, owner, name string) types.UserDrive {
	t.Helper()
	d, err := e.pg.CreateMemberTeamDrive(context.Background(),
		types.UserDrive{Name: name, Backend: types.DriveBackendDockerVolume, OwnerSubject: owner, CreatedBy: owner}, e.unit, 5)
	if err != nil {
		t.Fatalf("create team drive: %v", err)
	}
	return d
}

func (e teamDrivePGEnv) putWrite(id string, on bool) (int, string) {
	ctx := operatorCtx("sub-admin", "admin@corp.example", "admin")
	w := memberCall(ctx, e.srv.handlePutMemberTeamDrive, http.MethodPut, "/api/v1/drives/member/"+id+"/team",
		`{"members_can_write":`+strconv.FormatBool(on)+`}`, map[string]string{"id": id})
	return w.Code, w.Body.String()
}

// TestPutMemberTeamDrive_SuperOnlyAndAudited_PG: the write bit is set and answered with the drive's row and its unit's
// name, audited with before and after; a plain member drive is not found, and a drive being deleted is busy. The route
// is the super admin's (the authz table's classAdmin row).
func TestPutMemberTeamDrive_SuperOnlyAndAudited_PG(t *testing.T) {
	e := newTeamDrivePGEnv(t)
	ctx := context.Background()
	d := e.teamDrive(t, aliceSub, "Design assets")
	code, body := e.putWrite(d.ID.String(), true)
	var item client.MemberDriveAdminItem
	if code != http.StatusOK || json.Unmarshal([]byte(body), &item) != nil {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if !item.Team || !item.MembersCanWrite || item.TeamUnitID == nil || *item.TeamUnitID != e.unit || item.TeamUnitName != "Platform" {
		t.Errorf("answer = %+v, want the team row, writable by members, unit Platform", item)
	}
	if data := driveAuditData(t, e.h.audit, "drive.member.team"); data["members_can_write"] != true || data["was"] != false ||
		data["owner"] != aliceSub || data["unit_id"] != e.unit.String() || data["drive_id"] != d.ID.String() {
		t.Errorf("audit = %v", data)
	}
	if got, err := e.pg.GetMemberTeamDrive(ctx, d.ID); err != nil || !got.MembersCanWrite {
		t.Errorf("stored bit = %v (%v), want true", got.MembersCanWrite, err)
	}
	plain, err := e.pg.CreateMemberDrive(ctx, types.UserDrive{Name: "Notes", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub, CreatedBy: aliceSub}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := e.putWrite(plain.ID.String(), true); code != http.StatusNotFound {
		t.Errorf("a plain member drive = %d %s, want 404", code, body)
	}
	if _, _, err := e.pg.BeginMemberDriveDelete(ctx, aliceSub, d.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := e.putWrite(d.ID.String(), false); code != http.StatusConflict {
		t.Errorf("a drive being deleted = %d %s, want 409", code, body)
	}
}

// TestMemberTeamDriveDelete_OneCallPerClaim_PG: the delete of a team drive makes one physical call, on the shared
// object, for its claim; the same claim replayed makes none.
func TestMemberTeamDriveDelete_OneCallPerClaim_PG(t *testing.T) {
	e := newTeamDrivePGEnv(t)
	ctx := withActor(context.Background(), types.ActorHuman, aliceSub)
	d := e.teamDrive(t, aliceSub, "Design assets")
	tomb, started, err := e.pg.BeginMemberDriveDelete(ctx, aliceSub, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := e.srv.deleteMemberDrive(ctx, e.pg, tomb, started, memberDriveByOwner)
	second := e.srv.deleteMemberDrive(ctx, e.pg, tomb, started, memberDriveByOwner)
	if len(e.rn.calls) != 1 {
		t.Fatalf("physical calls = %d (first %+v, second %+v), want 1", len(e.rn.calls), first, second)
	}
	home := types.DriveTeamHome(d)
	if want := types.DriveObjectName(d, home); e.rn.calls[0].ObjectName != want || e.rn.calls[0].SubjectHash != "" {
		t.Errorf("call = %+v, want object %s with no subject", e.rn.calls[0], want)
	}
	if _, err := e.pg.GetMemberTeamDrive(context.Background(), d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row after a deleted outcome: err = %v, want ErrNotFound", err)
	}
}

// TestMemberTeamDriveCreate_CountsTowardLimit_PG: a team drive counts toward its maker's max_per_person.
func TestMemberTeamDriveCreate_CountsTowardLimit_PG(t *testing.T) {
	e := newTeamDrivePGEnv(t)
	ctx := context.Background()
	d := types.UserDrive{Name: "Design assets", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub, CreatedBy: aliceSub}
	if _, err := e.srv.insertMemberOrTeamDrive(ctx, e.pg, d, 1, &e.unit); err != nil {
		t.Fatalf("team drive: %v", err)
	}
	d.Name = "Notes"
	if _, err := e.srv.insertMemberOrTeamDrive(ctx, e.pg, d, 1, nil); !errors.Is(err, store.ErrMemberDriveLimit) {
		t.Errorf("a second drive over a limit of 1: err = %v, want ErrMemberDriveLimit", err)
	}
}

// TestMemberTeamDriveDelete_WaitsForAnyStartingRun_PG: while another person's run is STARTING, a team drive's delete
// is refused in use and issues nothing; a plain member drive's still waits only for its owner's runs.
func TestMemberTeamDriveDelete_WaitsForAnyStartingRun_PG(t *testing.T) {
	e := newTeamDrivePGEnv(t)
	ctx := context.Background()
	d := e.teamDrive(t, aliceSub, "Design assets")
	plain, err := e.pg.CreateMemberDrive(ctx, types.UserDrive{Name: "Notes", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub, CreatedBy: aliceSub}, 5)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "sub-member-carol", Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunStarting, SPIFFEID: "spiffe://wardyn.local/agent-run/" + uuid.NewString(), RunnerTarget: "docker"}); err != nil {
		t.Fatal(err)
	}
	wantReason(t, deleteMyDrive(e.srv, aliceSub, d.ID.String()), http.StatusConflict, reasonMemberDriveInUse)
	var marked bool
	var issues int
	if err := e.pool.QueryRow(ctx, `SELECT delete_started_at IS NOT NULL, delete_issues FROM user_drives WHERE id = $1`, d.ID).Scan(&marked, &issues); err != nil {
		t.Fatal(err)
	}
	if marked || issues != 0 || len(e.rn.calls) != 0 {
		t.Fatalf("marked %v, issues %d, calls %d; want no mark, no issue, no call", marked, issues, len(e.rn.calls))
	}
	if w := deleteMyDrive(e.srv, aliceSub, plain.ID.String()); w.Code != http.StatusNoContent || len(e.rn.calls) != 1 {
		t.Fatalf("plain delete beside another person's starting run = %d (calls %d), want 204: %s", w.Code, len(e.rn.calls), w.Body.String())
	}
}

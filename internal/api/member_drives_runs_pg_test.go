// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestMemberDriveNeverLegacyWinner: Alice holds an admin all-tier drive and
// creates her own. /me.user_drive and drive.enabled resolve the admin drive,
// byte-identical to before her drive existed; the catalogue marks only the
// admin drive legacy.
func TestMemberDriveNeverLegacyWinner(t *testing.T) {
	pg := store.NewPG(throwawayPGPool(t))
	srv := New(baseTestConfig(newHarness(t), pg))
	srv.cfg.RunnerTarget = "docker"
	ctx := context.Background()
	team, err := pg.UpsertUserDrive(ctx, *driveFixture(func(d *types.UserDrive) { d.Name = "Team" }), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpsertUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: types.CapabilitySubjectAll, DriveID: team.ID, Enabled: true}, false); err != nil {
		t.Fatal(err)
	}
	alice := personCtx(aliceSub)
	legacy := func() string {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(alice)
		me, unresolved := srv.resolveMeUserDrive(r)
		resolved, refusal := srv.authorizeRequestDrive(r, createRunRequest{Drive: &client.DriveSelection{Enabled: true}}, governanceCeiling{})
		if refusal != nil || resolved == nil {
			t.Fatalf("drive.enabled = %+v %+v", resolved, refusal)
		}
		b, err := json.Marshal([]any{me, unresolved, resolved})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	before := legacy()
	mine, err := pg.CreateMemberDrive(ctx, types.UserDrive{Name: "Mine", Backend: types.DriveBackendDockerVolume, OwnerSubject: aliceSub, CreatedBy: aliceSub}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if after := legacy(); after != before {
		t.Fatalf("a member drive moved the legacy drive:\nbefore %s\nafter  %s", before, after)
	}
	reach, err := srv.reachableDrives(alice, 0)
	if err != nil || len(reach) != 2 {
		t.Fatalf("reachable = %+v %v, want the admin drive and hers", reach, err)
	}
	for _, d := range reach {
		if want := d.winner.Drive.ID == team.ID; d.legacy != want || d.owned == want {
			t.Errorf("%s: legacy %v owned %v", d.winner.Drive.Name, d.legacy, d.owned)
		}
	}
	if drives, f := authorizeDrives(srv, alice, drivesRequest(mine.ID.String())); f != nil || len(drives) != 1 || drives[0].legacy {
		t.Errorf("attaching her drive = %+v %+v, want one non-legacy drive", drives, f)
	}
}

// TestPG_QueueStartMemberDriveRecheck: a member drive deleted, or being
// deleted, while the run waited ends the promoted run FAILED as
// drive_not_found, with nothing minted; one still there starts with it.
func TestPG_QueueStartMemberDriveRecheck(t *testing.T) {
	for name, change := range map[string]string{
		"deleted":       `DELETE FROM user_drives WHERE id = $1`,
		"being deleted": `UPDATE user_drives SET delete_started_at = now() WHERE id = $1`,
		"kept":          "",
	} {
		t.Run(name, func(t *testing.T) {
			e := newQueueEnv(t)
			e.srv.cfg.RunnerTarget = "docker"
			mine, err := e.pg.CreateMemberDrive(context.Background(), types.UserDrive{Name: "Mine", Backend: types.DriveBackendDockerVolume,
				OwnerSubject: types.CanonicalUserSubject(e.personP), CreatedBy: e.personP}, 3)
			if err != nil {
				t.Fatal(err)
			}
			pool := e.cappedPool("Busy", 1)
			occupant := e.occupy(pool)
			body := strings.Replace(endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`),
				`"no_repositories_or_drives":true`, fmt.Sprintf(`"drives":[{"drive_id":%q}]`, mine.ID), 1)
			w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
			var queued queuedLaunchAnswer
			if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil || w.Code != http.StatusCreated || queued.State != types.RunQueued {
				t.Fatalf("launch with her drive into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
			}
			if change == "" {
				e.free(occupant)
				e.waitStarted(queued.ID)
				e.eventually("the sandbox with her drive", func() bool {
					fr := e.runner.fakeRunner
					fr.mu.Lock()
					defer fr.mu.Unlock()
					return fr.lastSpec.RunID == queued.ID && len(fr.lastSpec.Drives) == 1 && fr.lastSpec.Drives[0].DriveID == mine.ID
				})
				return
			}
			e.exec(change, mine.ID)
			e.free(occupant)
			got := e.waitFailed(queued.ID)
			if got.StatusDetail != "queue: drive_not_found" || got.FailureHint != driveNotFoundMsg(mine.ID.String()) {
				t.Errorf("run = %q %q, want queue: drive_not_found", got.StatusDetail, got.FailureHint)
			}
			if got.SPIFFEID != "" || len(e.audits(got.ID, "identity.mint")) != 0 {
				t.Error("a refused start minted an identity")
			}
		})
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// secondPlaceSetup is her own pool holding her one laptop, online and allowing drives under /srv/dev/work
// (stream), and the share "team-share" allocated to her as directory alice, its second place set by bind.
func secondPlaceSetup(t *testing.T, e *placementEnv, bind func(d *types.UserDrive, laptop uuid.UUID)) (types.RunnerPool, uuid.UUID, types.UserDrive) {
	t.Helper()
	ctx := context.Background()
	e.srv.cfg.RunnerTarget = "docker"
	pool := e.createOwnPool(e.person, "Mine")
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.maxRuns(laptop, e.personP, types.RunnerMaxRunsCeiling)
	e.addToPool(e.person, pool.ID, laptop)
	e.stream(laptop, types.CC1, types.CC2, types.CC3)
	d := driveFixture(func(d *types.UserDrive) {
		d.Name, d.Backend, d.HostRoot, d.HomeTemplate, d.SizeMiB = "team-share", types.DriveBackendHostPath, "/srv/shares/team", types.HomeTemplateSub, 0
	})
	bind(d, laptop)
	drive, err := e.pg.UpsertUserDrive(ctx, *d, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.UpsertUserDriveGrant(ctx, *grantFixture(drive.ID, func(g *types.UserDriveGrant) { g.Subject, g.HomeOverride = e.personP, "alice" }), true); err != nil {
		t.Fatal(err)
	}
	return pool, laptop, drive
}

// A share reaches her own runner only once its second place is bound to that runner (M10, U2 a): bound,
// the run is placed there and mounts the share from the second place's path; unbound, or bound to another
// person's runner, the launch is refused with the existing drive_not_in_pool shape and nothing runs. The
// catalogue New Run reads for that pool answers the same way.
func TestPG_DriveSecondPlace_ReachesOnlyItsBoundRunner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bind   func(e *placementEnv) func(d *types.UserDrive, laptop uuid.UUID)
		reason string // the launch's refusal, "" for a run on her laptop
		msg    string
		avail  string // the catalogue's unavailable_reason for her pool
	}{
		{"bound to her laptop", func(*placementEnv) func(*types.UserDrive, uuid.UUID) {
			return func(d *types.UserDrive, laptop uuid.UUID) {
				d.AltBackend, d.AltHostRoot, d.AltRunnerID = types.DriveBackendHostPath, "/srv/dev/work/team", &laptop
			}
		}, "", "", ""},
		{"unbound", func(*placementEnv) func(*types.UserDrive, uuid.UUID) { return func(*types.UserDrive, uuid.UUID) {} },
			"drive_not_in_pool", `drive: "team-share" mounts only from the organisation's runners — choose an organisation pool, or launch without drive`, client.MeDriveUnavailableNotInPool},
		{"bound to another person's runner", func(e *placementEnv) func(*types.UserDrive, uuid.UUID) {
			theirs := e.claimedRunner("bob@example.com")
			return func(d *types.UserDrive, _ uuid.UUID) {
				d.AltBackend, d.AltHostRoot, d.AltRunnerID = types.DriveBackendHostPath, "/srv/dev/work/team", &theirs
			}
		}, "drive_not_in_pool", `drive: "team-share" is bound to a different runner — choose an organisation pool, or launch without drive`, client.MeDriveUnavailableBoundElsewhere},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			pool, laptop, drive := secondPlaceSetup(t, e, tc.bind(e))

			cw := e.call(e.person, http.MethodGet, "/api/v1/me/drives?runner_pool_id="+pool.ID.String(), "")
			var cat client.MeDriveCatalogue
			if err := json.Unmarshal(cw.Body.Bytes(), &cat); err != nil || cw.Code != http.StatusOK || len(cat.Drives) != 1 {
				t.Fatalf("catalogue = %d %s", cw.Code, cw.Body)
			}
			row := cat.Drives[0]
			if row.UnavailableReason != tc.avail {
				t.Errorf("catalogue unavailable_reason = %q, want %q", row.UnavailableReason, tc.avail)
			}
			if bound := tc.avail == ""; bound != (row.MountedFrom != nil) || (bound && (row.MountedFrom.Runner != "laptop" || row.MountedFrom.Path != "/srv/dev/work/team" ||
				row.MountsFrom != "Organisation runners and laptop")) {
				t.Errorf("catalogue mounts_from %q mounted_from %+v", row.MountsFrom, row.MountedFrom)
			}
			if tc.avail == client.MeDriveUnavailableBoundElsewhere && row.MountsFrom != "Organisation runners" {
				t.Errorf("another person's runner is named to her: mounts_from %q", row.MountsFrom)
			}

			w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(`"drives":[{"drive_id":"`+drive.ID.String()+`"}],"runner_pool_id":"`+pool.ID.String()+`"`))
			var out struct {
				ID     uuid.UUID `json:"id"`
				Error  string    `json:"error"`
				Reason string    `json:"reason"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if tc.reason != "" {
				if w.Code != http.StatusUnprocessableEntity || out.Reason != tc.reason || out.Error != tc.msg {
					t.Fatalf("launch = %d %q %q, want 422 %q %q", w.Code, out.Reason, out.Error, tc.reason, tc.msg)
				}
				if n := e.count(`SELECT count(*) FROM agent_runs WHERE created_by = $1`, e.personP); n != 0 {
					t.Fatalf("a refused launch left %d run(s)", n)
				}
				return
			}
			if w.Code != http.StatusCreated {
				t.Fatalf("launch = %d %s", w.Code, w.Body)
			}
			run, err := e.pg.GetRun(context.Background(), out.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Placement != types.PlacementLocal || run.RunnerID == nil || *run.RunnerID != laptop {
				t.Fatalf("run placed %s on %v, want her laptop %s", run.Placement, run.RunnerID, laptop)
			}
			spec := e.dispatchedSpec(run.ID)
			if len(spec.Drives) != 1 || spec.Drives[0].ObjectName != "/srv/dev/work/team/alice" || spec.Drives[0].HostRoot != "/srv/dev/work/team" ||
				spec.Drives[0].RunnerID == nil || *spec.Drives[0].RunnerID != laptop {
				t.Fatalf("dispatched drives = %+v, want the second place's /srv/dev/work/team/alice bound to the laptop", spec.Drives)
			}
		})
	}
}

// On the organisation's runners a bound share is the share it was: launched into an organisation pool, it
// meets the first place's own checks and gets the same answer, byte for byte, as the same share unbound
// (here this deployment allows no share root, so both are refused alike before anything runs).
func TestPG_DriveSecondPlace_OrganisationRunnersAnswerAsUnbound(t *testing.T) {
	answer := func(bind func(*types.UserDrive, uuid.UUID)) (int, string) {
		e := newPlacementEnv(t)
		_, _, drive := secondPlaceSetup(t, e, bind)
		org := e.createPool("Org", types.RunnerPoolOrganization)
		e.addExecutors(org.ID, "alpha")
		e.setPeople("alpha", everyonePeople)
		w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(`"drives":[{"drive_id":"`+drive.ID.String()+`"}],"runner_pool_id":"`+org.ID.String()+`"`))
		return w.Code, w.Body.String()
	}
	bc, bound := answer(func(d *types.UserDrive, laptop uuid.UUID) {
		d.AltBackend, d.AltHostRoot, d.AltRunnerID = types.DriveBackendHostPath, "/srv/dev/work/team", &laptop
	})
	uc, unbound := answer(func(*types.UserDrive, uuid.UUID) {})
	if bc != uc || bound != unbound || bc == http.StatusCreated {
		t.Fatalf("organisation pool: bound = %d %s, unbound = %d %s; want the same refusal", bc, bound, uc, unbound)
	}
}

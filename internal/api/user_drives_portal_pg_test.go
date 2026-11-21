// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Two portals on one issuer serve the same person. A run the first portal created with a portal drive is read, killed,
// listed, read for its output and offered an attach ticket through the first portal only: the second answers 403
// portal_run_scope by id and omits it from the list, whatever its own pin, while a run with no portal drive from either
// portal is unchanged. The same answers hold after the first portal's allocation is removed and after the first portal
// is revoked: neither ends the run's scope, and the second portal never takes over what the first created. The run is
// created marked, through the store's portal fence, as a launch creates it (the launch itself is tested in user_drives_portal_launch_pg_test.go).
func TestPortalTeamDrive_RunScopeAcrossPortalsOnOneIssuer_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	ctx := context.Background()
	b := e.bind(t)
	first, second := e.portal(t, b, &e.plat.ID), e.portal(t, b, &e.plat.ID)
	person, firstTok := e.person(t, first, "alice")
	_, secondTok := e.person(t, second, "alice")
	plainRun := e.run(t, person, &e.team.ID, first.ID)
	// The marked run is created as a launch creates it, through the store's portal fence (created_via, portal_drives).
	id, now := uuid.New(), time.Now().UTC()
	driveRun, err := e.st.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: person, Agent: "claude-code",
		Task: "t", ConfinementClass: types.CC2, State: types.RunRunning, UnitID: &e.team.ID, CreatedVia: &first.ID, PortalDrives: true,
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker"})
	if err != nil || !driveRun.PortalDrives {
		t.Fatalf("create the portal drive run through the fence = %+v, %v", driveRun, err)
	}
	team, err := e.st.UpsertUserDrive(ctx, types.UserDrive{Name: "Scope Data", Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, Writable: true, Reclaim: types.DriveReclaimRetain, Team: true, CreatedBy: "admin@example.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := e.st.AddUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: types.DriveSubjectPortal, Subject: first.ID.String(),
		DriveID: team.ID, Enabled: true, CreatedBy: "admin@example.com"}, true)
	if err != nil {
		t.Fatal(err)
	}

	// scoped checks every door through the second portal and the first, and the unmarked control.
	scoped := func(when string, firstLive bool) {
		t.Helper()
		id := driveRun.ID.String()
		if firstLive {
			if w := e.get(t, firstTok, "/api/v1/runs/"+id); w.Code != http.StatusOK {
				t.Errorf("%s: the creating portal reads its drive run = %d %s, want 200", when, w.Code, w.Body)
			}
			mine := decodePortalRuns(t, e.get(t, firstTok, "/api/v1/runs").Body.Bytes())
			if len(mine) != 2 {
				t.Errorf("%s: the creating portal lists %d runs, want both", when, len(mine))
			}
		}
		for _, route := range []struct{ method, suffix string }{
			{"GET", ""}, {"POST", "/kill"}, {"GET", "/events"}, {"GET", "/output"}, {"POST", "/attach-ticket"},
		} {
			w := do(t, e.h.srv, route.method, "/api/v1/runs/"+id+route.suffix, secondTok, "")
			if w.Code != http.StatusForbidden || errorReason(w) != "portal_run_scope" {
				t.Errorf("%s: %s %s through the other portal = %d %s, want 403 portal_run_scope", when, route.method, route.suffix, w.Code, w.Body)
			}
		}
		if w := e.get(t, secondTok, "/api/v1/runs/"+plainRun.ID.String()); w.Code != http.StatusOK {
			t.Errorf("%s: the other portal reads a run with no portal drive = %d %s, want today's 200", when, w.Code, w.Body)
		}
		seen := map[uuid.UUID]bool{}
		for _, r := range decodePortalRuns(t, e.get(t, secondTok, "/api/v1/runs").Body.Bytes()) {
			seen[r.ID] = true
		}
		if seen[driveRun.ID] || !seen[plainRun.ID] {
			t.Errorf("%s: the other portal lists %v, want the plain run and not the drive run", when, seen)
		}
	}
	scoped("before any change", true)

	if _, err := e.st.DeleteUserDriveGrant(ctx, allocation.ID); err != nil {
		t.Fatal(err)
	}
	scoped("after the first portal's allocation was removed", true)

	if _, _, err := e.st.RevokeDelegate(ctx, first.ID, time.Now().UTC(), false); err != nil {
		t.Fatal(err)
	}
	scoped("after the first portal was revoked", false)
}

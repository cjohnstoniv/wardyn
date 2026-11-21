// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Two portals on one issuer serve the same person. A run that mounted the first portal's drive is read, killed
// and listed through the first portal only: the second answers 403 portal_run_scope by id and omits it from the
// list, whatever its own pin, while a run with no portal drive from either portal is unchanged.
func TestPortalTeamDrive_RunScopeAcrossPortalsOnOneIssuer_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	b := e.bind(t)
	first, second := e.portal(t, b, &e.plat.ID), e.portal(t, b, &e.plat.ID)
	person, firstTok := e.person(t, first, "alice")
	_, secondTok := e.person(t, second, "alice")
	driveRun := e.run(t, person, &e.team.ID, first.ID)
	plainRun := e.run(t, person, &e.team.ID, first.ID)
	if _, err := e.st.Pool.Exec(context.Background(), `UPDATE agent_runs SET portal_drives = true WHERE id = $1`, driveRun.ID); err != nil {
		t.Fatalf("mark the run as mounting a portal drive: %v", err)
	}

	if w := e.get(t, firstTok, "/api/v1/runs/"+driveRun.ID.String()); w.Code != http.StatusOK {
		t.Errorf("the creating portal reads its drive run = %d %s, want 200", w.Code, w.Body)
	}
	for _, route := range []struct{ method, suffix string }{{"GET", ""}, {"POST", "/kill"}, {"GET", "/events"}} {
		w := do(t, e.h.srv, route.method, "/api/v1/runs/"+driveRun.ID.String()+route.suffix, secondTok, "")
		if w.Code != http.StatusForbidden || errorReason(w) != "portal_run_scope" {
			t.Errorf("%s %s through the other portal = %d %s, want 403 portal_run_scope", route.method, route.suffix, w.Code, w.Body)
		}
	}
	if w := e.get(t, secondTok, "/api/v1/runs/"+plainRun.ID.String()); w.Code != http.StatusOK {
		t.Errorf("the other portal reads a run with no portal drive = %d %s, want today's 200", w.Code, w.Body)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range decodePortalRuns(t, e.get(t, secondTok, "/api/v1/runs").Body.Bytes()) {
		seen[r.ID] = true
	}
	if seen[driveRun.ID] || !seen[plainRun.ID] {
		t.Errorf("the other portal lists %v, want the plain run and not the drive run", seen)
	}
	mine := decodePortalRuns(t, e.get(t, firstTok, "/api/v1/runs").Body.Bytes())
	if len(mine) != 2 {
		t.Errorf("the creating portal lists %d runs, want both", len(mine))
	}
}

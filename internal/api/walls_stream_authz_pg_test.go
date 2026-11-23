// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit-admin arm of a run's live stream (wallsRunReadable) against a real unit tree. A gate that lets the caller
// in is pinned as "not 401, 403 or 404, and no not_owner row for the run", never by the answer past the gate.

// wallsStreamUnitEnv is newUnitAdminEnv's tree with four running runs: inside (in ua-a1, below adminA's ua-a),
// outside (in ua-b), own (adminA's own, in ua-b) and noUnit (frozen to no unit).
type wallsStreamUnitEnv struct {
	*unitAdminEnv
	inside, outside, own, noUnit uuid.UUID
}

// wallsStreamUnitEnvFor builds the tree and seeds its runs.
func wallsStreamUnitEnvFor(t *testing.T) *wallsStreamUnitEnv {
	t.Helper()
	e := &wallsStreamUnitEnv{unitAdminEnv: newUnitAdminEnv(t, false)}
	e.inside = e.seedRun(&e.a1.ID, "sub-walls-stream-runner")
	e.outside = e.seedRun(&e.b.ID, "sub-walls-stream-runner")
	e.own = e.seedRun(&e.b.ID, uaSubAdminA)
	e.noUnit = e.seedRun(nil, "sub-walls-stream-runner")
	return e
}

// seedRun stores a running run of owner's frozen to unit (nil: none).
func (e *wallsStreamUnitEnv) seedRun(unit *uuid.UUID, owner string) uuid.UUID {
	e.t.Helper()
	id, now := uuid.New(), time.Now().UTC()
	run := types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code",
		Task: "t", ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker"}
	if unit != nil {
		u := *unit
		run.UnitID = &u
	}
	if _, err := e.pg.CreateRun(context.Background(), run); err != nil {
		e.t.Fatalf("create run: %v", err)
	}
	return id
}

// stream asks for run's live stream as cookie.
func (e *wallsStreamUnitEnv) stream(cookie *http.Cookie, run uuid.UUID) *httptest.ResponseRecorder {
	e.t.Helper()
	return wallsRoutesServe(e.t, e.srv.Handler(), "/api/v1/runs/"+run.String()+"/walls/stream", cookie, "")
}

// notOwnerRows counts the not_owner refusal rows written for run.
func (e *wallsStreamUnitEnv) notOwnerRows(run uuid.UUID) int {
	n := 0
	for _, ev := range e.h.audit.snapshot() {
		var data map[string]any
		if ev.Action == authz.AuditAction && ev.RunID != nil && *ev.RunID == run &&
			json.Unmarshal(ev.Data, &data) == nil && data["reason"] == string(authz.ReasonNotOwner) {
			n++
		}
	}
	return n
}

// wantPast fails the test unless cookie's request for run's live stream passes the gate.
func (e *wallsStreamUnitEnv) wantPast(who string, cookie *http.Cookie, run uuid.UUID) {
	e.t.Helper()
	before := e.notOwnerRows(run)
	if w := e.stream(cookie, run); wallsStreamRefused(w.Code) {
		e.t.Errorf("%s: %d %s, want past the gate", who, w.Code, w.Body.String())
	}
	if after := e.notOwnerRows(run); after != before {
		e.t.Errorf("%s: %d not_owner rows written, want none", who, after-before)
	}
}

// wantMissing fails the test unless cookie's request for run's live stream answers the missing run's 404.
func (e *wallsStreamUnitEnv) wantMissing(who string, cookie *http.Cookie, run uuid.UUID) {
	e.t.Helper()
	got := e.stream(cookie, run)
	if got.Code != http.StatusNotFound {
		e.t.Errorf("%s: %d %s, want the missing run's 404", who, got.Code, got.Body.String())
	}
	wallsStreamSameAnswer(e.t, who, got, e.stream(cookie, uuid.New()))
}

// TestRunWallsStreamUnitArmInSubtree: a unit admin reaches the live stream of a run in a unit below their grant,
// and the request writes no not_owner row.
func TestRunWallsStreamUnitArmInSubtree(t *testing.T) {
	e := wallsStreamUnitEnvFor(t)
	e.wantPast("a unit admin of the run's parent unit", e.adminA, e.inside)
	e.wantPast("a unit admin of the run's unit", e.adminA1, e.inside)
}

// TestRunWallsStreamUnitArmOutOfSubtree: a run in a unit outside the caller's units answers the missing run's 404.
func TestRunWallsStreamUnitArmOutOfSubtree(t *testing.T) {
	e := wallsStreamUnitEnvFor(t)
	e.wantMissing("a unit admin of another unit", e.adminA, e.outside)
	e.wantMissing("a unit admin of ua-a1", e.adminA1, e.outside)
}

// TestRunWallsStreamUnitArmOwnRun: a unit admin's own run, in a unit outside their units, passes as the owner's.
func TestRunWallsStreamUnitArmOwnRun(t *testing.T) {
	e := wallsStreamUnitEnvFor(t)
	e.wantPast("a unit admin on their own run", e.adminA, e.own)
}

// TestRunWallsStreamUnitArmNullUnit: a run frozen to no unit answers the missing run's 404 to every unit admin, a
// Default-unit admin included.
func TestRunWallsStreamUnitArmNullUnit(t *testing.T) {
	e := wallsStreamUnitEnvFor(t)
	e.wantMissing("a unit admin", e.adminA, e.noUnit)
	const sub = "sub-walls-stream-default-admin"
	e.grant(types.GovernanceUnit{ID: store.DefaultUnitID, Name: "Default"}, types.CapabilitySubjectUser, sub)
	e.wantMissing("a Default-unit admin", uaSession(t, sub, []string{}, false), e.noUnit)
}

// TestRunWallsStreamUnitArmRevokedGrant: the scope is read on every request, so the request after a revoke answers
// the missing run's 404.
func TestRunWallsStreamUnitArmRevokedGrant(t *testing.T) {
	e := wallsStreamUnitEnvFor(t)
	const sub = "sub-walls-stream-revoked"
	g := e.grant(e.a1, types.CapabilitySubjectUser, sub)
	cookie := uaSession(t, sub, []string{}, false)
	e.wantPast("a unit admin before the revoke", cookie, e.inside)
	if _, err := store.RevokeUnitAdminQ(context.Background(), e.pool, e.a1.ID, g.ID); err != nil {
		t.Fatalf("revoke the grant: %v", err)
	}
	e.wantMissing("a unit admin after the revoke", cookie, e.inside)
}

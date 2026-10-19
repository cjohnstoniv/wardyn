// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The org walls on the lanes that do not resolve through POST /runs' policy step: a Record Mode
// session, a launch that waited in a pool's queue, and a later change to a run's end.

// recordWalls allows two hosts and denies one; it raises no floor, so the fake runner still serves
// the session.
func recordWalls() types.OrgWallsWrite {
	allowed := []string{"api.anthropic.com", "pypi.org"}
	denied := []string{"evil.example"}
	return types.OrgWallsWrite{Enabled: true, Spec: types.OrgWallsSpec{AllowedDomains: &allowed, DeniedDomains: &denied}}
}

// recordUnderWalls records an operator workspace as an SSO super admin, the walls on or off, and
// returns the egress policy the session's proxy was handed.
func recordUnderWalls(t *testing.T, walls bool) types.RunPolicySpec {
	t.Helper()
	root, project := memberProjectRoot(t)
	fr := &fakeRunner{}
	srv := pgHarnessOnPool(t, fr, throwawayPGPool(t))
	srv.cfg.OIDC = &oidc.Authenticator{}
	srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	srv.router = srv.routes()
	h := supplierPG{srv: srv, fr: fr}
	ws := h.workspace(t, false, "rec-"+uuid.NewString()[:8], project, nil)
	if walls {
		if _, err := srv.cfg.Store.(store.OrgWallsStore).PutOrgWalls(context.Background(), recordWalls(), "seed"); err != nil {
			t.Fatal(err)
		}
	}
	super := ssoSession(t, "sub-walls-recorder", "recorder@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.String()+"/record", super, `{"name":"capture"}`); w.Code != http.StatusAccepted {
		t.Fatalf("record = %d %s", w.Code, w.Body)
	}
	fr.waitForSandbox(t)
	return fr.lastSpec.ProxyConfig.Policy
}

// TestOrgWallsBindRecordSessions: with the walls on, a super admin's learning session reaches only
// hosts the walls allow and carries their denies; with them off it allows all egress, as before.
func TestOrgWallsBindRecordSessions(t *testing.T) {
	if off := recordUnderWalls(t, false); !off.AllowAllEgress {
		t.Fatalf("walls off: the learning session does not allow all egress: %+v", off)
	}
	on := recordUnderWalls(t, true)
	if on.AllowAllEgress {
		t.Fatal("walls on: the learning session still allows all egress")
	}
	if !slices.Contains(on.DeniedDomains, "evil.example") {
		t.Errorf("walls on: denied %v lacks evil.example", on.DeniedDomains)
	}
	allowed := *recordWalls().Spec.AllowedDomains
	for _, d := range on.AllowedDomains {
		if !ceilingDenies(allowed, d) {
			t.Errorf("walls on: the session reaches %s, which the walls do not allow (allowed %v)", d, on.AllowedDomains)
		}
	}
}

// TestOrgWallsQueuedBreakGlassStartsAsTagged: an admin-token launch with a reason that waits in a
// full pool starts as the break-glass run it was tagged: unnarrowed, break_glass, one audit row.
func TestOrgWallsQueuedBreakGlassStartsAsTagged(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Shared", 1)
	occupant := e.occupy(pool)
	if _, err := e.pg.PutOrgWalls(context.Background(), testWalls(true), "seed"); err != nil {
		t.Fatal(err)
	}
	body := endBody("background", `"runner_pool_id":"`+pool.ID.String()+`","inline_policy":{"min_confinement_class":"CC1","allow_all_egress":true}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+adminToken)
	r.Header.Set(breakGlassReasonHeader, "pool incident")
	w := httptest.NewRecorder()
	panicFails(t, e.srv.Handler()).ServeHTTP(w, r)
	var queued queuedLaunchAnswer
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &queued) != nil || queued.State != types.RunQueued {
		t.Fatalf("launch into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	e.free(occupant)
	e.sweep()
	run := e.waitStarted(queued.ID)
	if policy := e.specFor(queued.ID).ProxyConfig.Policy; !policy.AllowAllEgress {
		t.Errorf("the started break-glass run was narrowed by the walls: %+v", policy)
	}
	if !run.BreakGlass {
		t.Error("run row break_glass = false, want true")
	}
	if rows := e.audits(queued.ID, "run.break_glass.open"); len(rows) != 1 {
		t.Errorf("run.break_glass.open rows = %d, want exactly 1", len(rows))
	}
}

// TestOrgWallsCapAnEarlierRunsEnd: a run created before the walls went on, whose profile allows a day,
// is held to the walls' hour when its owner later moves its end.
func TestOrgWallsCapAnEarlierRunsEnd(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.lifetimePool("Week", `{"default_sec":3600,"max_sec":604800}`)
	profile, err := e.pg.UpsertGovernanceProfile(context.Background(), types.GovernanceProfile{
		Name: "a day", CreatedBy: "seed", Ceiling: govTestPolicy,
		Limits: types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 86400, AllowNoEnd: true, UserChangesLimits: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.seedAssignment(profile, e.personP)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`))
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if _, err := e.pg.PutOrgWalls(context.Background(), testWalls(true), "seed"); err != nil {
		t.Fatal(err)
	}
	far := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	w = e.call(e.person, http.MethodPatch, "/api/v1/runs/"+created.ID.String(), `{"ends_at":"`+far+`"}`)
	var resp runEndWaitResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("owner's PATCH = %d %s", w.Code, w.Body)
	}
	if resp.EndsAt == nil || time.Until(*resp.EndsAt) > wallsMaxEnd*time.Second || !slices.Contains(resp.Capped, "ends_at") {
		t.Errorf("ends_at = %v capped %v, want capped at most %ds ahead", resp.EndsAt, resp.Capped, wallsMaxEnd)
	}
}

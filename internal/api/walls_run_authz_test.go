// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runWallsSameAsMissing asserts got equals the missing run's answer to the same caller: status, headers and body.
func runWallsSameAsMissing(t *testing.T, got, missing *httptest.ResponseRecorder, who string) {
	t.Helper()
	if got.Code != http.StatusNotFound || missing.Code != got.Code {
		t.Fatalf("%s: a foreign run answers %d, a missing one %d; want 404 for both", who, got.Code, missing.Code)
	}
	if !reflect.DeepEqual(got.Header(), missing.Header()) {
		t.Errorf("%s: headers %v, a missing run's %v", who, got.Header(), missing.Header())
	}
	if got.Body.String() != missing.Body.String() {
		t.Errorf("%s: body %q, a missing run's %q", who, got.Body.String(), missing.Body.String())
	}
}

// TestRunWallsUnitAdminGetsTheForeignRun404: an admin of the run's own unit is not the run's owner and not the
// security tier, so route B answers them the missing run's 404, byte for byte; the owner reads it.
func TestRunWallsUnitAdminGetsTheForeignRun404(t *testing.T) {
	e := newUnitAuditEnv(t)
	got := e.call(e.adminA, http.MethodGet, buildPath(runWallsPattern, e.runA.String()), "")
	missing := e.call(e.adminA, http.MethodGet, buildPath(runWallsPattern, uuid.NewString()), "")
	runWallsSameAsMissing(t, got, missing, "unit admin")

	owner := ssoSession(t, uaRunOwner, "run-owner@corp.example", oidc.RoleUser)
	if w := e.call(owner, http.MethodGet, buildPath(runWallsPattern, e.runA.String()), ""); w.Code != http.StatusOK {
		t.Fatalf("owner: %d %s", w.Code, w.Body.String())
	}
}

// TestRunWallsForeignRunIs404: a member who does not own the run gets the missing run's 404.
func TestRunWallsForeignRunIs404(t *testing.T) {
	srv, st := runWallsFixture(t)
	run := runWallsSeed(st, pvOwner, nil)
	other := pvMember(t, "sub-walls-other").cookie
	w := doSSO(t, srv, http.MethodGet, buildPath(runWallsPattern, run.ID.String()), other, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-owner: %d %s, want 404", w.Code, w.Body.String())
	}
	assertSameAsMissing404(t, srv, http.MethodGet, runWallsPattern, other, "", w, "a non-owning member")
}

// TestRunWallsOtherOwnersRunIsRefused: member A's sealed run carries its door for A; member B, who owns a run of
// their own, gets A's run as the missing run's 404, byte for byte, with no egress key.
func TestRunWallsOtherOwnersRunIsRefused(t *testing.T) {
	srv, st := runWallsFixture(t)
	const memberB = "sub-walls-member-b"
	sealed := func(r *types.AgentRun) {
		r.WallPreset = &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network", Scope: types.WallPresetScopeBuiltin, Version: 1}
	}
	runA, runB := runWallsSeed(st, pvOwner, sealed), runWallsSeed(st, memberB, sealed)
	a, b := pvMember(t, pvOwner).cookie, pvMember(t, memberB).cookie

	for _, own := range []struct {
		c   *http.Cookie
		run types.AgentRun
	}{{a, runA}, {b, runB}} {
		if w, body := runWallsGet(t, srv, own.c, own.run.ID, ""); w.Code != http.StatusOK || body.Run.Egress != wallsEgressSealed {
			t.Fatalf("own run: %d egress %q, want 200 %q (%s)", w.Code, body.Run.Egress, wallsEgressSealed, w.Body.String())
		}
	}
	got := doSSO(t, srv, http.MethodGet, buildPath(runWallsPattern, runA.ID.String()), b, "")
	missing := doSSO(t, srv, http.MethodGet, buildPath(runWallsPattern, uuid.NewString()), b, "")
	runWallsSameAsMissing(t, got, missing, "member B")
	if strings.Contains(got.Body.String(), `"egress"`) {
		t.Errorf("member B: the refusal carries egress: %s", got.Body.String())
	}
}

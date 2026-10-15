// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeAudited reports whether runner.revoke was recorded for the fixture's
// runner by `by` with `reason`.
func (f *admissionFixture) revokeAudited(t *testing.T, by, reason string) bool {
	t.Helper()
	audit := f.srv.cfg.Audit.(*syncAudit)
	audit.mu.Lock()
	defer audit.mu.Unlock()
	for _, ev := range audit.events {
		if ev.Action != placement.ActionRunnerRevoke || ev.Target != f.rn.ID.String() {
			continue
		}
		var data map[string]any
		if json.Unmarshal(ev.Data, &data) == nil && ev.Actor == by && data["by"] == by && data["reason"] == reason && data["runs_killed"] == float64(1) {
			return true
		}
	}
	return false
}

// unrevoked fails the test if a refused revoke touched the runner or its run.
func (f *admissionFixture) unrevoked(t *testing.T) {
	t.Helper()
	if got, _ := f.st.GetRunner(f.ctx, f.rn.ID); got.State != types.RunnerClaimed || got.RevokedBy != "" {
		t.Fatalf("a refused revoke changed the runner: %+v", got)
	}
	if got, _ := f.st.GetRun(f.ctx, f.run.ID); got.State != types.RunRunning {
		t.Fatalf("a refused revoke ended the run: %s", got.State)
	}
}

func TestRunnerRevokePG_RefusedDoorsChangeNothing(t *testing.T) {
	for _, tc := range []struct {
		name, method, sub, role, bearer, query string
		admin                                  bool
		want                                   int
	}{
		{name: "another person", sub: "bob", role: oidc.RoleUser, want: http.StatusNotFound},
		{name: "another person as admin on /me", sub: "bob", role: oidc.RoleAdmin, want: http.StatusNotFound},
		{name: "admin bearer on /me", bearer: adminToken, want: http.StatusForbidden},
		{name: "unknown reason", sub: "admission-owner", role: oidc.RoleUser, query: "?reason=lost", want: http.StatusBadRequest},
		{name: "owner on the security door", sub: "admission-owner", role: oidc.RoleUser, admin: true, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t)
			path := "/api/v1/me/runners/" + f.rn.ID.String() + tc.query
			if tc.admin {
				path = "/api/v1/runners/" + f.rn.ID.String()
			}
			var code int
			if tc.bearer != "" {
				code = do(t, f.srv, http.MethodDelete, path, tc.bearer, "").Code
			} else {
				code = doSSO(t, f.srv, http.MethodDelete, path, memberModeSSOSession(t, tc.sub, tc.sub+"@example.com", tc.role, false), "").Code
			}
			if code != tc.want {
				t.Fatalf("DELETE %s = %d, want %d", path, code, tc.want)
			}
			f.unrevoked(t)
		})
	}
}

func TestRunnerRevokePG_OwnerNotMineAndSecurityAdmin(t *testing.T) {
	f := newAdmissionFixture(t)
	owner := memberModeSSOSession(t, f.rn.Owner, "owner@example.com", oidc.RoleUser, false)
	path := "/api/v1/me/runners/" + f.rn.ID.String() + "?reason=not_mine"
	w := doSSO(t, f.srv, http.MethodDelete, path, owner, "")
	var body map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["runs_killed"] != float64(1) {
		t.Fatalf("owner revoke = %d %s", w.Code, w.Body.String())
	}
	got, _ := f.st.GetRunner(f.ctx, f.rn.ID)
	if got.State != types.RunnerRevoked || got.RevokedBy != f.rn.Owner || got.RevokeReason != types.RunnerRevokeNotMine {
		t.Fatalf("stored revoke = %s by %q for %q", got.State, got.RevokedBy, got.RevokeReason)
	}
	if !f.revokeAudited(t, f.rn.Owner, types.RunnerRevokeNotMine) {
		t.Fatal("runner.revoke does not carry the owner and not_mine")
	}
	if run, _ := f.st.GetRun(f.ctx, f.run.ID); run.State != types.RunKilled {
		t.Fatalf("run on the revoked runner is %s", run.State)
	}
	if w := doSSO(t, f.srv, http.MethodDelete, path, owner, ""); w.Code != http.StatusNotFound {
		t.Fatalf("a second revoke = %d", w.Code)
	}

	g := newAdmissionFixture(t)
	sec := memberModeSSOSession(t, "sec-admin", "sec@example.com", oidc.RoleSecurityAdmin, false)
	if w := doSSO(t, g.srv, http.MethodDelete, "/api/v1/runners/"+g.rn.ID.String(), sec, ""); w.Code != http.StatusOK {
		t.Fatalf("security admin revoke = %d %s", w.Code, w.Body.String())
	}
	got, _ = g.st.GetRunner(g.ctx, g.rn.ID)
	if got.State != types.RunnerRevoked || got.RevokedBy == "" || got.RevokedBy == g.rn.Owner || got.RevokeReason != "" {
		t.Fatalf("stored revoke = %s by %q for %q", got.State, got.RevokedBy, got.RevokeReason)
	}
	if !g.revokeAudited(t, got.RevokedBy, "") {
		t.Fatal("runner.revoke does not name the security admin")
	}
}

// The control frame to a peer inside the resume window is best effort: a session that already dropped
// must not turn a committed revoke into an error.
func TestRunnerRevokePG_ASessionThatAlreadyDroppedStillRevokes(t *testing.T) {
	f := newAdmissionFixture(t)
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	s.cancel()
	<-s.done // the runner's side is gone; the org hub still holds the session for the resume window
	sec := memberModeSSOSession(t, "sec-admin", "sec@example.com", oidc.RoleSecurityAdmin, false)
	w := doSSO(t, f.srv, http.MethodDelete, "/api/v1/runners/"+f.rn.ID.String(), sec, "")
	var body map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["state"] != string(types.RunnerRevoked) {
		t.Fatalf("revoke after the session dropped = %d %s, want 200 revoked", w.Code, w.Body.String())
	}
	if got, _ := f.st.GetRunner(f.ctx, f.rn.ID); got.State != types.RunnerRevoked {
		t.Fatalf("stored state = %s", got.State)
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ReservedPrincipalImpostorOwnsNothing: over the real run and token
// tables, a member-role impostor whose subject is the admin token — by a
// session cookie or by a wdn_ token row stored before the fix, exact or a case
// variant — neither reads a run the admin token created nor lists runs; the
// admin token itself still reads it.
func TestPG_ReservedPrincipalImpostorOwnsNothing(t *testing.T) {
	pool := throwawayPGPool(t)
	h := newHarness(t)
	pg := store.NewPG(pool)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	ctx := context.Background()

	run, err := pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: adminTokenPrincipal, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	path := "/api/v1/runs/" + run.ID.String()
	if w := do(t, srv, http.MethodGet, path, adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("control: the admin token reads its own run = %d, want 200", w.Code)
	}

	for _, sub := range []string{adminTokenPrincipal, "Admin-Token"} {
		raw := apiTokenPrefix + "impostor" + uuid.NewString()[:8]
		if _, err := pg.CreateAPIToken(ctx, types.APIToken{
			ID: uuid.New(), Principal: sub, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
			GroupsTruncated: new(bool), Name: "impostor",
		}, raw); err != nil {
			t.Fatalf("CreateAPIToken(%q): %v", sub, err)
		}
		for _, p := range []string{path, "/api/v1/runs"} {
			if w := do(t, srv, http.MethodGet, p, raw, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("token for %q: GET %s = %d, want 401; body=%s", sub, p, w.Code, w.Body.String())
			}
			if w := doSSO(t, srv, http.MethodGet, p, ssoSession(t, sub, "", oidc.RoleUser), ""); w.Code != http.StatusUnauthorized {
				t.Errorf("session for %q: GET %s = %d, want 401; body=%s", sub, p, w.Code, w.Body.String())
			}
		}
	}
}

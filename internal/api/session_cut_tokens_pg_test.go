// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// "Sign out everywhere" (sessions_only) ends browser sessions only. A request a token carries reads the
// person's credential cutoff, as apiTokenAuth does, and never the session-only cut.

func TestDelegation_SessionsOnlyLeavesDelegatedTokensWorking(t *testing.T) {
	e := newDelegationPG(t)
	e.h.srv.cfg.SessionRevocations = &pgTestRevocations{pool: e.pool, st: e.st}
	portal, cred := e.registerPortal(t, delegGroup)
	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("delegated /me before the sign-out: %d", w.Code)
	}
	time.Sleep(10 * time.Millisecond)
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/sessions/revoke", e.secAdmin, `{"sub":"`+delegPerson+`","sessions_only":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("sessions_only revoke: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("delegated /me after the sign-out: %d %s, want 200", w.Code, w.Body.String())
	}
}

func TestPG_GovernanceChanges_SessionsOnlyLeavesTheApproverTokenAbleToCommit(t *testing.T) {
	e := newGovEnv(t, func(c *Config) {
		pool := c.Store.(store.PG).Pool
		c.SessionRevocations = &pgTestRevocations{pool: pool, st: store.NewPG(pool)}
	})
	p := e.seedProfile("p", "pypi.org")
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/profiles/"+p.ID.String(), profileBody("p", "pypi.org", "github.com")))
	_, raw := e.mintToken(govSubBob, "bob@corp.example", oidc.RoleSecurityAdmin)
	time.Sleep(10 * time.Millisecond)
	if w := e.call(e.carol, http.MethodPost, "/api/v1/sessions/revoke", `{"sub":"`+govSubBob+`","sessions_only":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("sessions_only revoke: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.srv, http.MethodPost, approvePath(ch.ID), raw, ""); w.Code != http.StatusOK {
		t.Fatalf("approval by the token after the sign-out = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.profile(p.ID); len(got.Ceiling.AllowedDomains) != 2 {
		t.Errorf("the approved change did not apply: %v", got.Ceiling.AllowedDomains)
	}
}

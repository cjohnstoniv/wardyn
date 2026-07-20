// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── FIX #10: X-Wardyn-Principal cannot forge human attribution ───────────────
//
// principalFromRequest/actorFromRequest resolve the audit actor for admin-gated
// actions (decided_by on approvals, sub/sponsor + run.create/run.kill actor).
// The X-Wardyn-Principal header is a DEV-ONLY override (docs/sdk.md). If it were
// honored for a plain admin-token caller, any WARDYN_ADMIN_TOKEN bearer could
// record that a named human ("alice@example.com") approved a credential / created
// a run when no human acted — breaking invariant 4 (per-run identity) and the
// non-repudiation intent of invariant 6.

// TestPrincipalHeaderNotTrustedForAdminToken is the FIX #10 regression: a
// non-local admin-token request carrying X-Wardyn-Principal must NOT be attributed
// to that header value. The resolved actor is the non-human "admin-token" (system),
// never the forged human.
func TestPrincipalHeaderNotTrustedForAdminToken(t *testing.T) {
	const forged = "alice@example.com"
	// No LocalMode operator on the context and no OIDC session: exactly what an
	// automated caller holding ONLY the shared admin bearer token looks like.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil)
	r.Header.Set("X-Wardyn-Principal", forged)

	typ, name := actorFromRequest(r)
	if name == forged {
		t.Fatalf("actor name = %q; X-Wardyn-Principal forged a human under admin token (FIX #10 regressed)", name)
	}
	if typ != types.ActorSystem {
		t.Errorf("actor_type = %q, want %q (a token action must not be labeled human)", typ, types.ActorSystem)
	}
	if name != adminTokenPrincipal {
		t.Errorf("actor name = %q, want %q", name, adminTokenPrincipal)
	}
	// principalFromRequest is the name half; it must also refuse the forgery.
	if got := principalFromRequest(r); got == forged {
		t.Fatalf("principalFromRequest = %q; header must be ignored for admin token", got)
	}
}

// TestOIDCSessionPrincipalWinsOverHeader is the control: a verified OIDC human
// is attributed as the real human and the X-Wardyn-Principal header is ignored
// (a real identity already won). withOIDCHuman models what humanOrAdminAuth
// publishes after oidc.Middleware validates the session cookie.
func TestOIDCSessionPrincipalWinsOverHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil)
	r.Header.Set("X-Wardyn-Principal", "attacker@evil.example")
	r = r.WithContext(withOIDCHuman(r.Context(), "sub-alice@corp.example"))

	typ, name := actorFromRequest(r)
	if typ != types.ActorHuman {
		t.Errorf("actor_type = %q, want %q (verified SSO human)", typ, types.ActorHuman)
	}
	if name != "sub-alice@corp.example" {
		t.Fatalf("actor name = %q, want the OIDC sub (header must not override a verified session)", name)
	}
}

// TestLocalModeHonorsOperatorAndDevHeader is the control for the trusted
// single-dev machine: with no header the configured operator is used; the
// DEV-ONLY X-Wardyn-Principal override is honored ONLY here (LocalMode).
func TestLocalModeHonorsOperatorAndDevHeader(t *testing.T) {
	// (a) LocalMode, no header -> the configured operator, attributed human.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r = r.WithContext(withLocalPrincipal(r.Context(), "local:alice"))
	if typ, name := actorFromRequest(r); typ != types.ActorHuman || name != "local:alice" {
		t.Fatalf("LocalMode operator = (%q,%q), want (human, local:alice)", typ, name)
	}

	// (b) LocalMode + X-Wardyn-Principal -> dev override honored (trusted machine).
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r2 = r2.WithContext(withLocalPrincipal(r2.Context(), "local:alice"))
	r2.Header.Set("X-Wardyn-Principal", "dev@example.com")
	if typ, name := actorFromRequest(r2); typ != types.ActorHuman || name != "dev@example.com" {
		t.Fatalf("LocalMode dev override = (%q,%q), want (human, dev@example.com)", typ, name)
	}
}

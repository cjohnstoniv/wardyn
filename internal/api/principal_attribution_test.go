// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestLocalPrincipalOverrideIsBoundedAndClean is F339.
//
// In LOCAL MODE the X-Wardyn-Principal header IS honoured — the machine
// authenticates nobody, so the caller saying who they are is the only
// attribution there is. It was taken verbatim: no trim, no length cap, no
// control-character guard, while every other caller-supplied string that
// reaches a row in this package gets exactly that pair (access.go's
// canonicalRoleMapValue, apitokens.go's token name). The value becomes the
// ACTOR of append-only audit rows, so a 4096-byte header wrote a 4096-byte
// actor, and \n / \x00 / JSON metacharacters went into the one field a later
// investigation reads first, byte for byte.
//
// A rejected header falls back to the configured local operator rather than
// failing the request: this is attribution, not authorization, and nothing
// about the caller's reach turns on it.
func TestLocalPrincipalOverrideIsBoundedAndClean(t *testing.T) {
	const operator = "local:alice"
	local := func(t *testing.T, header string) (types.ActorType, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil)
		if header != "" {
			r.Header.Set("X-Wardyn-Principal", header)
		}
		r = r.WithContext(withLocalPrincipal(r.Context(), operator))
		return actorFromRequest(r)
	}

	t.Run("an ordinary override is still honoured", func(t *testing.T) {
		typ, name := local(t, "alice@example.com")
		if typ != types.ActorHuman || name != "alice@example.com" {
			t.Errorf("actor = %s/%q, want human/alice@example.com — the dev override is the point of local mode", typ, name)
		}
	})

	t.Run("it is trimmed", func(t *testing.T) {
		if _, name := local(t, "  alice@example.com \t"); name != "alice@example.com" {
			t.Errorf("actor name = %q, want the trimmed value — a padded header must not write a padded actor", name)
		}
	})

	for _, tc := range []struct {
		name, header string
	}{
		{"a control character", "alice@example.com\n\"actor\":\"admin\""},
		{"a NUL", "alice@example.com\x00"},
		{"a carriage return", "alice\r\nX-Forged: 1"},
		{"over the field cap", strings.Repeat("a", maxCapabilityGrantFieldLen+1)},
		{"whitespace only", "   \t "},
	} {
		t.Run(tc.name+" falls back to the configured operator", func(t *testing.T) {
			typ, name := local(t, tc.header)
			if name != operator {
				t.Errorf("actor name = %q (len %d), want %q — a header this package would refuse from any other "+
					"caller must not become the actor of an append-only row", name, len(name), operator)
			}
			if typ != types.ActorHuman {
				t.Errorf("actor_type = %q, want %q — falling back changes WHO, never WHAT KIND", typ, types.ActorHuman)
			}
		})
	}

	t.Run("at the cap exactly is accepted", func(t *testing.T) {
		at := strings.Repeat("a", maxCapabilityGrantFieldLen)
		if _, name := local(t, at); name != at {
			t.Errorf("a header of exactly the cap was refused — the boundary belongs on the inside")
		}
	})

	// FIX #10 is untouched by any of this: outside local mode the header is
	// ignored however clean it is.
	t.Run("a clean header still forges nothing off local mode", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil)
		r.Header.Set("X-Wardyn-Principal", "alice@example.com")
		if typ, name := actorFromRequest(r); typ != types.ActorSystem || name != adminTokenPrincipal {
			t.Errorf("actor = %s/%q, want %s/%q (FIX #10)", typ, name, types.ActorSystem, adminTokenPrincipal)
		}
	})
}

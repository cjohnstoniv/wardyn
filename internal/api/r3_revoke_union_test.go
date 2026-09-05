// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRevokeSessionsUnionsPrincipalAndEmail is F214.
//
// The email sweep ran only when the principal lookup came back EMPTY, so the two
// halves of one revoke disagreed about who was named. On any IdP where sub and
// email differ — Entra, where sub is an opaque per-app identifier — a human's
// tokens routinely straddle both forms: one row minted under the email, one
// under the sub. A revoke naming the email matched the literal row, skipped the
// email arm because it had found something, and left the human's other rows
// live. It answered 204 and wrote an audit row with a NON-ZERO tokens_revoked,
// so the documented detection heuristic — "a zero there is the signal" — never
// fired for the exact case it exists to catch.
//
// The session cutoff has always matched sub OR email (IsSessionRevoked); this
// pins the token half agreeing with it.
func TestRevokeSessionsUnionsPrincipalAndEmail(t *testing.T) {
	const email = "alice@corp.example"

	t.Run("a revoke naming one identity sweeps both of that human's rows", func(t *testing.T) {
		literal := types.APIToken{ID: uuid.New(), Principal: email, Email: email, CreatedAt: time.Now().UTC()}
		opaque := types.APIToken{ID: uuid.New(), Principal: "sub-alice-opaque", Email: email, CreatedAt: time.Now().UTC()}
		srv, _, st := sessionsTestServerWithTokens(t, []types.APIToken{literal, opaque})

		w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke",
			ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
			`{"sub":"`+email+`"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		if len(st.revoked) != 2 {
			t.Fatalf("revoked %d rows %v, want both of this human's tokens (%s literal, %s opaque-sub) — "+
				"the principal lookup finding one row must not suppress the email sweep, or the human keeps a live credential",
				len(st.revoked), st.revoked, literal.ID, opaque.ID)
		}
	})

	// The gate ALSO mis-fired on revoked rows: ListAPITokensByPrincipal is
	// unfiltered, so a target whose only literal row was already revoked
	// returned a non-empty list and suppressed the email arm entirely.
	t.Run("an already-revoked literal row does not suppress the sweep", func(t *testing.T) {
		revokedAt := time.Now().UTC().Add(-time.Hour)
		dead := types.APIToken{ID: uuid.New(), Principal: email, Email: email, RevokedAt: &revokedAt, CreatedAt: revokedAt}
		live := types.APIToken{ID: uuid.New(), Principal: "sub-alice-opaque", Email: email, CreatedAt: time.Now().UTC()}
		srv, _, st := sessionsTestServerWithTokens(t, []types.APIToken{dead, live})

		w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke",
			ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
			`{"sub":"`+email+`"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		if len(st.revoked) != 1 || st.revoked[0] != live.ID {
			t.Fatalf("revoked %v, want exactly the live opaque-sub row %s — an already-revoked literal row "+
				"made the lookup non-empty and switched the email sweep off", st.revoked, live.ID)
		}
	})

	// A row matched by BOTH arms must be revoked once, not twice: the audit
	// row's tokens_revoked is a count an operator reads.
	t.Run("a row matched by both arms is counted once", func(t *testing.T) {
		both := types.APIToken{ID: uuid.New(), Principal: email, Email: email, CreatedAt: time.Now().UTC()}
		srv, _, st := sessionsTestServerWithTokens(t, []types.APIToken{both})

		w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke",
			ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
			`{"sub":"`+email+`"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		if len(st.revoked) != 1 {
			t.Fatalf("revoked %v, want the single row exactly once — the union must de-duplicate by token id "+
				"or the audit row over-reports", st.revoked)
		}
	})

	// The control that keeps the union honest: a DIFFERENT human is untouched.
	t.Run("another human's rows are untouched", func(t *testing.T) {
		mine := types.APIToken{ID: uuid.New(), Principal: "sub-alice-opaque", Email: email, CreatedAt: time.Now().UTC()}
		bob := types.APIToken{ID: uuid.New(), Principal: "sub-bob", Email: "bob@corp.example", CreatedAt: time.Now().UTC()}
		srv, _, st := sessionsTestServerWithTokens(t, []types.APIToken{mine, bob})

		doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke",
			ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
			`{"sub":"`+email+`"}`)
		if len(st.revoked) != 1 || st.revoked[0] != mine.ID {
			t.Fatalf("revoked %v, want only %s — a union must not widen to every principal", st.revoked, mine.ID)
		}
	})
}

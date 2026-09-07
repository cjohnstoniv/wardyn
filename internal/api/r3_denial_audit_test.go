// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// denialRows returns the authz.denied events recorded so far.
func denialRows(events []types.AuditEvent) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range events {
		if ev.Action == "authz.denied" {
			out = append(out, ev)
		}
	}
	return out
}

// TestInHandlerMemberDenialsAreAudited is F343.
//
// docs/AUDIT-ACTIONS.md states the contract as "every member denial that isn't a
// plain foreign-resource 404", and the MIDDLEWARE honours it — the same member
// refused by an admin-only route leaves authz.denied/admin_surface. Two gates
// that live INSIDE a handler wrote their 403 and returned, so whether a member's
// attempt was recorded depended on where the refusal happened to be
// implemented: reaching for another human's credential namespace (?owner=) and
// reaching for a permanent workspace allowlist entry through the approval queue
// (decision_scope always) both left nothing at all.
func TestInHandlerMemberDenialsAreAudited(t *testing.T) {
	member := func(t *testing.T) *http.Cookie {
		t.Helper()
		return ssoSession(t, "sub-mallory", "mallory@corp.example", oidc.RoleMember)
	}

	t.Run("a member naming another namespace with ?owner=", func(t *testing.T) {
		for _, tc := range []struct{ name, method, path, body string }{
			{"GET list", http.MethodGet, "/api/v1/secrets?owner=sub-bob", ""},
			{"PUT write", http.MethodPut, "/api/v1/secrets/anthropic-api-key?owner=sub-bob", `{"value":"sk-x"}`},
			{"DELETE", http.MethodDelete, "/api/v1/secrets/anthropic-api-key?owner=sub-bob", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h, srv := secretsRBACServer(t, &memSecrets{m: map[string][]byte{}})
				w := doSSO(t, srv, tc.method, tc.path, member(t), tc.body)
				if w.Code != http.StatusForbidden {
					t.Fatalf("%s %s = %d, want 403; body=%s", tc.method, tc.path, w.Code, w.Body.String())
				}
				rows := denialRows(h.audit.events)
				if len(rows) != 1 {
					t.Fatalf("authz.denied rows = %d, want 1 — a member reaching for another human's credential "+
						"namespace is exactly the attempt this row exists to leave behind", len(rows))
				}
				if got, _ := auditDataField(t, rows[0], "reason"); got != "admin_surface" {
					t.Errorf("reason = %q, want admin_surface (the same reason the middleware writes for this member "+
						"on an admin-only route)", got)
				}
				if got, _ := auditDataField(t, rows[0], "method"); got != tc.method {
					t.Errorf("method = %q, want %q — the row is shape-identical to the middleware's", got, tc.method)
				}
				if rows[0].Outcome != "denied" {
					t.Errorf("outcome = %q, want denied", rows[0].Outcome)
				}
				// It names no namespace: the refusal runs before any lookup, so
				// neither the response nor the row may say whether ?owner=
				// resolves to a real principal.
				if v, ok := auditDataField(t, rows[0], "owner"); ok {
					t.Errorf("the row echoed the requested owner %q — a constant refusal that logs the value is an "+
						"existence oracle with extra steps", v)
				}
			})
		}
	})

	// THE CONTROL that makes the arm above mean something: an ADMIN using
	// ?owner= is not a denial and writes no such row.
	t.Run("an operator using ?owner= is not a denial", func(t *testing.T) {
		h, srv := secretsRBACServer(t, &memSecrets{m: map[string][]byte{}})
		w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets?owner=sub-bob",
			ssoSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin), "")
		if w.Code == http.StatusForbidden {
			t.Fatalf("an operator was refused ?owner= (%d); body=%s", w.Code, w.Body.String())
		}
		if rows := denialRows(h.audit.events); len(rows) != 0 {
			t.Errorf("authz.denied rows = %d for an operator's own admin surface, want 0", len(rows))
		}
	})

	// THE SECOND SILENT GATE, in another handler: `always` is operator-only
	// (rule 6) because it writes a permanent workspace allowlist entry, which is
	// the back door around the operatorOnly PUT /workspaces/{id}/approved-egress.
	// The refusal is a 403 and left no row, while its two neighbours on the same
	// path (the capability refusal and the four-eyes one) both write authz.denied.
	t.Run("a member reaching for decision_scope always", func(t *testing.T) {
		f := newScopeFixture(t)
		id := f.seedEgress(t, "registry.npmjs.org")
		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
			ssoSession(t, f.memberID, "member@corp.example", oidc.RoleMember),
			decideBody(t, types.ScopeAlways, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("member picking always = %d, want 403; body=%s", w.Code, w.Body.String())
		}
		rows := denialRows(f.rec.events)
		if len(rows) != 1 {
			t.Fatalf("authz.denied rows = %d, want 1 — a member reaching for a permanent workspace allowlist entry "+
				"through the approval queue is the attempt rule 6 exists to refuse, and a refusal nobody records "+
				"is a door nobody can prove was tried", len(rows))
		}
		if got, _ := auditDataField(t, rows[0], "reason"); got != "security_admin_surface" {
			t.Errorf("reason = %q, want security_admin_surface — the same reason requireSecurityOperator writes "+
				"for this predicate", got)
		}
		if rows[0].Target != id.String() {
			t.Errorf("target = %q, want the approval id %s — this file's other denials key on it", rows[0].Target, id)
		}
		if rows[0].RunID == nil || *rows[0].RunID != f.runID {
			t.Errorf("run_id = %v, want %s — the row sits beside the run's own approval trail", rows[0].RunID, f.runID)
		}
	})
}

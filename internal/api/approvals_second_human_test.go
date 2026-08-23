// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// audit returns what the fixture's server recorded so far.
func (f *scopeFixture) audit() []types.AuditEvent { return f.rec.events }

// seedCredential is seedEgress's other-kind twin: a PENDING CREDENTIAL approval
// on the same run, for asserting what the egress-only rules do NOT touch.
func (f *scopeFixture) seedCredential(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalCredential,
		RequestedScope: json.RawMessage(`{"host":"dev.azure.com","secret_name":"ado-pat"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()
	return id
}

// TestSecondHuman_OffByDefault is the compatibility half: with the switch unset,
// the run's own creator decides their own egress approval exactly as before.
func TestSecondHuman_OffByDefault(t *testing.T) {
	f := newScopeFixture(t)
	creator := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
	id := f.seedEgress(t, "registry.npmjs.org")

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", creator, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("switch off, creator decides: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestSecondHuman_RefusesTheRunsCreator is the switch doing its job: the human
// who created the run cannot be the human who approves its egress. Asserted for
// BOTH verbs — a self-DENY is not a safe direction to leave open, since a deny
// is how an operator closes an approval they would rather nobody saw.
func TestSecondHuman_RefusesTheRunsCreator(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv(envEgressSecondHuman, "1")
			f := newScopeFixture(t)
			creator := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
			id := f.seedEgress(t, "registry.npmjs.org")

			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, creator, `{}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("creator self-%s: status = %d, want 403; body=%s", verb, w.Code, w.Body.String())
			}
			// PENDING -> decided is one-way, so the refusal must land BEFORE
			// Decide(): a 403 over an already-decided approval would be a gate
			// nobody can take back.
			f.approval.mu.Lock()
			state := f.approval.byID[id].State
			f.approval.mu.Unlock()
			if state != types.ApprovalPending {
				t.Fatalf("approval state = %q after a refused decision, want PENDING", state)
			}
		})
	}
}

// TestSecondHuman_AdmitsADifferentHuman: the gate wants a SECOND human, not no
// human. Another admin — who did not create the run — decides normally.
func TestSecondHuman_AdmitsADifferentHuman(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "1")
	f := newScopeFixture(t)
	other := ssoSession(t, "sub-someone-else", "reviewer@corp.example", oidc.RoleAdmin)
	id := f.seedEgress(t, "registry.npmjs.org")

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", other, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second human approves: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestSecondHuman_AdminTokenBreakGlass pins the documented BYPASS, so the
// exemption is a tested property rather than an accident someone later "fixes"
// into a lockout. A bare admin-token caller is attributed system/admin-token
// precisely because a shared token carries no per-human identity — there is no
// second human to compare it against — so it passes, and the bypass is recorded
// as approval.second_human.bypass rather than left silent.
func TestSecondHuman_AdminTokenBreakGlass(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "1")
	f := newScopeFixture(t)
	id := f.seedEgress(t, "registry.npmjs.org")

	w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", adminToken, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin-token break-glass: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	found := false
	for _, ev := range f.audit() {
		if ev.Action == "approval.second_human.bypass" {
			found = true
			if ev.ActorType != types.ActorSystem || ev.Actor != adminTokenPrincipal {
				t.Errorf("bypass event actor = %s/%s, want %s/%s",
					ev.ActorType, ev.Actor, types.ActorSystem, adminTokenPrincipal)
			}
		}
	}
	if !found {
		t.Fatal("admin-token bypass wrote no approval.second_human.bypass event — the break-glass must not be silent")
	}
}

// TestSecondHuman_CredentialApprovalUnaffected: the switch is scoped to EGRESS
// decisions. A credential approval — already admin-only regardless of ownership
// — is not narrowed further, so an operator who happens to have created the run
// can still release its credential.
func TestSecondHuman_CredentialApprovalUnaffected(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "1")
	f := newScopeFixture(t)
	creator := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
	id := f.seedCredential(t)

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", creator, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("credential approval under the egress switch: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

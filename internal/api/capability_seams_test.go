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
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Capability ENFORCEMENT — the four seams where a 0.6 grant actually changes
// what a member can do. The resolver's own precedence matrix lives in
// capabilities_test.go; this file only asks whether each seam consults it, at
// the right point in its own authorization order, and stays out of the way when
// nothing is granted and no switch is on (the whole upgrade story).
//
// Every fixture here wraps an EXISTING store double in capStore, so capability
// rows are the only thing that differs from the suite the seam already had.

// withCaps points srv's store at the double it already had, wrapped so the
// capability reads answer from grants/enf instead of the double's empty stubs.
func withCaps(srv *Server, inner store.Store, grants []types.CapabilityGrant, enf map[string]bool) {
	srv.cfg.Store = &capStore{Store: inner, grants: grants, enf: enf}
}

// auditReasons returns the "reason" detail of every recorded event with the
// given action, in order — the field every authz.denied consumer reads.
func auditReasons(t *testing.T, srv *Server, action string) []string {
	t.Helper()
	rec, ok := srv.cfg.Audit.(*recRecorder)
	if !ok {
		t.Fatalf("audit recorder = %T, want *recRecorder", srv.cfg.Audit)
	}
	var out []string
	for _, ev := range rec.events {
		if ev.Action != action {
			continue
		}
		var d struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("unmarshal %s data: %v", action, err)
		}
		out = append(out, d.Reason)
	}
	return out
}

// ─── D1: deciding an egress approval ──────────────────────────────────────────

// approveAs POSTs the plain (scope-less) approve the console sends.
func approveAs(t *testing.T, srv *Server, sess *http.Cookie, id uuid.UUID) (int, string) {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", sess, `{"reason":"test"}`)
	return w.Code, w.Body.String()
}

// TestDecide_EgressHostCapability walks the gate on the ONE path it guards: a
// member deciding an egress_domain approval raised by a run they own.
func TestDecide_EgressHostCapability(t *testing.T) {
	const host = "registry.npmjs.org"
	allow, deny := types.CapabilityAllow, types.CapabilityDeny

	for _, tc := range []struct {
		name   string
		grants []types.CapabilityGrant
		enf    map[string]bool
		want   int
	}{
		{
			name: "no grants and no switch: decided, exactly as 0.5 did",
			want: http.StatusOK,
		},
		{
			name: "switch on, nothing granted: refused",
			enf:  map[string]bool{capEgressHost: true},
			want: http.StatusForbidden,
		},
		{
			name:   "switch on, the host granted to this member: decided",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, "sub-member-scope", capEgressHost, host, allow)},
			enf:    map[string]bool{capEgressHost: true},
			want:   http.StatusOK,
		},
		{
			name:   "switch on, a *.suffix grant covers it",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "*.npmjs.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			want:   http.StatusOK,
		},
		{
			name:   "switch on, a DIFFERENT host granted: still refused",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "pypi.org", allow)},
			enf:    map[string]bool{capEgressHost: true},
			want:   http.StatusForbidden,
		},
		{
			// The adoption on-ramp: a deny reaches a member with the switch
			// still off, so an admin can blacklist one host without taking the
			// whole deployment fail-closed.
			name:   "a deny bites with the switch OFF",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, host, deny)},
			want:   http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			withCaps(f.srv, f.store, tc.grants, tc.enf)
			member := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleMember)
			id := f.seedEgress(t, host)

			code, body := approveAs(t, f.srv, member, id)
			if code != tc.want {
				t.Fatalf("status = %d, want %d: %s", code, tc.want, body)
			}

			// A refusal must leave the approval re-decidable: PENDING->decided
			// is one-way, so a gate that fired after Decide() would already have
			// burned the request it then refused.
			wantState := types.ApprovalApproved
			if tc.want != http.StatusOK {
				wantState = types.ApprovalPending
			}
			f.approval.mu.Lock()
			state := f.approval.byID[id].State
			f.approval.mu.Unlock()
			if state != wantState {
				t.Fatalf("approval state = %q, want %q", state, wantState)
			}

			reasons := auditReasons(t, f.srv, "authz.denied")
			switch {
			case tc.want == http.StatusForbidden:
				if len(reasons) != 1 || reasons[0] != "capability_egress_host" {
					t.Fatalf("authz.denied reasons = %v, want [capability_egress_host]", reasons)
				}
			case len(reasons) != 0:
				t.Fatalf("authz.denied recorded on an allowed decision: %v", reasons)
			}
		})
	}
}

// TestDecide_EgressHostCapabilityRunsAfterOwnership pins the ORDER, which is
// the security property rather than a preference: the gate must never answer
// before kind and ownership have, or its 403 becomes an existence oracle for
// other people's approvals — exactly what the two byte-identical 404s above it
// exist to deny. Every case below runs under the strictest posture there is
// (enforced, with a `*` deny), so a gate that ran first would 403 all three.
func TestDecide_EgressHostCapabilityRunsAfterOwnership(t *testing.T) {
	f := newScopeFixture(t)
	withCaps(f.srv, f.store,
		[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, capWildcard, types.CapabilityDeny)},
		map[string]bool{capEgressHost: true})
	member := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleMember)

	// An egress approval on somebody else's run.
	foreignRun := uuid.New()
	f.store.mu.Lock()
	f.store.runs[foreignRun] = types.AgentRun{ID: foreignRun, CreatedBy: "someone-else", State: types.RunRunning}
	f.store.mu.Unlock()
	foreign := uuid.New()
	own := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[foreign] = types.ApprovalRequest{
		ID: foreign, RunID: foreignRun, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"registry.npmjs.org"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	// A CREDENTIAL approval on the member's OWN run: kind is checked first, so
	// this reads identically to the foreign one.
	f.approval.byID[own] = types.ApprovalRequest{
		ID: own, RunID: f.runID, Kind: types.ApprovalCredential,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()

	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"foreign egress approval", foreign},
		{"own credential approval", own},
		{"no such approval", uuid.New()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := approveAs(t, f.srv, member, tc.id)
			if code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (a 403 here confirms the row exists): %s", code, body)
			}
		})
	}

	if reasons := auditReasons(t, f.srv, "authz.denied"); len(reasons) != 1 || reasons[0] != "not_owner" {
		t.Fatalf("authz.denied reasons = %v, want exactly [not_owner] — no capability reason may leak here", reasons)
	}
}

// TestDecide_EgressHostCapabilityExemptsOperators: an admin decides a host no
// grant covers, under a `*` deny with the switch on. The tier that writes the
// grants is not bounded by them.
func TestDecide_EgressHostCapabilityExemptsOperators(t *testing.T) {
	f := newScopeFixture(t)
	withCaps(f.srv, f.store,
		[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, capWildcard, types.CapabilityDeny)},
		map[string]bool{capEgressHost: true})
	admin := ssoSession(t, "sub-admin-cap", "admin@corp.example", oidc.RoleAdmin)

	if code, body := approveAs(t, f.srv, admin, f.seedEgress(t, "registry.npmjs.org")); code != http.StatusOK {
		t.Fatalf("admin decide: status = %d, want 200: %s", code, body)
	}
}

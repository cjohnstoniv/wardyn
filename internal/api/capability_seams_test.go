// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// ─── D2: a member's own inline policy ─────────────────────────────────────────

// capPolicyServer is a member-facing create-run resolution: the secrets harness
// (so an api_key grant's secret actually exists) with capability rows behind it.
// Nothing here goes through the router — resolveRunPolicy is called directly,
// the same way TestFilterMemberGrants calls the gate it covers, because the
// narrowing happens entirely inside it and the daemonless harness cannot get a
// run past the confinement gate anyway.
func capPolicyServer(t *testing.T, grants []types.CapabilityGrant, enf map[string]bool) *harness {
	t.Helper()
	h, _ := newSecretsHarness(t)
	h.srv.cfg.Store = &capStore{grants: grants, enf: enf}
	return h
}

// memberRequest is an inbound POST /runs carrying a MEMBER's verified identity
// — the subject a user grant is written against.
func memberRequest(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	return r.WithContext(withOIDCGroups(operatorCtx(capSub, capEmail, oidc.RoleMember), nil))
}

// resolveInline runs the member's inline policy through the real chokepoint and
// returns the resolved spec plus the warnings Review would show.
func resolveInline(t *testing.T, h *harness, spec types.RunPolicySpec) (types.RunPolicySpec, []string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := memberRequest(t)
	req := createRunRequest{Agent: "claude-code", InlinePolicy: &spec}
	got, _, warns, ok := h.srv.resolveRunPolicy(r.Context(), w, r, &req, false)
	if !ok {
		t.Fatalf("resolveRunPolicy refused: %d %s", w.Code, w.Body.String())
	}
	return got, warns
}

// dropWarns keeps only the drop notices, discarding composer.Clamp's own
// unrelated ceiling notes (resources, confinement, …) that every member
// resolution carries.
func dropWarns(warns []string) []string {
	var out []string
	for _, w := range warns {
		if strings.HasPrefix(w, "dropped ") {
			out = append(out, w)
		}
	}
	return out
}

func apiKeyGrantSpec(host, secret string) types.GrantSpec {
	return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": host, "secret_name": secret})}
}

// TestInlinePolicy_EgressHostNarrowing: a member may author only the hosts they
// hold, and only their OWN authored list is touched.
func TestInlinePolicy_EgressHostNarrowing(t *testing.T) {
	ceiling := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"pypi.org", "registry.npmjs.org"},
	}

	t.Run("no grants and no switch: both hosts survive", func(t *testing.T) {
		h := capPolicyServer(t, nil, nil)
		h.srv.cfg.DefaultPolicy = ceiling
		got, warns := resolveInline(t, h, types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			AllowedDomains:      []string{"pypi.org", "registry.npmjs.org"},
		})
		if len(got.AllowedDomains) != 2 || len(dropWarns(warns)) != 0 {
			t.Fatalf("domains = %v, warns = %v; want both kept and nothing said", got.AllowedDomains, warns)
		}
	})

	t.Run("switch on: only the granted host survives, and Review says which went", func(t *testing.T) {
		h := capPolicyServer(t,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", types.CapabilityAllow)},
			map[string]bool{capEgressHost: true})
		h.srv.cfg.DefaultPolicy = ceiling
		got, warns := resolveInline(t, h, types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			AllowedDomains:      []string{"pypi.org", "registry.npmjs.org"},
		})
		if !slices.Equal(got.AllowedDomains, []string{"pypi.org"}) {
			t.Fatalf("domains = %v, want [pypi.org]", got.AllowedDomains)
		}
		if d := dropWarns(warns); len(d) != 1 || !strings.Contains(d[0], "registry.npmjs.org") {
			t.Fatalf("drop warnings = %v, want one naming the dropped host", d)
		}
		if reasons := auditReasons(t, h.srv, "authz.denied"); !slices.Equal(reasons, []string{"capability_egress_host"}) {
			t.Fatalf("authz.denied reasons = %v, want [capability_egress_host]", reasons)
		}
	})

	t.Run("the ADMIN's own ceiling is never narrowed", func(t *testing.T) {
		// The doctrine: a capability bounds what the MEMBER chose. The
		// stored/default path resolves an admin-authored spec, so a `*` deny on
		// this member leaves it exactly as authored.
		h := capPolicyServer(t,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, capWildcard, types.CapabilityDeny)},
			map[string]bool{capEgressHost: true})
		h.srv.cfg.DefaultPolicy = ceiling
		w := httptest.NewRecorder()
		r := memberRequest(t)
		req := createRunRequest{Agent: "claude-code"}
		got, _, _, ok := h.srv.resolveRunPolicy(r.Context(), w, r, &req, false)
		if !ok {
			t.Fatalf("resolveRunPolicy refused the default policy: %d %s", w.Code, w.Body.String())
		}
		if !slices.Equal(got.AllowedDomains, ceiling.AllowedDomains) {
			t.Fatalf("domains = %v, want the operator's own %v untouched", got.AllowedDomains, ceiling.AllowedDomains)
		}
	})
}

// TestInlinePolicy_SecretNarrowing: a stored-secret pairing must clear BOTH the
// operator's eligible list and this member's own secret grants.
func TestInlinePolicy_SecretNarrowing(t *testing.T) {
	// An operator ceiling that eligible-lists the EXACT pairing, so
	// filterMemberGrants keeps it and the capability is the only thing left.
	pairing := apiKeyGrantSpec("api.anthropic.com", "anthropic-api-key")
	ceiling := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants:      []types.GrantSpec{pairing},
	}
	authored := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants:      []types.GrantSpec{pairing},
	}

	t.Run("no grants and no switch: the operator-listed pairing survives", func(t *testing.T) {
		h := capPolicyServer(t, nil, nil)
		h.srv.cfg.DefaultPolicy = ceiling
		got, warns := resolveInline(t, h, authored)
		if len(got.EligibleGrants) != 1 || len(dropWarns(warns)) != 0 {
			t.Fatalf("grants = %d, warns = %v; want the pairing kept silently", len(got.EligibleGrants), warns)
		}
	})

	t.Run("switch on, secret ungranted: dropped even though the operator listed it", func(t *testing.T) {
		h := capPolicyServer(t, nil, map[string]bool{capSecret: true})
		h.srv.cfg.DefaultPolicy = ceiling
		got, warns := resolveInline(t, h, authored)
		if len(got.EligibleGrants) != 0 {
			t.Fatalf("grants = %d, want 0 — an operator-eligible pairing is not by itself a member's to use", len(got.EligibleGrants))
		}
		if d := dropWarns(warns); len(d) != 1 || !strings.Contains(d[0], "anthropic-api-key") {
			t.Fatalf("drop warnings = %v, want one naming the secret", d)
		}
		if reasons := auditReasons(t, h.srv, "authz.denied"); !slices.Equal(reasons, []string{"capability_secret"}) {
			t.Fatalf("authz.denied reasons = %v, want [capability_secret]", reasons)
		}
	})

	t.Run("switch on, secret granted: kept", func(t *testing.T) {
		h := capPolicyServer(t,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capSecret, "anthropic-api-key", types.CapabilityAllow)},
			map[string]bool{capSecret: true})
		h.srv.cfg.DefaultPolicy = ceiling
		got, warns := resolveInline(t, h, authored)
		if len(got.EligibleGrants) != 1 || len(dropWarns(warns)) != 0 {
			t.Fatalf("grants = %d, warns = %v; want the pairing kept", len(got.EligibleGrants), warns)
		}
	})
}

// TestInlinePolicy_DropsAreAggregatedPerReason: a spec that loses many things
// produces ONE event per reason, not one per thing, and the pre-existing
// ceiling drop (which used to be a warning and nothing else) is now among them.
func TestInlinePolicy_DropsAreAggregatedPerReason(t *testing.T) {
	h := capPolicyServer(t, nil, map[string]bool{capEgressHost: true, capSecret: true})
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"pypi.org", "registry.npmjs.org", "files.pythonhosted.org"},
		// Wildcard api_key ceiling: an arbitrary pairing is a ceiling drop
		// (grant_pairing_not_eligible), never reaching the secret capability.
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey}},
	}
	got, warns := resolveInline(t, h, types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"pypi.org", "registry.npmjs.org", "files.pythonhosted.org"},
		EligibleGrants:      []types.GrantSpec{apiKeyGrantSpec("pypi.org", "anthropic-api-key")},
	})
	if len(got.AllowedDomains) != 0 || len(got.EligibleGrants) != 0 {
		t.Fatalf("resolved spec = %+v, want everything member-authored dropped", got)
	}
	if d := dropWarns(warns); len(d) != 4 { // 3 hosts + 1 grant pairing
		t.Fatalf("drop warnings = %v, want one per dropped item (Review names each)", d)
	}
	reasons := auditReasons(t, h.srv, "authz.denied")
	if !slices.Equal(reasons, []string{"capability_egress_host", "grant_pairing_not_eligible"}) {
		t.Fatalf("authz.denied reasons = %v, want one event per reason, sorted", reasons)
	}
}

// TestInlinePolicy_PreflightDoesNotAudit: Review re-resolves on every edit, so a
// dry run warns without writing denials nobody's run ever hit — the same rule
// policy.inline already follows.
func TestInlinePolicy_PreflightDoesNotAudit(t *testing.T) {
	h := capPolicyServer(t, nil, map[string]bool{capEgressHost: true})
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"pypi.org"}}

	w := httptest.NewRecorder()
	r := memberRequest(t)
	req := createRunRequest{Agent: "claude-code", InlinePolicy: &types.RunPolicySpec{
		MinConfinementClass: types.CC2, AllowedDomains: []string{"pypi.org"},
	}}
	_, _, warns, ok := h.srv.resolveRunPolicy(r.Context(), w, r, &req, true)
	if !ok {
		t.Fatalf("preflight resolution refused: %d %s", w.Code, w.Body.String())
	}
	if d := dropWarns(warns); len(d) != 1 {
		t.Fatalf("drop warnings = %v, want the drop surfaced in Review", d)
	}
	if reasons := auditReasons(t, h.srv, "authz.denied"); len(reasons) != 0 {
		t.Fatalf("dry run wrote authz.denied events: %v", reasons)
	}
}

// ─── D3: the request fields a member does not freely choose ───────────────────

// denyRequest runs the request-level gate directly and returns whether it
// refused, plus the status it wrote. The HTTP wiring on both doors (create and
// preflight) is already pinned by TestBYOI_MemberDenied403; what varies here is
// the capability state, not the route.
func denyRequest(t *testing.T, srv *Server, req createRunRequest) (bool, int) {
	t.Helper()
	w := httptest.NewRecorder()
	denied := srv.denyMemberRequest(w, memberRequest(t), req)
	return denied, w.Code
}

// TestDenyMemberRequest_ImageWidens: 0.5 refused every member's image outright,
// so an unenforced kind must go on refusing — the mirror of every narrowing
// kind, and the reason capGranted is not capAllowed.
func TestDenyMemberRequest_ImageWidens(t *testing.T) {
	const ref = "ghcr.io/acme/agent:1.4.2"
	allowRef := grant(types.CapabilitySubjectUser, capSub, capImage, ref, types.CapabilityAllow)

	for _, tc := range []struct {
		name       string
		grants     []types.CapabilityGrant
		enf        map[string]bool
		req        createRunRequest
		wantDenied bool
	}{
		{
			name: "no grants and no switch: refused, exactly as 0.5 did",
			req:  createRunRequest{Image: ref}, wantDenied: true,
		},
		{
			name: "switch on but nothing granted: still refused",
			enf:  map[string]bool{capImage: true},
			req:  createRunRequest{Image: ref}, wantDenied: true,
		},
		{
			// The half that would be wrong if the widening kind reused
			// capAllowed: an unenforced kind reads as "allowed" there.
			name:   "granted but the switch is OFF: refused",
			grants: []types.CapabilityGrant{allowRef},
			req:    createRunRequest{Image: ref}, wantDenied: true,
		},
		{
			name:   "granted and enforced: the member may name it",
			grants: []types.CapabilityGrant{allowRef},
			enf:    map[string]bool{capImage: true},
			req:    createRunRequest{Image: ref}, wantDenied: false,
		},
		{
			name:   "a DIFFERENT ref granted: refused (exact refs, no near miss)",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capImage, "ghcr.io/acme/agent:1.4.1", types.CapabilityAllow)},
			enf:    map[string]bool{capImage: true},
			req:    createRunRequest{Image: ref}, wantDenied: true,
		},
		{
			name: "a deny beats the * grant",
			grants: []types.CapabilityGrant{
				grant(types.CapabilitySubjectAll, "", capImage, capWildcard, types.CapabilityAllow),
				grant(types.CapabilitySubjectUser, capSub, capImage, ref, types.CapabilityDeny),
			},
			enf: map[string]bool{capImage: true},
			req: createRunRequest{Image: ref}, wantDenied: true,
		},
		{
			// devcontainer_repo is not a capability kind: it executes
			// attacker-authored build config, so no row makes it nameable.
			name:   "devcontainer_repo stays refused under a * image grant",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, capWildcard, types.CapabilityAllow)},
			enf:    map[string]bool{capImage: true},
			req:    createRunRequest{DevcontainerRepo: "org/repo"}, wantDenied: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.Store = &capStore{grants: tc.grants, enf: tc.enf}
			denied, code := denyRequest(t, h.srv, tc.req)
			if denied != tc.wantDenied {
				t.Fatalf("denied = %v, want %v (status %d)", denied, tc.wantDenied, code)
			}
			if tc.wantDenied {
				if code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", code)
				}
				if reasons := auditReasons(t, h.srv, "authz.denied"); !slices.Equal(reasons, []string{"byoi_member"}) {
					t.Fatalf("authz.denied reasons = %v, want [byoi_member] (the reason OPERATIONS already documents)", reasons)
				}
			}
		})
	}
}

// TestDenyMemberRequest_WorkspaceNarrows: launching against an onboarded
// workspace is something every member could already do, so it stays allowed
// until an admin enforces the kind.
func TestDenyMemberRequest_WorkspaceNarrows(t *testing.T) {
	ws := uuid.New()
	req := createRunRequest{Agent: "claude-code", WorkspaceID: &ws}

	for _, tc := range []struct {
		name       string
		grants     []types.CapabilityGrant
		enf        map[string]bool
		wantDenied bool
	}{
		{name: "no grants and no switch: launched, exactly as 0.5 did"},
		{
			name: "switch on, nothing granted: refused",
			enf:  map[string]bool{capWorkspace: true}, wantDenied: true,
		},
		{
			name:   "switch on, this workspace granted: launched",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capWorkspace, ws.String(), types.CapabilityAllow)},
			enf:    map[string]bool{capWorkspace: true},
		},
		{
			name:   "switch on, a different workspace granted: refused",
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capWorkspace, uuid.New().String(), types.CapabilityAllow)},
			enf:    map[string]bool{capWorkspace: true}, wantDenied: true,
		},
		{
			name:       "a deny bites with the switch off",
			grants:     []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capWorkspace, ws.String(), types.CapabilityDeny)},
			wantDenied: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.Store = &capStore{grants: tc.grants, enf: tc.enf}
			denied, code := denyRequest(t, h.srv, req)
			if denied != tc.wantDenied {
				t.Fatalf("denied = %v, want %v (status %d)", denied, tc.wantDenied, code)
			}
			if !tc.wantDenied {
				return
			}
			if code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", code)
			}
			if reasons := auditReasons(t, h.srv, "authz.denied"); !slices.Equal(reasons, []string{"capability_workspace"}) {
				t.Fatalf("authz.denied reasons = %v, want [capability_workspace]", reasons)
			}
		})
	}
}

// TestDenyMemberRequest_WorkspaceRefusedOnBothDoors: preflight must refuse what
// launch would, or Review previews a checklist for a run that cannot start.
func TestDenyMemberRequest_WorkspaceRefusedOnBothDoors(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Store = &capStore{Store: newAuthzStore(), enf: map[string]bool{capWorkspace: true}}
	h.srv.router = h.srv.routes()
	member := ssoSession(t, capSub, capEmail, oidc.RoleMember)
	body := `{"agent":"claude-code","workspace_id":"` + uuid.New().String() + `"}`

	for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		t.Run(path, func(t *testing.T) {
			w := doSSO(t, h.srv, http.MethodPost, path, member, body)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestDenyMemberRequest_OperatorsAreExempt: the tier that writes the grants
// launches whatever it likes, and pays no store read to find out.
func TestDenyMemberRequest_OperatorsAreExempt(t *testing.T) {
	ws := uuid.New()
	h := newHarness(t)
	// A store that ERRORS on every capability read: an operator must never
	// reach it, so a 500 here would prove the exemption is not first.
	h.srv.cfg.Store = &capStore{err: errors.New("boom"), enf: map[string]bool{capWorkspace: true, capImage: true}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).
		WithContext(withOIDCGroups(operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin), nil))
	if denied := h.srv.denyMemberRequest(w, r, createRunRequest{Image: "ghcr.io/acme/agent:1", WorkspaceID: &ws}); denied {
		t.Fatalf("admin denied: %d %s", w.Code, w.Body.String())
	}
}

// ─── D4: what a member sees on the secrets list ───────────────────────────────

// listSecretNames reads GET /secrets as the given session.
func listSecretNames(t *testing.T, srv *Server, sess *http.Cookie) []string {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets", sess, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /secrets = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Names []string `json:"names"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode names: %v", err)
	}
	return body.Names
}

// TestListSecrets_MemberNarrowing: the listing is the reading half of the same
// grant that bounds an inline policy, so the picker cannot offer a name the
// launch gate would drop.
func TestListSecrets_MemberNarrowing(t *testing.T) {
	newSrv := func(t *testing.T, grants []types.CapabilityGrant, enf map[string]bool) *Server {
		t.Helper()
		h := newHarness(t)
		h.srv.cfg.OIDC = &oidc.Authenticator{}
		h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{
			"anthropic-api-key": []byte("sk-ant"),
			"prod-db-password":  []byte("hunter2"),
		}}
		h.srv.cfg.Store = &capStore{grants: grants, enf: enf}
		h.srv.router = h.srv.routes()
		return h.srv
	}
	member := ssoSession(t, capSub, capEmail, oidc.RoleMember)
	admin := ssoSession(t, "sub-admin-secrets", "admin@corp.example", oidc.RoleAdmin)

	t.Run("no grants and no switch: the whole list, exactly as 0.5 returned it", func(t *testing.T) {
		got := listSecretNames(t, newSrv(t, nil, nil), member)
		slices.Sort(got)
		if !slices.Equal(got, []string{"anthropic-api-key", "prod-db-password"}) {
			t.Fatalf("names = %v, want both", got)
		}
	})

	t.Run("switch on: only what the member holds", func(t *testing.T) {
		srv := newSrv(t,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capSecret, "anthropic-api-key", types.CapabilityAllow)},
			map[string]bool{capSecret: true})
		if got := listSecretNames(t, srv, member); !slices.Equal(got, []string{"anthropic-api-key"}) {
			t.Fatalf("names = %v, want only the granted one", got)
		}
		// The admin's own listing is untouched: they are the tier that writes
		// the grants, and a secrets page that hid rows from them would be
		// unusable.
		adminNames := listSecretNames(t, srv, admin)
		slices.Sort(adminNames)
		if !slices.Equal(adminNames, []string{"anthropic-api-key", "prod-db-password"}) {
			t.Fatalf("admin names = %v, want the full list", adminNames)
		}
	})

	t.Run("switch on with nothing granted: an empty list, never null", func(t *testing.T) {
		if got := listSecretNames(t, newSrv(t, nil, map[string]bool{capSecret: true}), member); len(got) != 0 {
			t.Fatalf("names = %v, want none", got)
		}
	})

	t.Run("a deny hides one name with the switch off", func(t *testing.T) {
		srv := newSrv(t,
			[]types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capSecret, "prod-db-password", types.CapabilityDeny)},
			nil)
		if got := listSecretNames(t, srv, member); !slices.Equal(got, []string{"anthropic-api-key"}) {
			t.Fatalf("names = %v, want the denied one hidden and the rest kept", got)
		}
	})
}

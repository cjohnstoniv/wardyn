// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The no-Pool newHarness panics inside store.CreateRun when a create-run request
// gets PAST all validation (the harness has no Pool). The chi Recoverer turns
// that panic into a 500. So for these inline-policy tests:
//   - a request REJECTED by validation returns its 4xx (400/422) and never
//     reaches the store;
//   - a request ACCEPTED past the validation boundary returns 500 (the recovered
//     nil-Pool panic), proving validation let it through.
// Status 500 is the "accepted past validation" sentinel in the no-Pool harness.

// TestCreateRun_InlineAndPolicyIDBothSet asserts the XOR: supplying both
// inline_policy and policy_id is a 400 before any store write.
func TestCreateRun_InlineAndPolicyIDBothSet(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets",` +
		`"policy_id":"11111111-1111-1111-1111-111111111111",` +
		`"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("both set: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_InlineEmptySpecRejected asserts an empty/invalid inline spec
// (missing min_confinement_class) is a 400 (validatePolicySpec fails closed).
func TestCreateRun_InlineEmptySpecRejected(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty inline spec: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_InlineUnknownConfinementRejected asserts an unknown
// min_confinement_class in the inline spec is a 400 (validatePolicySpec).
func TestCreateRun_InlineUnknownConfinementRejected(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{"min_confinement_class":"CC9"}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown inline confinement: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_InlineBadMountRejected asserts an inline workspace mount whose
// source is a denied host path (/etc) is rejected with 400 via ValidateMount,
// wired through validatePolicySpec — the same deny-list as a stored policy.
func TestCreateRun_InlineBadMountRejected(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","inline_policy":{"min_confinement_class":"CC2",` +
		`"workspace_mounts":[{"source":"/etc","target":"/work"}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("inline /etc mount: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_InlineWeakerConfinementRejected asserts an inline
// min_confinement_class of CC3 combined with a requested confinement_class of
// CC1 is a 422: the requested class is weaker than the (inline) policy minimum.
// This proves the inline spec drives the SAME confinement floor a stored policy
// does, and the check fires before any store write (no Pool).
func TestCreateRun_InlineWeakerConfinementRejected(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","confinement_class":"CC1",` +
		`"inline_policy":{"min_confinement_class":"CC3"}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inline CC3 vs requested CC1: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_InlineLocalMountRejectedUnlessOnboarded asserts the onboarding
// gate: a raw inline local-dir mount whose source is NOT a registered onboarded
// workspace is REJECTED at run-create (422), never silently mounted. The spec is
// still STRUCTURALLY valid (deny-list + confinement) — the rejection is the new
// store-aware onboarding check, which is the whole point of the feature. (Before
// onboarded workspaces this raw mount was accepted; it now must be onboarded first
// via the workspaces API. This no-Store harness makes the gate fail closed — it
// cannot verify onboarding without a store — which is the same 422.)
func TestCreateRun_InlineLocalMountRejectedUnlessOnboarded(t *testing.T) {
	h := newHarness(t)
	rw := false
	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		WorkspaceMounts: []types.WorkspaceMount{
			{Source: "/home/me/project", Target: "/work", ReadOnly: &rw},
		},
	}
	if err := validatePolicySpec(spec); err != nil {
		t.Fatalf("spec must still be structurally valid, got: %v", err)
	}

	body := `{"agent":"claude-code","inline_policy":{"min_confinement_class":"CC2",` +
		`"workspace_mounts":[{"source":"/home/me/project","target":"/work","read_only":false}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-onboarded inline mount must be rejected (422), got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_MemberInlineClamped pins item 5: a member's inline_policy is
// clamped to DefaultPolicy BEFORE resolution/validation; an admin's identical
// request is not. Observed via the policy.inline audit event's
// min_confinement_class field, which resolveRunPolicy records right after
// clamping — BEFORE the no-Store harness's later CreateRun panic (see the
// file's "accepted past validation" 500-sentinel doc comment above), so the
// clamp's effect is visible even though the request never fully succeeds.
func TestCreateRun_MemberInlineClamped(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	h.srv.router = h.srv.routes()

	const body = `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{"min_confinement_class":"CC1"}}`

	doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember), body)
	if got := lastInlinePolicyConfinement(t, h.audit.events); got != string(types.CC2) {
		t.Fatalf("member: policy.inline min_confinement_class = %q, want %q (clamped up to DefaultPolicy)", got, types.CC2)
	}

	doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), body)
	if got := lastInlinePolicyConfinement(t, h.audit.events); got != string(types.CC1) {
		t.Fatalf("admin: policy.inline min_confinement_class = %q, want %q (unclamped)", got, types.CC1)
	}
}

// lastInlinePolicyConfinement returns the min_confinement_class of the LAST
// policy.inline audit event in events, failing the test if there is none.
func lastInlinePolicyConfinement(t *testing.T, events []types.AuditEvent) string {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Action != "policy.inline" {
			continue
		}
		var d struct {
			MinConfinementClass string `json:"min_confinement_class"`
		}
		if err := json.Unmarshal(events[i].Data, &d); err != nil {
			t.Fatalf("decode policy.inline data: %v", err)
		}
		return d.MinConfinementClass
	}
	t.Fatal("no policy.inline audit event recorded")
	return ""
}

// TestFilterMemberGrants covers the secret-exfil gate: a member's inline
// stored-secret grant is kept only when the operator eligible-listed the exact
// {host, secret} pairing (or it is a host-pinned sentinel); everything else is
// dropped, and the run's own model grant is re-added by the fold downstream.
func TestFilterMemberGrants(t *testing.T) {
	h := newHarness(t)
	apiKey := func(host, secret string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": host, "secret_name": secret})}
	}

	// Wildcard api_key ceiling (the common LLM config): a member pairing an
	// arbitrary stored secret with an allowlisted host is DROPPED - its pairing
	// is not one the operator listed, so nothing is injected.
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey}}}
	kept, warns, code, err := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{apiKey("attacker.example", "prod-db-password")})
	if code != 0 || err != nil {
		t.Fatalf("exfil pairing: code=%d err=%v, want (0,nil) - dropped, not errored", code, err)
	}
	if len(kept) != 0 || len(warns) != 1 {
		t.Fatalf("exfil pairing: kept=%d warns=%d, want (0 kept, 1 warn)", len(kept), len(warns))
	}
	// The run's OWN model-access grant (real provider key) is likewise dropped
	// under a wildcard ceiling - that is fine, ensureLLMGrant re-adds it after
	// resolveRunPolicy returns (see TestCreateRun_MemberInlineGrantExfilDropped).
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{apiKey("api.anthropic.com", "anthropic-api-key")}); len(kept) != 0 {
		t.Fatalf("real-key LLM grant under wildcard ceiling: kept=%d, want 0 (dropped, re-folded downstream)", len(kept))
	}

	// A SPECIFIC operator pairing lets a member reuse THAT exact pairing...
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKey("api.corp.example", "corp-key")}}
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{apiKey("API.Corp.Example", "corp-key")}); len(kept) != 1 {
		t.Fatalf("exact operator-listed pairing (host case-insensitive): kept=%d, want 1", len(kept))
	}
	// ...but not that secret on a DIFFERENT host, nor a DIFFERENT secret on it.
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{apiKey("attacker.example", "corp-key")}); len(kept) != 0 {
		t.Fatalf("operator secret on attacker host: kept=%d, want 0 (dropped)", len(kept))
	}
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{apiKey("api.corp.example", "prod-db-password")}); len(kept) != 0 {
		t.Fatalf("different secret on listed host: kept=%d, want 0 (dropped)", len(kept))
	}

	// git_pat references a stored secret too - same drop.
	gitPAT := types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "git.attacker.example", "secret_name": "corp-key"})}
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{gitPAT}); len(kept) != 0 {
		t.Fatalf("member git_pat exfil pairing: kept=%d, want 0 (dropped)", len(kept))
	}

	// github_token references no stored secret - always kept (its scope is
	// intersected by composer.Clamp, not gated here).
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{{Kind: types.GrantGitHubToken}}); len(kept) != 1 {
		t.Fatalf("github_token grant: kept=%d, want 1", len(kept))
	}
	// A malformed api_key scope is a bad request (fail closed), not a silent drop.
	bad := types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "api.corp.example"})} // no secret_name
	if _, _, code, err := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{bad}); code != http.StatusUnprocessableEntity || err == nil {
		t.Fatalf("malformed api_key scope: code=%d err=%v, want (422, error)", code, err)
	}
	// No grants: nothing to filter.
	if kept, warns, code, err := h.srv.filterMemberGrants(context.Background(), "", nil, nil); len(kept) != 0 || len(warns) != 0 || code != 0 || err != nil {
		t.Fatalf("no grants: kept=%d warns=%d code=%d err=%v", len(kept), len(warns), code, err)
	}
}

// TestFilterMemberGrants_SSHKeyKnownHostsPairing is the W12-B-2 regression: an
// ssh_key grant's known_hosts_secret_ref must match the ceiling's OWN
// known_hosts_secret_ref for that exact (host, key_secret_ref) pairing — a
// member must not be able to reuse an operator-approved key pairing while
// attaching a DIFFERENT known_hosts_secret_ref of their own choosing. Before the
// fix, storedSecretGrantPairing ignored known_hosts_secret_ref entirely, so a
// mismatched/added ref here was wrongly KEPT (it would let mintSSHKey, broker.go,
// return an arbitrary stored secret's value as Minted.KnownHosts, escaping this
// gate). Fails on base 6d76911; passes once known_hosts_secret_ref is part of
// the pairing comparison.
func TestFilterMemberGrants_SSHKeyKnownHostsPairing(t *testing.T) {
	h := newHarness(t)
	sshKey := func(host, keyRef, khRef string) types.GrantSpec {
		sc := map[string]any{"host": host, "key_secret_ref": keyRef}
		if khRef != "" {
			sc["known_hosts_secret_ref"] = khRef
		}
		return types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(sc)}
	}

	// Operator ceiling: github.com/gh-ssh-key pinned to gh-known-hosts.
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{sshKey("github.com", "gh-ssh-key", "gh-known-hosts")},
	}

	// Exact pairing (same host, key, known_hosts) is kept.
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{sshKey("github.com", "gh-ssh-key", "gh-known-hosts")}); len(kept) != 1 {
		t.Fatalf("exact ssh_key pairing incl. known_hosts: kept=%d, want 1", len(kept))
	}
	// Same host+key, but a DIFFERENT known_hosts_secret_ref of the member's own
	// choosing: dropped. This is the W12-B-2 bypass case.
	if kept, warns, code, err := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{sshKey("github.com", "gh-ssh-key", "attacker-secret")}); len(kept) != 0 || len(warns) != 1 || code != 0 || err != nil {
		t.Fatalf("mismatched known_hosts_secret_ref: kept=%d warns=%d code=%d err=%v, want (0,1,0,nil) - member must not smuggle a different known_hosts ref", len(kept), len(warns), code, err)
	}
	// Same host+key, known_hosts_secret_ref OMITTED where the ceiling names one:
	// also not an exact match, dropped.
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{sshKey("github.com", "gh-ssh-key", "")}); len(kept) != 0 {
		t.Fatalf("omitted known_hosts_secret_ref vs ceiling's set one: kept=%d, want 0", len(kept))
	}

	// Ceiling with NO known_hosts_secret_ref (the common case: the image-baked
	// known_hosts covers github.com/ADO). A member's grant must also omit it to
	// match (empty==empty); naming ANY known_hosts_secret_ref is a mismatch.
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{sshKey("dev.azure.com", "ado-ssh-key", "")},
	}
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{sshKey("dev.azure.com", "ado-ssh-key", "")}); len(kept) != 1 {
		t.Fatalf("both empty known_hosts_secret_ref: kept=%d, want 1", len(kept))
	}
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{sshKey("dev.azure.com", "ado-ssh-key", "some-secret")}); len(kept) != 0 {
		t.Fatalf("member-added known_hosts_secret_ref where ceiling has none: kept=%d, want 0", len(kept))
	}
}

// TestCreateRun_MemberInlineGrantExfilDropped proves the gate is wired into the
// create-run inline path and is member-only: a member's unmatched stored-secret
// pairing is dropped from the resolved policy (eligible_grants=0), while an
// operator (ceiling authority) keeps theirs.
func TestCreateRun_MemberInlineGrantExfilDropped(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	// Wildcard api_key ceiling + the attacker host allowlisted, so ONLY the
	// grant-scope gate - not egress - stands between a member and exfil.
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"attacker.example"},
		EligibleGrants:      []types.GrantSpec{{Kind: types.GrantAPIKey}},
	}
	h.srv.router = h.srv.routes()

	// Pair a REAL operator secret (seeded) with an attacker-controlled but
	// allowlisted host. CC2 (like TestCreateRun_MemberInlineClamped) stops the run
	// at the confinement gate on this daemonless harness — AFTER resolveRunPolicy
	// records policy.inline — so the audit shows the resolved grant count, and the
	// admin's kept grant clears validateInlineSecretRefs (the secret exists).
	const body = `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"api_key","scope":{"host":"attacker.example","secret_name":"anthropic-api-key"}}]}}`

	// Member: the exfil pairing is dropped - the resolved inline policy carries
	// zero grants, so nothing is ever injected.
	doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember), body)
	if got := lastInlinePolicyGrantCount(t, h.audit.events); got != 0 {
		t.Fatalf("member: policy.inline eligible_grants = %d, want 0 (exfil pairing dropped)", got)
	}

	// Operator (ceiling authority) is unclamped - their grant is kept.
	doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), body)
	if got := lastInlinePolicyGrantCount(t, h.audit.events); got != 1 {
		t.Fatalf("admin: policy.inline eligible_grants = %d, want 1 (unclamped)", got)
	}
}

// lastInlinePolicyGrantCount returns the eligible_grants count of the LAST
// policy.inline audit event in events, failing the test if there is none.
func lastInlinePolicyGrantCount(t *testing.T, events []types.AuditEvent) int {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Action != "policy.inline" {
			continue
		}
		var d struct {
			EligibleGrants int `json:"eligible_grants"`
		}
		if err := json.Unmarshal(events[i].Data, &d); err != nil {
			t.Fatalf("decode policy.inline data: %v", err)
		}
		return d.EligibleGrants
	}
	t.Fatal("no policy.inline audit event recorded")
	return 0
}

// TestValidateInlineSecretRefs_Matrix exercises validateInlineSecretRefs across
// the four documented outcomes: present ok / missing err / reserved err / no
// store err. It uses the memSecrets fake (seeded with "anthropic-api-key").
func TestValidateInlineSecretRefs_Matrix(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	ctx := context.Background()

	apiKeyGrant := func(secretName string) types.RunPolicySpec {
		return types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantAPIKey,
				Scope: mustJSON(map[string]any{"host": "api.anthropic.com", "secret_name": secretName}),
			}},
		}
	}

	// present => ok (no error, code 0).
	if code, err := h.srv.validateInlineSecretRefs(ctx, "", apiKeyGrant("anthropic-api-key")); err != nil || code != 0 {
		t.Fatalf("present secret: code=%d err=%v, want (0,nil)", code, err)
	}

	// no api_key grants => ok regardless of store.
	noKeys := types.RunPolicySpec{MinConfinementClass: types.CC2}
	if code, err := h.srv.validateInlineSecretRefs(ctx, "", noKeys); err != nil || code != 0 {
		t.Fatalf("no api_key grants: code=%d err=%v, want (0,nil)", code, err)
	}

	// missing secret => 422.
	if code, err := h.srv.validateInlineSecretRefs(ctx, "", apiKeyGrant("does-not-exist")); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("missing secret: code=%d err=%v, want (422,err)", code, err)
	}

	// reserved name => 422 (never even consults the store value).
	if code, err := h.srv.validateInlineSecretRefs(ctx, "", apiKeyGrant("wardyn-signing-key")); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("reserved secret: code=%d err=%v, want (422,err)", code, err)
	}

	// no store configured => 422.
	noStore := newHarness(t) // default harness has no Secrets store
	if code, err := noStore.srv.validateInlineSecretRefs(ctx, "", apiKeyGrant("anthropic-api-key")); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("no store: code=%d err=%v, want (422,err)", code, err)
	}

	// Subscription OAuth sentinel: NOT a stored secret. Without a subscription
	// token provider it is a clear 422 (not the misleading "unknown secret" hint);
	// WITH a provider it validates without needing the name in the store (the
	// saved-workspace-replay fix).
	sentinel := apiKeyGrant(types.SubscriptionOAuthSecret)
	if code, err := h.srv.validateInlineSecretRefs(ctx, "", sentinel); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("sentinel w/o provider: code=%d err=%v, want (422,err)", code, err)
	}
	h.srv.cfg.SubscriptionToken = fakeSubToken{}
	defer func() { h.srv.cfg.SubscriptionToken = nil }()
	if code, err := h.srv.validateInlineSecretRefs(ctx, "", sentinel); err != nil || code != 0 {
		t.Fatalf("sentinel w/ provider: code=%d err=%v, want (0,nil)", code, err)
	}
}

// fakeSubToken is a minimal subscription.Provider for tests: it only needs to be
// non-nil for validateInlineSecretRefs' sentinel special-case.
type fakeSubToken struct{}

func (fakeSubToken) Current(context.Context) (subscription.Token, error) {
	return subscription.Token{Value: "live-oauth-token"}, nil
}
func (fakeSubToken) Peek() (subscription.Token, error) {
	return subscription.Token{Value: "live-oauth-token"}, nil
}

// TestCreateRun_InlineMissingSecretRejected wires the secret check through the
// HTTP path: an inline api_key grant referencing an unknown secret is a 422
// before any store write (the validation boundary).
func TestCreateRun_InlineMissingSecretRejected(t *testing.T) {
	h, _ := newSecretsHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{` +
		`"min_confinement_class":"CC2","eligible_grants":[{"kind":"api_key",` +
		`"scope":{"host":"api.example.com","secret_name":"nope-not-here"}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inline missing secret: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// TestValidatePolicySpec_RejectsReservedApiKeySecret asserts the write-time guard
// that closes the key-exfiltration path for BOTH stored policies (POST /policies)
// and inline specs: an api_key grant naming a reserved platform-internal secret
// (wardyn-signing-key / wardyn-session-key) is rejected by validatePolicySpec. The
// injection sink (handleInternalInjection) enforces the same invariant defense-in-
// depth. A non-reserved api_key name passes this guard (its EXISTENCE is
// validateInlineSecretRefs' job, not validatePolicySpec's).
func TestValidatePolicySpec_RejectsReservedApiKeySecret(t *testing.T) {
	reserved := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "attacker.example", "secret_name": "wardyn-signing-key"}),
		}},
	}
	if err := validatePolicySpec(reserved); err == nil {
		t.Fatal("validatePolicySpec must reject an api_key grant referencing a reserved secret name")
	}

	ok := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "api.anthropic.com", "secret_name": "anthropic-api-key"}),
		}},
	}
	if err := validatePolicySpec(ok); err != nil {
		t.Fatalf("validatePolicySpec must accept a non-reserved api_key grant, got: %v", err)
	}
}

// TestValidatePolicySpec_RejectsSplittingApiKeyHeader is the header-name half of
// the same write-time guard: an api_key grant's Header is written verbatim onto
// a forwarded request by the proxy, so a name carrying CR/LF is a
// header-splitting shape. Rejected here for stored AND inline specs; the
// injection sink enforces it again defense-in-depth
// (TestHandleInternalInjection_RejectsSplittingHeaderName).
func TestValidatePolicySpec_RejectsSplittingApiKeyHeader(t *testing.T) {
	spec := func(header string) types.RunPolicySpec {
		return types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind: types.GrantAPIKey,
				Scope: mustJSON(map[string]any{
					"host": "api.anthropic.com", "secret_name": "anthropic-api-key", "header": header,
				}),
			}},
		}
	}
	for _, bad := range []string{"X-Tok\r\nX-Evil: 1", "X-Tok\nX-Evil: 1", "Authorization: Bearer", "X Tok", "X-Tok\x00"} {
		if err := validatePolicySpec(spec(bad)); err == nil {
			t.Errorf("validatePolicySpec accepted api_key header %q, want rejection", bad)
		}
	}
	// An omitted header defaults to "Authorization" in injectionRuleFromScope,
	// so the guard must not fail-closed on the ordinary shape.
	for _, good := range []string{"", "Authorization", "x-api-key", "DD-API-KEY"} {
		if err := validatePolicySpec(spec(good)); err != nil {
			t.Errorf("validatePolicySpec rejected valid api_key header %q: %v", good, err)
		}
	}
}

// TestPolicy_RejectsBedrockResidentSecretAtSinks asserts the three RESIDENT
// AWS SigV4 credential names read directly by resolveBedrockAuth (aws-access-key-id
// / aws-secret-access-key / aws-session-token) are sink-reserved — an
// api_key/git_pat/ssh_key grant naming one is rejected at policy-write time — so a
// policy can never exfiltrate the operator's long-lived AWS secret key as an
// injected header or git password. bedrock-api-key is deliberately NOT reserved:
// the never-resident Bedrock BEARER path legitimately authors a host-pinned api_key
// grant for it (runs.go), and reserving it would fail-close that path.
func TestPolicy_RejectsBedrockResidentSecretAtSinks(t *testing.T) {
	for _, name := range []string{"aws-access-key-id", "aws-secret-access-key", "aws-session-token"} {
		apiKey := types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantAPIKey,
				Scope: mustJSON(map[string]any{"host": "attacker.example", "secret_name": name}),
			}},
		}
		if err := validatePolicySpec(apiKey); err == nil {
			t.Fatalf("api_key naming resident AWS secret %q must be rejected", name)
		}
		gitPAT := types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantGitPAT,
				Scope: mustJSON(map[string]any{"host": "attacker.example", "secret_name": name}),
			}},
		}
		if err := validatePolicySpec(gitPAT); err == nil {
			t.Fatalf("git_pat naming resident AWS secret %q must be rejected", name)
		}
		sshKey := types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantSSHKey,
				Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": name}),
			}},
		}
		if err := validatePolicySpec(sshKey); err == nil {
			t.Fatalf("ssh_key naming resident AWS secret %q must be rejected", name)
		}
	}

	// bedrock-api-key must stay usable in an api_key grant (the never-resident BEARER
	// Bedrock path) — over-reserving it would break that path.
	bearer := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "bedrock-runtime.us-east-1.amazonaws.com", "secret_name": "bedrock-api-key"}),
		}},
	}
	if err := validatePolicySpec(bearer); err != nil {
		t.Fatalf("bedrock-api-key api_key grant must NOT be sink-reserved: %v", err)
	}
}

// ─── H1 regression: the stored/default policy branch now runs the SAME
// validateInlineSecretRefs check as the inline branch (previously it only ran
// for inline_policy) — a stored or default policy naming a missing secret now
// 422s at create, naming the secret, instead of only failing later at first
// proxy injection. See CHANGELOG.md "Changed".

// stubPolicyStore is a minimal store.Store for the stored-policy path: it
// embeds the interface (nil — any other method panics if called, which is
// fine here since resolveRunPolicy's secret check fires before any other
// store call in handleCreateRun) and overrides ONLY GetPolicy.
type stubPolicyStore struct {
	store.Store
	policy types.RunPolicy
}

func (s stubPolicyStore) GetPolicy(context.Context, uuid.UUID) (types.RunPolicy, error) {
	return s.policy, nil
}

// TestCreateRun_StoredPolicyMissingSecretRejected asserts a run created with
// policy_id pointing at a STORED policy whose api_key grant references a
// not-yet-stored secret is rejected 422, naming the secret, at create — the
// same outcome TestCreateRun_InlineMissingSecretRejected proves for inline.
func TestCreateRun_StoredPolicyMissingSecretRejected(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with only "anthropic-api-key"
	policyID := uuid.New()
	h.srv.cfg.Store = stubPolicyStore{policy: types.RunPolicy{
		ID: policyID,
		Spec: types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantAPIKey,
				Scope: mustJSON(map[string]any{"host": "api.example.com", "secret_name": "nope-not-here"}),
			}},
		},
	}}
	h.srv.router = h.srv.routes() // re-mount with the Store swap in effect

	body := `{"agent":"claude-code","repo":"acme/widgets","policy_id":"` + policyID.String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("stored policy missing secret: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "nope-not-here") {
		t.Fatalf("422 body must name the missing secret, got: %s", w.Body.String())
	}
}

// TestCreateRun_DefaultPolicyMissingSecretRejected asserts the SAME outcome
// when neither policy_id nor inline_policy is set — the configured
// DefaultPolicy runs through the identical check.
func TestCreateRun_DefaultPolicyMissingSecretRejected(t *testing.T) {
	h, _ := newSecretsHarness(t)
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: mustJSON(map[string]any{"host": "api.example.com", "secret_name": "still-not-here"}),
		}},
	}

	body := `{"agent":"claude-code","repo":"acme/widgets"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("default policy missing secret: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "still-not-here") {
		t.Fatalf("422 body must name the missing secret, got: %s", w.Body.String())
	}
}

// TestCreateRun_StoredPolicyNoSecretStoreRejected asserts the H1-decision
// grants-exist-but-Secrets==nil case for the stored-policy path: a stored
// policy with an api_key grant, but NO secret store configured at all, 422s
// (fail closed) exactly like the inline branch already does.
func TestCreateRun_StoredPolicyNoSecretStoreRejected(t *testing.T) {
	h := newHarness(t) // default harness: no Secrets store configured
	policyID := uuid.New()
	h.srv.cfg.Store = stubPolicyStore{policy: types.RunPolicy{
		ID: policyID,
		Spec: types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantAPIKey,
				Scope: mustJSON(map[string]any{"host": "api.example.com", "secret_name": "anthropic-api-key"}),
			}},
		},
	}}
	h.srv.router = h.srv.routes()

	body := `{"agent":"claude-code","repo":"acme/widgets","policy_id":"` + policyID.String() + `"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("stored policy, no secret store: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// TestStoredSecretGrantPairing_UnknownKindIsRefused is the closed-switch
// regression. storedSecretGrantPairing's default arm used to return
// covered=false — indistinguishable from "github_token names no stored secret"
// — so BOTH member gates waved an unrecognized kind straight through:
// filterMemberGrants kept it unclamped by the operator's eligible-grant
// pairing, and narrowMemberInlinePolicy kept it unchecked against capSecret.
// Any grant kind added to types.GrantKind and wired to a stored secret was
// therefore member-authorable until somebody remembered to extend the switch.
// It must now be REFUSED (covered=true WITH an error), which filterMemberGrants
// renders as a 422 and narrowMemberInlinePolicy as a drop.
func TestStoredSecretGrantPairing_UnknownKindIsRefused(t *testing.T) {
	unknown := types.GrantSpec{
		Kind:  types.GrantKind("some_future_kind"),
		Scope: mustJSON(map[string]any{"secret_name": "prod-db-password"}),
	}

	_, _, _, covered, err := storedSecretGrantPairing(unknown)
	if !covered || err == nil {
		t.Fatalf("unknown kind: covered=%v err=%v, want (true, error) — the default arm must refuse, not fall through", covered, err)
	}

	// Gate 1: the whole spec is rejected 422, never silently narrowed.
	h := newHarness(t)
	kept, _, code, ferr := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{unknown})
	if code != http.StatusUnprocessableEntity || ferr == nil || len(kept) != 0 {
		t.Fatalf("filterMemberGrants(unknown kind): kept=%d code=%d err=%v, want (0, 422, error)", len(kept), code, ferr)
	}

	// Gate 2 (defense in depth — gate 1 runs first in the only shipped order):
	// dropped rather than kept, so the ordering is not the only thing holding.
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{unknown}}
	warns, drops, nerr := h.srv.narrowMemberInlinePolicy(context.Background(), "", &spec)
	if nerr != nil {
		t.Fatalf("narrowMemberInlinePolicy: unexpected error %v", nerr)
	}
	if len(spec.EligibleGrants) != 0 || len(warns) != 1 || len(drops) != 1 {
		t.Fatalf("narrowMemberInlinePolicy(unknown kind): kept=%d warns=%d drops=%d, want (0,1,1)",
			len(spec.EligibleGrants), len(warns), len(drops))
	}

	// The two kinds that genuinely name no stored secret stay uncovered.
	for _, k := range []types.GrantKind{types.GrantGitHubToken, types.GrantCloudSTS} {
		if _, _, _, covered, err := storedSecretGrantPairing(types.GrantSpec{Kind: k}); covered || err != nil {
			t.Fatalf("%s: covered=%v err=%v, want (false, nil)", k, covered, err)
		}
	}
}

// ─── 6c: own-key exemptions ───────────────────────────────────────────────────

// memberAPIKeyGrant builds an api_key grant scoped to (host, secret) — the
// same shape apiKey() in TestFilterMemberGrants builds, factored out for the
// own-key tests below.
func memberAPIKeyGrant(host, secret string) types.GrantSpec {
	return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": host, "secret_name": secret})}
}

// TestFilterMemberGrants_OwnKeyToModelHost_Kept: a member's OWN api_key
// secret, paired with a model-provider host the run's own (clamped) egress
// allows, is admitted with NO operator eligible-grant pairing at all.
func TestFilterMemberGrants_OwnKeyToModelHost_Kept(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{owned: map[string]map[string][]byte{"bob": {"my-anthropic-key": {}}}}
	g := memberAPIKeyGrant("api.anthropic.com", "my-anthropic-key")
	kept, warns, code, err := h.srv.filterMemberGrants(context.Background(), "bob", []string{"api.anthropic.com"}, []types.GrantSpec{g})
	if err != nil || code != 0 {
		t.Fatalf("own key to model host: code=%d err=%v", code, err)
	}
	if len(kept) != 1 || len(warns) != 0 {
		t.Fatalf("own key to model host: kept=%d warns=%d, want (1, 0)", len(kept), len(warns))
	}
}

// TestFilterMemberGrants_OwnsNothing_Dropped: the same host+ceiling, but the
// member owns no secret by that name — dropped exactly as an unpaired one is.
func TestFilterMemberGrants_OwnsNothing_Dropped(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{owned: map[string]map[string][]byte{"bob": {}}}
	g := memberAPIKeyGrant("api.anthropic.com", "my-anthropic-key")
	kept, _, code, err := h.srv.filterMemberGrants(context.Background(), "bob", []string{"api.anthropic.com"}, []types.GrantSpec{g})
	if err != nil || code != 0 {
		t.Fatalf("owns nothing: code=%d err=%v", code, err)
	}
	if len(kept) != 0 {
		t.Fatalf("owns nothing: kept=%d, want 0", len(kept))
	}
}

// TestFilterMemberGrants_OwnKeyToNonModelHost_Dropped: the member owns the
// secret, but the host is not a model-provider host — the own-key arm never
// fires for an arbitrary host (it would be a generic secret-exfil primitive).
func TestFilterMemberGrants_OwnKeyToNonModelHost_Dropped(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{owned: map[string]map[string][]byte{"bob": {"my-key": {}}}}
	g := memberAPIKeyGrant("attacker.example", "my-key")
	kept, _, code, err := h.srv.filterMemberGrants(context.Background(), "bob", []string{"attacker.example"}, []types.GrantSpec{g})
	if err != nil || code != 0 {
		t.Fatalf("own key to non-model host: code=%d err=%v", code, err)
	}
	if len(kept) != 0 {
		t.Fatalf("own key to non-model host: kept=%d, want 0", len(kept))
	}
}

// TestFilterMemberGrants_UnpairedOperatorSecret_Dropped: an OPERATOR-owned
// secret name (the member owns nothing by that name) paired with a model
// host, with no operator eligible-grant pairing, is still dropped — ownership
// must be proved, never assumed from the name alone.
func TestFilterMemberGrants_UnpairedOperatorSecret_Dropped(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": {}}, owned: map[string]map[string][]byte{"bob": {}}}
	g := memberAPIKeyGrant("api.anthropic.com", "anthropic-api-key")
	kept, _, code, err := h.srv.filterMemberGrants(context.Background(), "bob", []string{"api.anthropic.com"}, []types.GrantSpec{g})
	if err != nil || code != 0 {
		t.Fatalf("unpaired operator secret: code=%d err=%v", code, err)
	}
	if len(kept) != 0 {
		t.Fatalf("unpaired operator secret: kept=%d, want 0 (the member does not own this name)", len(kept))
	}
}

// TestFilterMemberGrants_GitPATOwnSecret_Dropped: the own-key arm is api_key
// only — a git_pat pairing the member "owns" the secret for still needs the
// operator's eligible-grant pairing (git_pat/ssh_key/env_secret stay
// ceiling-paired, per the plan).
func TestFilterMemberGrants_GitPATOwnSecret_Dropped(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{owned: map[string]map[string][]byte{"bob": {"my-pat": {}}}}
	g := types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "gitlab.corp.example", "secret_name": "my-pat"})}
	kept, _, code, err := h.srv.filterMemberGrants(context.Background(), "bob", []string{"gitlab.corp.example"}, []types.GrantSpec{g})
	if err != nil || code != 0 {
		t.Fatalf("git_pat own secret: code=%d err=%v", code, err)
	}
	if len(kept) != 0 {
		t.Fatalf("git_pat own secret: kept=%d, want 0 (git_pat stays ceiling-paired)", len(kept))
	}
}

// TestNarrowMemberInlinePolicy_CapSecretOwnKey_Kept_OperatorOnlyName_Refused:
// with capSecret ENFORCED and no capability grants at all, a member's own
// secret name is exempt (kept); an operator-only name is still refused.
func TestNarrowMemberInlinePolicy_CapSecretOwnKey_Kept_OperatorOnlyName_Refused(t *testing.T) {
	st := &capStore{enf: map[string]bool{capSecret: true}}
	srv := capServer(st)
	srv.cfg.Secrets = &memSecrets{owned: map[string]map[string][]byte{"bob": {"my-own-key": {}}}}
	ctx := memberCtx(nil)

	own := &types.RunPolicySpec{EligibleGrants: []types.GrantSpec{memberAPIKeyGrant("api.anthropic.com", "my-own-key")}}
	_, drops, err := srv.narrowMemberInlinePolicy(ctx, "bob", own)
	if err != nil {
		t.Fatalf("own key: unexpected error %v", err)
	}
	if len(own.EligibleGrants) != 1 || len(drops) != 0 {
		t.Fatalf("own key must be kept under enforced capSecret with no grant, got kept=%d drops=%+v", len(own.EligibleGrants), drops)
	}

	opOnly := &types.RunPolicySpec{EligibleGrants: []types.GrantSpec{memberAPIKeyGrant("api.anthropic.com", "operator-only-key")}}
	_, drops2, err2 := srv.narrowMemberInlinePolicy(ctx, "bob", opOnly)
	if err2 != nil {
		t.Fatalf("operator-only name: unexpected error %v", err2)
	}
	if len(opOnly.EligibleGrants) != 0 || len(drops2) != 1 {
		t.Fatalf("an operator-only name must still be refused under enforced capSecret, got kept=%d drops=%+v", len(opOnly.EligibleGrants), drops2)
	}
}

// TestValidateInlineSecretRefs_OwnName_201: an api_key grant naming a secret
// the operator does NOT hold, but the member DOES own, passes — the operator
// lacking the name is not itself a 422 once ownership covers it.
func TestValidateInlineSecretRefs_OwnName_201(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{owned: map[string]map[string][]byte{"bob": {"my-anthropic-key": {}}}}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{memberAPIKeyGrant("api.anthropic.com", "my-anthropic-key")}}
	if code, err := h.srv.validateInlineSecretRefs(context.Background(), "bob", spec); err != nil || code != 0 {
		t.Fatalf("own name, operator lacks it: code=%d err=%v, want (0, nil)", code, err)
	}
	// Negative control: an operator-only name the member doesn't own still 422s.
	spec2 := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{memberAPIKeyGrant("api.anthropic.com", "operator-only-key")}}
	if code, err := h.srv.validateInlineSecretRefs(context.Background(), "bob", spec2); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("operator-only name, not owned: code=%d err=%v, want (422, error)", code, err)
	}
}

// TestCreateRun_OperatorStillUnclamped: an operator's inline_policy is NEVER
// routed through filterMemberGrants/narrowMemberInlinePolicy — an egress host
// and a grant pairing that would be dropped for a member survive untouched,
// with zero clamp warnings, for an operator caller.
func TestCreateRun_OperatorStillUnclamped(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
	}
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"prod-db-password": []byte("v")}}
	req := createRunRequest{
		Agent: "claude-code",
		InlinePolicy: &types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			AllowedDomains:      []string{"attacker.example"},
			EligibleGrants:      []types.GrantSpec{memberAPIKeyGrant("attacker.example", "prod-db-password")},
		},
	}
	w := httptest.NewRecorder()
	// A plain request with no OIDC session reads as operator (isOperator).
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	spec, _, warnings, ok := h.srv.resolveRunPolicy(context.Background(), w, r, &req, false)
	if !ok {
		t.Fatalf("resolveRunPolicy rejected: %s", w.Body.String())
	}
	if len(warnings) != 0 {
		t.Fatalf("an operator must never be clamped, got warnings %v", warnings)
	}
	if len(spec.AllowedDomains) != 1 || spec.AllowedDomains[0] != "attacker.example" {
		t.Fatalf("operator's own inline egress must survive unclamped, got %v", spec.AllowedDomains)
	}
	if len(spec.EligibleGrants) != 1 {
		t.Fatalf("operator's own inline grant must survive unclamped, got %d", len(spec.EligibleGrants))
	}
}

// TestIntegrations_MemberKeySynthesisesRow_NoWarning: a member's own
// anthropic-api-key secret, with NO operator row of that name at all,
// synthesises the legacy anthropic_api_key integration row and provisions
// model access with no "no model access" warning.
func TestIntegrations_MemberKeySynthesisesRow_NoWarning(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{owned: map[string]map[string][]byte{"bob": {"anthropic-api-key": []byte("sk-ant-test")}}}

	integ, ok := h.srv.resolveIntegrationRef(context.Background(), "bob", "anthropic_api_key")
	if !ok {
		t.Fatal("member's own anthropic-api-key must synthesise the legacy anthropic_api_key row")
	}
	if !types.AIProviderKind(integ.Kind) {
		t.Fatalf("synthesised row kind = %q, not an AI-provider kind", integ.Kind)
	}

	present := h.srv.presentSecretNamesFor(context.Background(), "bob")
	if !present["anthropic-api-key"] {
		t.Fatal("presentSecretNamesFor must include the member's own secret")
	}
	spec := &types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"},
		EligibleGrants: []types.GrantSpec{memberAPIKeyGrant("api.anthropic.com", "anthropic-api-key")},
	}
	note, provisioned := h.srv.reconcileLLMAccess(spec, "claude-code", present, false, false)
	if !provisioned {
		t.Fatalf("expected model access provisioned with no operator row, got note=%q", note)
	}
}

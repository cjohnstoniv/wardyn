// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

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
	if code, err := h.srv.validateInlineSecretRefs(ctx, apiKeyGrant("anthropic-api-key")); err != nil || code != 0 {
		t.Fatalf("present secret: code=%d err=%v, want (0,nil)", code, err)
	}

	// no api_key grants => ok regardless of store.
	noKeys := types.RunPolicySpec{MinConfinementClass: types.CC2}
	if code, err := h.srv.validateInlineSecretRefs(ctx, noKeys); err != nil || code != 0 {
		t.Fatalf("no api_key grants: code=%d err=%v, want (0,nil)", code, err)
	}

	// missing secret => 422.
	if code, err := h.srv.validateInlineSecretRefs(ctx, apiKeyGrant("does-not-exist")); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("missing secret: code=%d err=%v, want (422,err)", code, err)
	}

	// reserved name => 422 (never even consults the store value).
	if code, err := h.srv.validateInlineSecretRefs(ctx, apiKeyGrant("wardyn-signing-key")); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("reserved secret: code=%d err=%v, want (422,err)", code, err)
	}

	// no store configured => 422.
	noStore := newHarness(t) // default harness has no Secrets store
	if code, err := noStore.srv.validateInlineSecretRefs(ctx, apiKeyGrant("anthropic-api-key")); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("no store: code=%d err=%v, want (422,err)", code, err)
	}

	// Subscription OAuth sentinel: NOT a stored secret. Without a subscription
	// token provider it is a clear 422 (not the misleading "unknown secret" hint);
	// WITH a provider it validates without needing the name in the store (the
	// saved-workspace-replay fix).
	sentinel := apiKeyGrant(types.SubscriptionOAuthSecret)
	if code, err := h.srv.validateInlineSecretRefs(ctx, sentinel); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("sentinel w/o provider: code=%d err=%v, want (422,err)", code, err)
	}
	h.srv.cfg.SubscriptionToken = fakeSubToken{}
	defer func() { h.srv.cfg.SubscriptionToken = nil }()
	if code, err := h.srv.validateInlineSecretRefs(ctx, sentinel); err != nil || code != 0 {
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

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A secret store that cannot be listed is the server's fault, not the policy
// author's: validateRunSecretRefs answers 503 with a sentence free of driver
// text, and the run refusal carries the transient store reason (#2781).
func TestValidateRunSecretRefs_ListFailureIs503NotInvalidPolicy(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = listFailSecrets{Store: &memSecrets{}}
	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants:      []types.GrantSpec{apiKeyGrant("api.anthropic.com", "anthropic-api-key")},
	}
	code, err := h.srv.validateRunSecretRefs(context.Background(), "", "", spec, true)
	if err == nil || code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d err=%v, want (503, err)", code, err)
	}
	if strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("driver text leaked into the refusal: %v", err)
	}
}

// The run refusal for that 503 names the transient store reason and carries no
// "invalid ..." prefix.
func TestRunSecretRefsRefusal_StoreFaultHasNoInvalidPrefix(t *testing.T) {
	ref := runSecretRefsRefusal("invalid inline_policy: ", http.StatusServiceUnavailable, errSecretStoreUnavailable)
	if ref.status != http.StatusServiceUnavailable || ref.body.Reason != reasonSinkStoreUnavailable || strings.Contains(ref.body.Error, "invalid") {
		t.Fatalf("refusal = %d %+v, want 503 %s without an \"invalid\" prefix", ref.status, ref.body, reasonSinkStoreUnavailable)
	}
	ref = runSecretRefsRefusal("invalid policy: ", http.StatusUnprocessableEntity, errors.New("secret x not found"))
	if ref.status != http.StatusUnprocessableEntity || ref.body.Reason != reasonInlinePolicyInvalid {
		t.Fatalf("a genuine policy fault must keep 422 %s, got %d %+v", reasonInlinePolicyInvalid, ref.status, ref.body)
	}
}

// POST /runs with an inline secret grant while the secret store cannot be listed answers 503 with
// the transient store reason, never a 422 "invalid inline_policy" (#2781).
func TestCreateRun_InlineSecretListFailureIs503(t *testing.T) {
	h, _ := newSecretsHarness(t)
	h.srv.cfg.Secrets = listFailSecrets{Store: &memSecrets{}}
	body := `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{` +
		`"min_confinement_class":"CC2","eligible_grants":[{"kind":"api_key",` +
		`"scope":{"host":"api.example.com","secret_name":"anthropic-api-key"}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonSinkStoreUnavailable {
		t.Fatalf("inline secret with an unlistable store = %d %s, want 503 %s", w.Code, w.Body.String(), reasonSinkStoreUnavailable)
	}
}

// A secret named in a person's inline policy that the operator does not hold is
// looked up in their own rows; a store fault there is the fixed retryable 503
// store_unavailable, never "unknown secret" and its 422 (#3506).
func TestValidateRunSecretRefs_OwnListFaultIs503NotUnknownSecret(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = ownListFaultSecrets{Store: &memSecrets{}, reads: &atomic.Int64{}}
	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants:      []types.GrantSpec{apiKeyGrant("api.example.com", "alice-own-key")},
	}
	code, err := h.srv.validateRunSecretRefs(context.Background(), "sub-alice", "sub-alice", spec, true)
	if code != http.StatusServiceUnavailable || !errors.Is(err, errSecretStoreUnavailable) {
		t.Fatalf("code=%d err=%v, want (503, %v)", code, err, errSecretStoreUnavailable)
	}
	ref := runSecretRefsRefusal("invalid inline_policy: ", code, err)
	if ref.status != http.StatusServiceUnavailable || ref.body.Reason != reasonSinkStoreUnavailable {
		t.Fatalf("refusal = %d %+v, want 503 %s", ref.status, ref.body, reasonSinkStoreUnavailable)
	}
}

// ownFaultMemberServer is a governed deployment that pairs operator-key with
// api.anthropic.com, over a store whose person's own rows cannot be listed.
func ownFaultMemberServer(t *testing.T) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, r3PlainStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantAPIKey,
			Scope: json.RawMessage(`{"host":"api.anthropic.com","secret_name":"operator-key","header":"X-Api-Key"}`),
		}},
	}
	cfg.Secrets = ownListFaultSecrets{Store: newCountingSecretStore(nil, nil), reads: &atomic.Int64{}}
	return New(cfg)
}

// A member's own model key on a model host, checked by filterUserGrants' own-key
// arm, reaches the door as the fixed 503 store_unavailable through the policy
// resolution, not an invalid inline_policy and not a dropped grant (#3506).
func TestPreflight_OwnKeyListFaultIs503StoreUnavailable(t *testing.T) {
	srv := ownFaultMemberServer(t)
	body, err := json.Marshal(map[string]any{"agent": "claude-code", "task": "t", "inline_policy": types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants:      []types.GrantSpec{apiKeyGrant("api.anthropic.com", "alice-model-key")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	member := ssoSession(t, "sub-gov-bob", "bob@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", member, string(body))
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonSinkStoreUnavailable {
		t.Fatalf("preflight = %d %s, want 503 %s", w.Code, w.Body.String(), reasonSinkStoreUnavailable)
	}
}

// narrowUserInlinePolicy's ownership exemption carries the fault out, and
// capabilityRunRefusal answers it with the fixed 503, not a 500 (#3506).
func TestNarrowUserInlinePolicy_OwnListFaultIs503NotServerError(t *testing.T) {
	srv := ownFaultMemberServer(t)
	ctx := withOwnedSecretMemo(withCeilingMemo(govMemberCtx([]string{"eng"}, false)))
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{apiKeyGrant("api.anthropic.com", "alice-model-key")}}
	_, _, err := srv.narrowUserInlinePolicy(ctx, "sub-gov-bob", &spec)
	if !errors.Is(err, errSecretStoreUnavailable) {
		t.Fatalf("narrow = %v, want %v", err, errSecretStoreUnavailable)
	}
	ref := capabilityRunRefusal(err)
	if ref.err != nil || ref.status != http.StatusServiceUnavailable || ref.body.Reason != reasonSinkStoreUnavailable {
		t.Fatalf("refusal = %d %+v (server error %v), want 503 %s", ref.status, ref.body, ref.err, reasonSinkStoreUnavailable)
	}
}

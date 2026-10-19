// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

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

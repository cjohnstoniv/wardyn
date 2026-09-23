// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// perUserDisconnectSrv is a per_user deployment with TWO captured AWS SSO
// sessions already stored — the admin's own and a member's, each in its own
// namespace, which is where storeAWSSSOBlob puts every capture under per_user.
func perUserDisconnectSrv(t *testing.T, admin, member string) (*Server, *memSecrets) {
	t.Helper()
	h := newHarness(t)
	st := &integStore{
		govEscapeStore: newGovEscapeStore(&capStore{}),
		site: agentRoster(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: perUserPortal,
		}),
	}
	sec := &memSecrets{m: map[string][]byte{}}
	for _, owner := range []string{admin, member} {
		blob := []byte(`{"access_token":"` + owner + `-token","start_url":"` + perUserPortal +
			`","region":"us-east-1","account_id":"123456789012","role_name":"R","expires_at":"2100-01-01T00:00:00Z"}`)
		if err := sec.For(owner).Put(t.Context(), harnessCredSecretName(awsSSOProvider), blob); err != nil {
			t.Fatalf("seed %s: %v", owner, err)
		}
	}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.OIDC = &oidc.Authenticator{}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	srv := New(cfg)
	h.srv = srv
	return srv, sec
}

// TestHandleHarnessDisconnect_PerUserDeletesTheCallersOwnBlob pins that a
// per-user Disconnect is scoped to the caller, not the whole estate.
//
// Under per_user EVERY capture — the admin's included — lives in For(subject),
// so an unscoped Delete would remove nothing anybody had captured and still
// answer {"captured": false}: an operator's own Disconnect would be a silent
// no-op on a per-user estate.
//
// The route tier stays operatorOnly, so this revokes the CALLER's own session
// and nobody else's. A member's stored session is superseded by their next
// sign-in, ends at the IdP when an admin revokes the session there, and expires
// with its registration — docs/OPERATIONS.md's "AWS SSO per-user" subsection and
// the THREAT-MODEL residency row say so, and name a self-service member
// Disconnect as a 0.8 item.
func TestHandleHarnessDisconnect_PerUserDeletesTheCallersOwnBlob(t *testing.T) {
	// The namespace selector is the SESSION SUBJECT (withHumanIdentity stores
	// Sub, and principalFromRequest returns it) — the same value storeAWSSSOBlob
	// scoped the capture with.
	const admin, member = "sub-admin", "sub-member"
	srv, sec := perUserDisconnectSrv(t, admin, member)

	w := doSSO(t, srv, http.MethodDelete, "/api/v1/setup/harness-credential/aws",
		ssoSession(t, admin, "admin@corp.example", oidc.RoleAdmin), "")
	if w.Code != http.StatusOK {
		t.Fatalf("disconnect: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp["captured"] != false {
		t.Errorf("resp = %v, want captured:false", resp)
	}
	if _, ok := sec.owned[admin][harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("the caller's OWN per_user session survived their Disconnect — the delete is still unscoped")
	}
	// And nobody else's moved: this route revokes the caller's own, nothing more.
	if _, ok := sec.owned[member][harnessCredSecretName(awsSSOProvider)]; !ok {
		t.Error("an operator's Disconnect deleted a MEMBER's captured session")
	}
}

// TestHandleHarnessDisconnect_SharedStaysOperatorWide: with no per_user row the
// delete is byte-for-byte where it always was, for every provider.
func TestHandleHarnessDisconnect_SharedStaysOperatorWide(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{
		harnessCredSecretName(awsSSOProvider): []byte(`{"access_token":"operator"}`),
		harnessCredSecretName("anthropic"):    []byte(`{"token":"sk-ant-oat01-existing"}`),
	}}
	_, srv := harnessCredSrv(t, sec)
	for _, provider := range []string{"aws", "anthropic"} {
		if w := do(t, srv, http.MethodDelete, "/api/v1/setup/harness-credential/"+provider, adminToken, ""); w.Code != http.StatusOK {
			t.Fatalf("%s disconnect: code = %d, want 200; body=%s", provider, w.Code, w.Body.String())
		}
	}
	if len(sec.m) != 0 {
		t.Errorf("shared/legacy disconnect left %v in the operator namespace", sec.m)
	}
}

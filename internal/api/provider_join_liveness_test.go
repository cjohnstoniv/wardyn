// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestProviderLiveness_CredentialClassification is #532's reason, per kind and
// state: model_credential rides only on a refusal the person's own sign-in or
// stored key repairs, because the console answers that reason with the
// provider's door and a relaunch. Every other refusal is repaired by something
// no sign-in does (an admin, an install fix, time), so it carries none.
func TestProviderLiveness_CredentialClassification(t *testing.T) {
	const owner = "live-owner@example.com"
	key := types.ModelProvider{ID: "k", UID: "uid-k", Kind: types.ModelProviderAnthropicAPIKey,
		Harnesses: []types.ProviderHarness{{Harness: "claude-code"}}}
	endpoint := key
	endpoint.Kind, endpoint.BaseURL = types.ModelProviderCustomEndpoint, "https://gw.corp.example"
	sub := subProvider("s")
	bearer, sso := brBearerProvider(), brSSOProvider()
	noRegion := bearer
	noRegion.Bedrock = &types.BedrockSettings{}
	odd := key
	odd.Kind = "a_kind_this_build_never_heard_of"
	later, earlier := time.Now().Add(8*time.Hour), time.Now().Add(-time.Hour)
	renewable := func(expires time.Time) []byte {
		b, _ := json.Marshal(awsSSOBlob{AccessToken: "sso-access-0123456789", RefreshToken: "sso-refresh-0123456789",
			ClientID: "client", ClientSecret: "client-secret-0123456789", StartURL: brStartURL, Region: "us-east-1",
			AccountID: "123456789012", RoleName: "BedrockUser", ExpiresAt: expires, CapturedAt: time.Now().Add(-2 * time.Hour),
			RegistrationExpiresAt: time.Now().Add(90 * 24 * time.Hour)})
		return b
	}
	otherPortal, _ := json.Marshal(awsSSOBlob{AccessToken: "tok", StartURL: "https://other.awsapps.com/start", Region: "us-east-1",
		AccountID: "123456789012", RoleName: "BedrockUser", ExpiresAt: later})
	for _, tc := range []struct {
		name       string
		p          types.ModelProvider
		owner      string
		own        map[string][]byte // part → the owner's own row
		setup      func(*Server)
		aws        string // the fake AWS token endpoint's error code; "" = never called
		refresh    bool
		state      string // "" = live
		credential bool
	}{
		{name: "key: not stored", p: key, state: mpRunNoKey, credential: true},
		{name: "endpoint: not stored", p: endpoint, state: mpRunNoToken, credential: true},
		{name: "key: the admin token", p: key, owner: adminTokenPrincipal, state: mpcNoPerson},
		{name: "key: stored", p: key, own: map[string][]byte{providerKeyPart: []byte("sk-own")}},
		{name: "subscription: not signed in", p: sub, state: mpSubNotSignedIn, credential: true},
		{name: "subscription: no sign-in image", p: sub, setup: func(s *Server) { s.cfg.AgentImages = nil }, state: mpSubNoImage},
		{name: "subscription: the admin token", p: sub, owner: adminTokenPrincipal, state: mpSubNotPerson},
		{name: "subscription: no secret store", p: sub, setup: func(s *Server) { s.cfg.Secrets = nil }, state: mpSubNoStore},
		{name: "subscription: signed in", p: sub, own: map[string][]byte{providerOAuthPart: subBlob("tok")}},
		{name: "bedrock key: not stored", p: bearer, state: mpRunNoKey, credential: true},
		{name: "bedrock key: blank", p: bearer, own: map[string][]byte{providerKeyPart: []byte("  ")}, state: mpRunNoKey, credential: true},
		{name: "bedrock key: the admin token", p: bearer, owner: adminTokenPrincipal, state: mpBRNotPerson},
		{name: "bedrock key: no secret store", p: bearer, setup: func(s *Server) { s.cfg.Secrets = nil }, state: mpBRNoStore},
		{name: "bedrock key: no region or model", p: noRegion, state: fmt.Sprintf(mpBRUnset, "claude-code")},
		{name: "aws sign-in: not signed in", p: sso, state: mpBRNotSignedIn, credential: true},
		{name: "aws sign-in: another pinned account", p: sso,
			own:   map[string][]byte{providerSSOPart: brBlob("tok", "222222222222", "BedrockUser", later)},
			state: fmt.Sprintf(mpBRPinned, "222222222222", "BedrockUser", "123456789012", "BedrockUser"), credential: true},
		{name: "aws sign-in: another access portal", p: sso, own: map[string][]byte{providerSSOPart: otherPortal},
			state: mpBRPortal, credential: true},
		{name: "aws sign-in: lapsed, not renewable", p: sso,
			own:   map[string][]byte{providerSSOPart: brBlob("tok", "123456789012", "BedrockUser", earlier)},
			state: mpBRNotSignedIn, credential: true},
		{name: "aws sign-in: lapsed, AWS spent the refresh token", p: sso, refresh: true, aws: "invalid_grant",
			own: map[string][]byte{providerSSOPart: renewable(earlier)}, state: mpBRNotSignedIn, credential: true},
		{name: "aws sign-in: lapsed, AWS did not answer the renewal", p: sso, refresh: true, aws: "slow_down",
			own: map[string][]byte{providerSSOPart: renewable(earlier)}, state: mpBRRenewing},
		{name: "aws sign-in: live", p: sso, own: map[string][]byte{providerSSOPart: brBlob("tok", "123456789012", "BedrockUser", later)}},
		{name: "a kind with no arm", p: odd, state: fmt.Sprintf(mpRunStateNotServing, "claude-code")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(tc.p)}, &capStore{}, nil)
			srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
			srv.cfg.Runner = nil
			who := owner
			if tc.owner != "" {
				who = tc.owner
			}
			for part, v := range tc.own {
				_ = srv.cfg.Secrets.For(who).Put(context.Background(), providerSecretName(tc.p.UID, part), v)
			}
			if tc.aws != "" {
				fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": tc.aws})
				})
			}
			if tc.setup != nil {
				tc.setup(srv)
			}
			_, d, err := srv.providerLiveness(context.Background(), tc.p, "claude-code", who, tc.refresh)
			if err != nil {
				t.Fatalf("liveness: %v", err)
			}
			if tc.state == "" {
				if d != (providerDenial{}) {
					t.Fatalf("denial = %+v, want live", d)
				}
				return
			}
			if !strings.Contains(d.msg, tc.state) || d.credential != tc.credential {
				t.Errorf("denial = %+v\nwant a refusal naming %q with credential=%v", d, tc.state, tc.credential)
			}
			// The class and the remedy travel together: a credential refusal
			// sends the person to the connect door, and nothing else does.
			if d.credential != strings.Contains(d.msg, mpRunRemedySignIn) {
				t.Errorf("credential=%v but the sentence's remedy disagrees: %q", d.credential, d.msg)
			}
		})
	}
}

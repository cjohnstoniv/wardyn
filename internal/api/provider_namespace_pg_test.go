// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ModelCredNeverFromOperatorNamespace is multi-provider rule 10 against
// a REAL secretstore/pg.Store, whose For(owner).Get falls back to the
// operator's row: for every provider credential kind, with the operator's and
// bob's rows stored under the very name alice's own would use, alice's run is
// served her own credential while she has one, and once she has none every
// read — ownSecret / readAWSSSOBlob, the liveness every door makes, dispatch
// and the injection sink — reads absent and never serves either row.
func TestPG_ModelCredNeverFromOperatorNamespace(t *testing.T) {
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatalf("secretstore/pg.New: %v", err)
	}
	const alice, bob = subOwner, "bob@example.com"
	later := time.Now().Add(8 * time.Hour)
	key := func(who string) []byte { return []byte("key-of-" + who + "-0123456789") }
	oauth := func(who string) []byte { return subBlob("oat-of-" + who) }
	sso := func(who string) []byte { return brBlob("sso-of-"+who, "123456789012", "BedrockUser", later) }
	endpoint := types.ModelProvider{ID: "corp-gw", UID: uuid.NewString(), Kind: types.ModelProviderCustomEndpoint,
		BaseURL: "https://gw.corp.example", Auth: &types.ProviderAuth{Header: "Authorization", Format: "Bearer %s"},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Path: "/anthropic"}}}

	for _, tc := range []struct {
		name  string
		p     types.ModelProvider
		agent string
		part  string
		cred  func(who string) []byte
		token string // the credential string of "of-<who>" the sink would serve
		// refusal is the sink's answer once alice has none: 424 "not stored",
		// or for an AWS sign-in the 423 hold that asks alice herself to sign in.
		refusal int
	}{
		{"anthropic key", mpKeyProvider("anthropic", uuid.NewString(), types.ModelProviderAnthropicAPIKey,
			types.ProviderHarness{Harness: "claude-code"}), "claude-code", providerKeyPart, key, "key-of-", http.StatusFailedDependency},
		{"openai key", mpKeyProvider("openai", uuid.NewString(), types.ModelProviderOpenAIAPIKey,
			types.ProviderHarness{Harness: "codex-cli"}), "codex-cli", providerKeyPart, key, "key-of-", http.StatusFailedDependency},
		{"custom endpoint token", endpoint, "claude-code", providerKeyPart, key, "key-of-", http.StatusFailedDependency},
		{"claude subscription", subProvider("claude"), "claude-code", providerOAuthPart, oauth, "oat-of-", http.StatusFailedDependency},
		{"bedrock bearer", brBearerProvider(), "claude-code", providerKeyPart, key, "key-of-", http.StatusFailedDependency},
		{"bedrock sso", brSSOProvider(), "claude-code", providerSSOPart, sso, "sso-of-", http.StatusLocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h, st, _ := subHarness(t, tc.p)
			st.Store = store.NewPG(pool) // anything the fake does not answer goes to real Postgres, never a nil Store
			st.run.Agent = tc.agent
			h.srv.cfg.Secrets = sec
			h.srv.cfg.BedrockAWSConfigDir = t.TempDir()
			h.srv.cfg.AWSSSOProxyInject = true
			h.srv.router = h.srv.routes()
			name := providerSecretName(tc.p.UID, tc.part)
			operator := tc.cred("operator")
			if err := sec.Put(ctx, name, operator); err != nil {
				t.Fatalf("seed the operator namespace: %v", err)
			}
			for _, who := range []string{bob, alice} {
				if err := sec.For(who).Put(ctx, name, tc.cred(who)); err != nil {
					t.Fatalf("seed %s: %v", who, err)
				}
			}
			// The hazard this rule exists for, on the real store: a person with
			// no row of their own is handed the operator's by a bare Get.
			if got, err := sec.For("carol@example.com").Get(ctx, name); err != nil || string(got) != string(operator) {
				t.Fatalf("For(carol).Get = %q, %v; want the operator's row (the fallback ownSecret refuses)", got, err)
			}
			leaks := func(body string) bool {
				return strings.Contains(body, tc.token+"operator") || strings.Contains(body, tc.token+bob)
			}

			if _, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil); !ok || len(st.grants) != 1 {
				t.Fatalf("dispatch with alice's own credential: ok=%v grants=%d (failed: %q)", ok, len(st.grants), st.failed)
			}
			grant := st.grants[0]
			if code, body := resolvePGGrant(t, h, st, grant); code != http.StatusOK || !strings.Contains(body, tc.token+alice) || leaks(body) {
				t.Fatalf("sink = %d %s; want alice's own credential and nobody else's", code, body)
			}

			if err := sec.For(alice).Delete(ctx, name); err != nil {
				t.Fatalf("remove alice's own credential: %v", err)
			}
			for _, owner := range []string{alice, ""} {
				raw, found, err := h.srv.ownSecret(ctx, owner, name)
				if tc.part == providerSSOPart {
					var blob awsSSOBlob
					blob, found, err = h.srv.readAWSSSOBlob(ctx, chosenProvider{provider: tc.p, owner: owner}.awsScope())
					raw = []byte(blob.AccessToken)
				}
				if err != nil || found {
					t.Errorf("read for owner %q: found=%v err=%v value=%q; want absent", owner, found, err, raw)
				}
			}
			if _, d, err := h.srv.providerLiveness(ctx, tc.p, tc.agent, alice, false); err != nil || !d.credential {
				t.Errorf("liveness for alice = %+v, %v; want a credential refusal", d, err)
			}
			if code, body := resolvePGGrant(t, h, st, grant); code != tc.refusal || leaks(body) {
				t.Errorf("sink after alice removed hers = %d %s; want %d, serving nobody else's", code, body, tc.refusal)
			}
			// Last: a refused dispatch fails the run, which revokes its token.
			st.grants, st.failed = nil, ""
			if _, ok := dispatchSub(h, st, &types.RunPolicySpec{}, map[string]string{}, nil); ok || len(st.grants) != 0 || st.failed == "" || leaks(st.failed) {
				t.Errorf("dispatch without alice's credential: ok=%v grants=%d failed=%q; want refused", ok, len(st.grants), st.failed)
			}
		})
	}
}

// resolvePGGrant asks the injection sink to resolve g exactly as dispatch
// authored it.
func resolvePGGrant(t *testing.T, h *harness, st *subStore, g types.CredentialGrant) (int, string) {
	t.Helper()
	var scope struct {
		Host, Header, Format string
		SecretName           string `json:"secret_name"`
	}
	if err := json.Unmarshal(g.Spec.Scope, &scope); err != nil {
		t.Fatal(err)
	}
	rule := egress.InjectionRule{Host: scope.Host, Header: scope.Header, Format: scope.Format, SecretName: scope.SecretName}
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-" + g.ID.String(), Injection: &rule}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), h.mintRunToken(t, st.run.ID), "")
	return rr.Code, rr.Body.String()
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestManagedCredProvider(t *testing.T) {
	store := &memSecrets{m: map[string][]byte{}}
	p := NewManagedCredProvider(store, "anthropic")

	// Not connected: fails closed.
	if _, err := p.Current(context.Background()); err == nil {
		t.Fatal("expected error before a token is stored")
	}

	// Store a blob, then it resolves.
	blob, _ := json.Marshal(managedCredBlob{Token: "sk-ant-oat01-real-token"})
	_ = store.Put(context.Background(), harnessCredSecretName("anthropic"), blob)
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Current after store: %v", err)
	}
	if tok.Value != "sk-ant-oat01-real-token" {
		t.Fatalf("wrong token: %q", tok.Value)
	}
	// Managed tokens carry no machine-readable expiry (zero) so the sink treats
	// them as static — no re-resolve churn.
	if !tok.ExpiresAt.IsZero() {
		t.Fatalf("managed token must have zero expiry, got %v", tok.ExpiresAt)
	}

	// Empty token blob == not connected.
	empty, _ := json.Marshal(managedCredBlob{Token: ""})
	_ = store.Put(context.Background(), harnessCredSecretName("anthropic"), empty)
	if _, err := p.Current(context.Background()); err == nil {
		t.Fatal("empty token must fail closed")
	}
}

func TestManagedSentinelCredsAreInert(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(managedSentinelCredsB64())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var d struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if uerr := json.Unmarshal(raw, &d); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if d.ClaudeAiOauth.AccessToken != managedSentinelAccessToken {
		t.Fatalf("access token is not the inert sentinel: %q", d.ClaudeAiOauth.AccessToken)
	}
	if d.ClaudeAiOauth.RefreshToken != "" {
		t.Fatal("sentinel must carry a BLANK refresh token")
	}
	if d.ClaudeAiOauth.ExpiresAt != 4102444800000 {
		t.Fatalf("sentinel expiry must be pinned far out, got %d", d.ClaudeAiOauth.ExpiresAt)
	}
}

func TestHarnessSecretIsReserved(t *testing.T) {
	// Every provider that supports container login must have its stored blob name
	// reserved, so the generic secrets API cannot clobber/list it and the injection
	// sink refuses to resolve it as a raw value. reservedSecret covers the
	// wardyn-harness-*-oauth PATTERN, so a future provider row is sealed
	// automatically — this test guards that the pattern actually matches every
	// name harnessCredSecretName generates.
	for _, agent := range []string{"claude-code"} {
		hl, ok := agentHarnessLogin(agent)
		if !ok {
			continue
		}
		if !reservedSecret(hl.secretName) {
			t.Fatalf("managed harness secret %q (provider %q) is NOT reserved — it would be listable/injectable as a raw value", hl.secretName, hl.provider)
		}
	}
	// The pattern must also seal a hypothetical future provider's blob.
	if !reservedSecret(harnessCredSecretName("codex")) {
		t.Fatal("reservedSecret must cover the wardyn-harness-<provider>-oauth pattern for future providers")
	}
}

func TestManagedOptOut_APIKeyInjectionWins(t *testing.T) {
	// The managed-subscription dispatch gate must stay a FALLBACK: when the run
	// already carries an anthropic api-key injection (the operator chose api-key),
	// managed must NOT fire and silently override it.
	anthropic := []runner.InjectionGrant{{Rule: egress.InjectionRule{Host: "api.anthropic.com"}}}
	if !hasAnthropicAPIKeyInjection(anthropic) {
		t.Fatal("should detect an api.anthropic.com injection")
	}
	// Trailing dot / case should still match (mirrors the sink host check).
	dotted := []runner.InjectionGrant{{Rule: egress.InjectionRule{Host: "API.Anthropic.com."}}}
	if !hasAnthropicAPIKeyInjection(dotted) {
		t.Fatal("host match must normalize case + trailing dot")
	}
	// A non-anthropic injection (e.g. OpenAI) must NOT block managed.
	other := []runner.InjectionGrant{{Rule: egress.InjectionRule{Host: "api.openai.com"}}}
	if hasAnthropicAPIKeyInjection(other) {
		t.Fatal("a non-anthropic injection must not count")
	}
	if hasAnthropicAPIKeyInjection(nil) {
		t.Fatal("no injections must not count")
	}
}

func TestAgentHarnessLogin(t *testing.T) {
	hl, ok := agentHarnessLogin("claude-code")
	if !ok {
		t.Fatal("claude-code must support container login")
	}
	if hl.sentinel != types.ManagedOAuthSecret {
		t.Fatalf("wrong sentinel: %q", hl.sentinel)
	}
	if hl.injectHost != "api.anthropic.com" {
		t.Fatalf("wrong inject host pin: %q", hl.injectHost)
	}
	// Codex has no container-login path in v1.
	if _, ok := agentHarnessLogin("codex-cli"); ok {
		t.Fatal("codex-cli must NOT support container login in v1")
	}
}

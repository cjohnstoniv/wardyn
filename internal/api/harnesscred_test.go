// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

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
	// The stored blob name must be reserved so the generic secrets API cannot
	// clobber/list it and the injection sink refuses to resolve it as a value.
	if !reservedSecretNames[harnessCredSecretName("anthropic")] {
		t.Fatal("managed harness secret name must be in reservedSecretNames")
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

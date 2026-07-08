// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package backends

import "testing"

// Inspect must reuse enabledDefault/needsAPIKey/provider and resolve keys per
// (keyPresent || envFallback). It reports disabled backends too (the live
// registry drops them), so the wizard can surface disabled + needs-key states.
func TestInspect(t *testing.T) {
	cfg := RegistryConfig{
		Backends: []BackendSpec{
			{Name: "anthropic-default", Wire: "anthropic", Model: "claude", APIKeySecret: "anthropic-key"}, // empty transport => "api"
			{Name: "bedrock", Wire: "anthropic", Transport: "bedrock", Model: "claude", Region: "us-east-1"},
			{Name: "claude-cli", Wire: "cli", Transport: "claude", Model: "claude"}, // cli defaults OFF
			{Name: "openai-api", Wire: "openai", Transport: "api", Model: "gpt", APIKeySecret: "openai-key", Enabled: boolPtr(false)},
			{Name: "azure-entra", Wire: "openai", Transport: "azure", Auth: "entra", Model: "gpt", BaseURL: "https://x"},
		},
	}
	// Only anthropic-key is present; openai-key is absent.
	keyPresent := func(name string) bool { return name == "anthropic-key" }

	byName := func(rs []BackendReadiness) map[string]BackendReadiness {
		m := make(map[string]BackendReadiness, len(rs))
		for _, r := range rs {
			m[r.Name] = r
		}
		return m
	}

	// No env fallback.
	got := byName(Inspect(cfg, keyPresent, false))
	if len(got) != 5 {
		t.Fatalf("Inspect returned %d entries, want 5", len(got))
	}

	// Transport normalizes empty => "api" for HTTP wires; carries the tool/mode verbatim otherwise.
	if b := got["anthropic-default"]; !b.Enabled || !b.NeedsKey || !b.KeyResolved || b.Provider != "anthropic" || b.Transport != "api" {
		t.Errorf("anthropic-default = %+v; want enabled+needsKey+resolved, provider anthropic, transport api (normalized)", b)
	}
	if b := got["bedrock"]; !b.Enabled || b.NeedsKey || !b.KeyResolved || b.Transport != "bedrock" {
		t.Errorf("bedrock = %+v; want enabled, no key needed, resolved true, transport bedrock", b)
	}
	if b := got["claude-cli"]; b.Enabled || b.NeedsKey || !b.KeyResolved || b.Provider != "subscription" || b.Transport != "claude" {
		t.Errorf("claude-cli = %+v; want disabled (cli default off), no key, resolved true, provider subscription, transport claude", b)
	}
	if b := got["openai-api"]; b.Enabled || !b.NeedsKey || b.KeyResolved {
		t.Errorf("openai-api = %+v; want disabled, needsKey, KeyResolved false (openai-key absent, no env fallback)", b)
	}
	// Azure/entra carries transport + auth through; entra needs no static key.
	if b := got["azure-entra"]; b.Transport != "azure" || b.Auth != "entra" || b.NeedsKey {
		t.Errorf("azure-entra = %+v; want transport azure, auth entra, no static key needed", b)
	}

	// Env fallback flips KeyResolved true for the needs-key backend whose secret is absent.
	got2 := byName(Inspect(cfg, keyPresent, true))
	if b := got2["openai-api"]; !b.KeyResolved {
		t.Errorf("openai-api with env fallback: KeyResolved = %v, want true", b.KeyResolved)
	}
}

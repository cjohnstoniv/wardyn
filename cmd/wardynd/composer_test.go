// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeComposerSecrets is a minimal secretstore.Store for the composer-registry
// tests: only Get is ever called by buildComposerRegistry/
// composerSpecFromIntegrations, so every other method is the embedded nil
// interface's (never invoked; a call would nil-panic, which is the point —
// it documents these paths never touch them).
type fakeComposerSecrets struct {
	secretstore.Store
	m map[string][]byte
}

func (f *fakeComposerSecrets) Get(_ context.Context, name string) ([]byte, error) {
	if v, ok := f.m[name]; ok {
		return v, nil
	}
	return nil, secretstore.ErrNotFound
}

func aiIntegration(id, typ string, defaultFor ...string) types.Integration {
	return types.Integration{ID: id, Category: types.IntegrationAIProvider, Type: typ, DefaultFor: defaultFor}
}

// TestBuildComposerRegistry_ZeroIntegrations_NilRegistry pins the literal
// hard-requirement: WARDYN_COMPOSER_CONFIG unset AND no eligible Integration
// keeps TODAY's boot behavior — nil registry, no error, and (per
// buildOptionalFeatures' wiring) the compose endpoints then 404 honestly.
func TestBuildComposerRegistry_ZeroIntegrations_NilRegistry(t *testing.T) {
	reg, readiness, err := buildComposerRegistry("", &fakeComposerSecrets{}, types.SiteConfig{}, "", "")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if reg != nil {
		t.Fatalf("registry = %+v, want nil", reg)
	}
	if readiness != nil {
		t.Fatalf("readiness = %+v, want nil", readiness)
	}
}

// TestBuildComposerRegistry_ConfigSetWinsWholesale: WARDYN_COMPOSER_CONFIG
// (cfgVal) set must win WHOLESALE — early return, no merging with
// Integrations — even when a SiteConfig carries an eligible
// DefaultFor:wardyn_features integration that would otherwise resolve to a
// COMPLETELY DIFFERENT backend.
func TestBuildComposerRegistry_ConfigSetWinsWholesale(t *testing.T) {
	sc := types.SiteConfig{Integrations: []types.Integration{
		aiIntegration("would-be-derived", "openai_api_key", "wardyn_features"),
	}}
	cfgVal := `{"default":"from-config","backends":[{"name":"from-config","wire":"fake"}]}`
	reg, _, err := buildComposerRegistry(cfgVal, &fakeComposerSecrets{}, sc, "", "")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if reg == nil {
		t.Fatal("registry = nil, want the -composer-config-built registry")
	}
	if got := reg.Default(); got != "from-config" {
		t.Errorf("default = %q, want from-config (the Integration must never be merged in)", got)
	}
	list := reg.List()
	if len(list) != 1 || list[0].Name != "from-config" {
		t.Errorf("backends = %+v, want exactly the one -composer-config backend", list)
	}
}

// TestWardynFeaturesBackendSpec_Mapping pins Task 3's mapping table.
func TestWardynFeaturesBackendSpec_Mapping(t *testing.T) {
	cases := []struct {
		name       string
		in         types.Integration
		wantWire   string
		wantTransp string
		wantOK     bool
	}{
		{"anthropic_api_key -> anthropic", types.Integration{Type: "anthropic_api_key"}, "anthropic", "", true},
		{"openai_api_key -> openai", types.Integration{Type: "openai_api_key"}, "openai", "", true},
		{"azure_openai -> openai/azure", types.Integration{Type: "azure_openai"}, "openai", "azure", true},
		{
			"bedrock -> anthropic/bedrock", types.Integration{Type: "bedrock",
				Config: mustJSONForTest(map[string]any{"region": "us-east-1"})}, "anthropic", "bedrock", true,
		},
		{
			"anthropic_subscription managed -> sandbox",
			types.Integration{Type: "anthropic_subscription", Config: mustJSONForTest(map[string]any{"lane": "managed"})},
			"sandbox", "", true,
		},
		{
			"anthropic_subscription unset lane -> sandbox (managed default)",
			types.Integration{Type: "anthropic_subscription"}, "sandbox", "", true,
		},
		{
			// PLATFORM-API-6: resident_host has no mapping — WardynFeaturesBackend
			// can never select a row whose lane is resident_host in the first
			// place (subscriptionCaps hard-codes its wardyn_features cell OFF), so
			// it falls to the same "sandbox" default every other lane value gets.
			"anthropic_subscription resident_host -> sandbox (no dead cli mapping)",
			types.Integration{Type: "anthropic_subscription", Config: mustJSONForTest(map[string]any{"lane": "resident_host"})},
			"sandbox", "", true,
		},
		{"unmapped type -> not ok", types.Integration{Type: "git_host"}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, ok := wardynFeaturesBackendSpec(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if spec.Wire != c.wantWire || spec.Transport != c.wantTransp {
				t.Errorf("spec = %+v, want wire=%s transport=%s", spec, c.wantWire, c.wantTransp)
			}
		})
	}
}

// TestBuildComposerRegistry_DerivesFromIntegration_AnthropicAPIKey is the
// end-to-end "config unset, one eligible integration" branch: a stored
// anthropic_api_key integration marked DefaultFor:wardyn_features, with its
// secret actually present, must produce a live one-backend registry.
func TestBuildComposerRegistry_DerivesFromIntegration_AnthropicAPIKey(t *testing.T) {
	sc := types.SiteConfig{Integrations: []types.Integration{
		{
			ID: "acme-anthropic", Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
			Credentials: map[string]string{"api_key": "acme-anthropic-key"},
			Config:      mustJSONForTest(map[string]any{"model": "claude-sonnet-4-5"}),
			DefaultFor:  []string{"wardyn_features"},
		},
	}}
	secrets := &fakeComposerSecrets{m: map[string][]byte{"acme-anthropic-key": []byte("sk-acme")}}
	reg, _, err := buildComposerRegistry("", secrets, sc, "", "")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if reg == nil {
		t.Fatal("registry = nil, want a derived one-backend registry")
	}
	list := reg.List()
	if len(list) != 1 || list[0].Name != "acme-anthropic" || list[0].Provider != "anthropic" {
		t.Errorf("backends = %+v, want exactly the derived acme-anthropic/anthropic backend", list)
	}
}

// TestBuildComposerRegistry_NotMarkedDefault_StaysNil: a stored ai_provider
// integration that is NOT marked DefaultFor:wardyn_features must never be
// picked — mirrors "zero eligible integrations" even though one is stored.
func TestBuildComposerRegistry_NotMarkedDefault_StaysNil(t *testing.T) {
	sc := types.SiteConfig{Integrations: []types.Integration{
		{
			ID: "acme-anthropic", Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
			Credentials: map[string]string{"api_key": "acme-anthropic-key"},
			// no DefaultFor
		},
	}}
	secrets := &fakeComposerSecrets{m: map[string][]byte{"acme-anthropic-key": []byte("sk-acme")}}
	reg, _, err := buildComposerRegistry("", secrets, sc, "", "")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if reg != nil {
		t.Fatalf("registry = %+v, want nil (integration is not marked as the wardyn_features default)", reg)
	}
}

// TestBuildComposerRegistry_MisconfiguredDerivedIntegration_DegradesToNil:
// capabilitiesFor reports azure_openai's wardyn_features cell as
// UNCONDITIONALLY available (integrations.go), so an operator could mark one
// DefaultFor:wardyn_features with no api_key secret configured at all. The
// derived spec then can't resolve a key — buildRegistryFromConfig errors —
// and that must degrade to "no composer" rather than hard-failing wardynd's
// boot the way a hand-authored -composer-config typo rightly does.
func TestBuildComposerRegistry_MisconfiguredDerivedIntegration_DegradesToNil(t *testing.T) {
	sc := types.SiteConfig{Integrations: []types.Integration{
		aiIntegration("acme-azure", "azure_openai", "wardyn_features"), // no Credentials at all
	}}
	reg, readiness, err := buildComposerRegistry("", &fakeComposerSecrets{}, sc, "", "")
	if err != nil {
		t.Fatalf("err = %v, want nil (must degrade, not hard-fail boot)", err)
	}
	if reg != nil || readiness != nil {
		t.Fatalf("registry/readiness = %+v/%+v, want nil/nil", reg, readiness)
	}
}

// mustJSONForTest is a local json-marshal-or-panic helper (mirrors the
// package's own mustJSON precedent in internal/api, kept local since this
// package has no equivalent already).
func mustJSONForTest(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

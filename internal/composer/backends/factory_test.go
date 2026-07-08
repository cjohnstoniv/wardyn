// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package backends

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

func boolPtr(b bool) *bool { return &b }

func TestNewFromSpec_FakeBackend(t *testing.T) {
	c, err := NewFromSpec(BackendSpec{Name: "f", Wire: "fake", Model: "test"}, "")
	if err != nil {
		t.Fatalf("fake: %v", err)
	}
	p, err := c.Propose(context.Background(), composer.ComposeRequest{Prompt: "anything"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if p.Run.Agent == "" || p.InlinePolicy.MinConfinementClass == "" {
		t.Errorf("fake proposal should be a sane non-empty setup: %+v", p)
	}
}

func TestNewFromSpec_UnknownWire(t *testing.T) {
	if _, err := NewFromSpec(BackendSpec{Name: "x", Wire: "bogus"}, ""); err == nil {
		t.Errorf("unknown wire should error")
	}
}

func TestBuildRegistry_ResolvesKeysAndDefaults(t *testing.T) {
	var resolved []string
	resolveKey := func(spec BackendSpec) (string, error) {
		resolved = append(resolved, spec.Name)
		return "resolved-key", nil
	}
	cfg := RegistryConfig{
		Default: "openai-api",
		Backends: []BackendSpec{
			{Name: "openai-api", Wire: "openai", Transport: "api", Model: "gpt", APIKeySecret: "openai-key"},
			{Name: "bedrock", Wire: "anthropic", Transport: "bedrock", Model: "anthropic.claude", Region: "us-east-1"},
			{Name: "local", Wire: "fake", Model: "qwen"},
		},
	}
	reg, warnings, err := BuildRegistry(cfg, resolveKey)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !reg.Enabled() || reg.Default() != "openai-api" {
		t.Fatalf("default=%q enabled=%v", reg.Default(), reg.Enabled())
	}
	// Only the openai api backend needs a key; bedrock (SigV4) and fake do not.
	if len(resolved) != 1 || resolved[0] != "openai-api" {
		t.Errorf("expected only openai-api to resolve a key, got %v", resolved)
	}
	_ = warnings
	if len(reg.List()) != 3 {
		t.Errorf("want 3 backends, got %d", len(reg.List()))
	}
}

func TestBuildRegistry_CLIDefaultsDisabledWithWarning(t *testing.T) {
	cfg := RegistryConfig{
		Default: "api",
		Backends: []BackendSpec{
			{Name: "api", Wire: "fake", Model: "m"},
			{Name: "claude-sub", Wire: "cli", Transport: "claude", Model: "opus"}, // no enabled -> off
		},
	}
	reg, warnings, err := BuildRegistry(cfg, func(BackendSpec) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// CLI backend defaults disabled -> not in the registry.
	for _, b := range reg.List() {
		if b.Name == "claude-sub" {
			t.Errorf("cli backend should default disabled")
		}
	}
	if !hasSub(warnings, "disabled") {
		t.Errorf("expected a disabled warning, got %v", warnings)
	}
}

func TestBuildRegistry_CLIEnabledEmitsToSWarning(t *testing.T) {
	cfg := RegistryConfig{
		Default: "claude-sub",
		Backends: []BackendSpec{
			{Name: "claude-sub", Wire: "cli", Transport: "claude", Model: "opus", Enabled: boolPtr(true)},
		},
	}
	_, warnings, err := BuildRegistry(cfg, func(BackendSpec) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasSub(warnings, "ToS") && !hasSub(warnings, "subscription") {
		t.Errorf("expected a subscription ToS warning, got %v", warnings)
	}
}

func TestBuildRegistry_KeyResolutionFailureIsFatal(t *testing.T) {
	cfg := RegistryConfig{
		Default: "openai",
		Backends: []BackendSpec{
			{Name: "openai", Wire: "openai", Transport: "api", Model: "gpt", APIKeySecret: "missing"},
		},
	}
	_, _, err := BuildRegistry(cfg, func(BackendSpec) (string, error) {
		return "", context.DeadlineExceeded // any error
	})
	if err == nil {
		t.Errorf("a backend that needs a key but can't resolve one must fail closed")
	}
}

func hasSub(xs []string, sub string) bool {
	for _, x := range xs {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

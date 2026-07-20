// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package backends constructs composer.Composer implementations from operator
// config (the registry of LLM backends). Each provider wire lives in its own
// subpackage (anthropic, openai, cli); this package is the thin factory that maps
// a declarative BackendSpec onto them, plus a deterministic "fake" backend for
// dev / e2e.
package backends

import (
	"errors"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/anthropic"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/cli"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/openai"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/sandbox"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// BackendSpec is one entry in the operator's composer registry config (JSON).
// Credentials are referenced, never inlined: api_key_secret names a secret in the
// at-rest secret store (cmd/wardynd resolves it, with an env fallback) — the spec
// itself carries no secret value.
type BackendSpec struct {
	Name      string `json:"name"`
	Wire      string `json:"wire"`      // "anthropic" | "openai" | "cli" | "fake"
	Transport string `json:"transport"` // anthropic: api|bedrock; openai: api|azure|compatible; cli: claude|codex
	Model     string `json:"model"`

	BaseURL string `json:"base_url,omitempty"` // openai azure/compatible
	Region  string `json:"region,omitempty"`   // anthropic bedrock
	Auth    string `json:"auth,omitempty"`     // openai azure: apikey|entra

	APIKeySecret string `json:"api_key_secret,omitempty"` // secret-store name (HTTP key backends)

	BinPath        string `json:"bin_path,omitempty"`        // cli
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // cli / sandbox

	MaxAttempts int   `json:"max_attempts,omitempty"`
	Enabled     *bool `json:"enabled,omitempty"` // default true; cli default false (subscription ToS)
}

// RegistryConfig is the whole composer config: a default backend name + the set
// of configured backends.
type RegistryConfig struct {
	Default  string        `json:"default"`
	Backends []BackendSpec `json:"backends"`
}

// needsAPIKey reports whether a backend authenticates with a static API key
// (vs cloud IAM / subscription / dummy), so cmd/wardynd knows to resolve one.
func (s BackendSpec) needsAPIKey() bool {
	switch s.Wire {
	case "anthropic":
		return s.Transport == "" || s.Transport == "api"
	case "openai":
		if s.Transport == "azure" {
			return s.Auth == "apikey"
		}
		return s.Transport == "" || s.Transport == "api" || s.Transport == "compatible"
	default:
		return false
	}
}

// enabledDefault returns the effective enabled flag: cli backends default OFF
// (subscription ToS / rate limits — opt-in), everything else defaults ON.
func (s BackendSpec) enabledDefault() bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return s.Wire != "cli"
}

// normalizedTransport is the effective transport for display: HTTP wires default
// to "api" (what NewFromSpec resolves via orDefault); cli carries the tool
// (claude|codex) and fake the variant, both verbatim.
func (s BackendSpec) normalizedTransport() string {
	switch s.Wire {
	case "anthropic", "openai":
		return orDefault(s.Transport, "api")
	default:
		return s.Transport
	}
}

// provider is the display provider for the UI picker. Both subscription-backed
// wires (host cli, container-mode sandbox) present as "subscription".
func (s BackendSpec) provider() string {
	if s.Wire == "cli" || s.Wire == "sandbox" {
		return "subscription"
	}
	return s.Wire
}

// NewFromSpec builds one Composer from a spec and an already-resolved API key
// (empty when the backend needs none). It is wire/transport-dispatch only.
func NewFromSpec(spec BackendSpec, apiKey string) (composer.Composer, error) {
	switch spec.Wire {
	case "anthropic":
		return anthropic.NewComposer(anthropic.Config{
			Transport: orDefault(spec.Transport, "api"), Model: spec.Model,
			APIKey: apiKey, Region: spec.Region, MaxAttempts: spec.MaxAttempts,
		})
	case "openai":
		return openai.NewComposer(openai.Config{
			Transport: orDefault(spec.Transport, "api"), Model: spec.Model,
			APIKey: apiKey, BaseURL: spec.BaseURL, AzureAuth: spec.Auth, MaxAttempts: spec.MaxAttempts,
		})
	case "cli":
		timeout := time.Duration(spec.TimeoutSeconds) * time.Second
		return cli.NewComposer(cli.Config{
			Tool: spec.Transport, Model: spec.Model, BinPath: spec.BinPath,
			Timeout: timeout, MaxAttempts: spec.MaxAttempts,
		})
	case "sandbox":
		// Container-mode subscription composer: runs the real claude inside a
		// governed run (managed token injected proxy-side). Its run launcher is
		// late-bound by the Server after construction (SetRunClaude).
		return sandbox.New(sandbox.Config{Model: spec.Model}), nil
	case "fake":
		return newFake(spec.Model, spec.Transport), nil
	default:
		return nil, fmt.Errorf("composer: unknown backend wire %q", spec.Wire)
	}
}

// BuildRegistry constructs a *composer.Registry from RegistryConfig. resolveKey
// resolves a backend's API key (secret-store name -> value, with the caller's env
// fallback baked in); it is only called for backends that needsAPIKey(). Disabled
// backends are skipped. It returns the registry plus human-readable warnings
// (e.g. the subscription ToS notice) for the operator log.
func BuildRegistry(cfg RegistryConfig, resolveKey func(spec BackendSpec) (string, error)) (*composer.Registry, []string, error) {
	var entries []composer.RegistryEntry
	var warnings []string
	for _, spec := range cfg.Backends {
		if spec.Name == "" || spec.Wire == "" {
			return nil, nil, errors.New("composer: backend requires name and wire")
		}
		if !spec.enabledDefault() {
			warnings = append(warnings, fmt.Sprintf("composer backend %q is configured but disabled", spec.Name))
			continue
		}
		if spec.Wire == "cli" {
			warnings = append(warnings, fmt.Sprintf(
				"composer backend %q uses a CLI SUBSCRIPTION (%s): rate-limited and intended for interactive use — prefer an API key for automation; the Codex subscription is not permitted on public repos per OpenAI ToS",
				spec.Name, spec.Transport))
		}
		var apiKey string
		if spec.needsAPIKey() {
			k, err := resolveKey(spec)
			if err != nil {
				return nil, nil, fmt.Errorf("composer backend %q: %w", spec.Name, err)
			}
			apiKey = k
		}
		c, err := NewFromSpec(spec, apiKey)
		if err != nil {
			return nil, nil, fmt.Errorf("composer backend %q: %w", spec.Name, err)
		}
		entries = append(entries, composer.RegistryEntry{
			Info:     composer.BackendInfo{Name: spec.Name, Provider: spec.provider(), Model: spec.Model},
			Composer: c,
		})
	}
	reg, err := composer.NewRegistry(cfg.Default, entries)
	return reg, warnings, err
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// BackendReadiness is the BOOT-snapshot readiness of one configured backend for
// the first-run setup surface. Unlike the live registry (which drops disabled
// and unresolvable backends), it reports every configured backend so the wizard
// can show disabled + needs-key states. KeySecret is a secret NAME, never a
// value.
type BackendReadiness struct {
	Name        string
	Provider    string
	Model       string
	Wire        string
	Transport   string // normalized (HTTP wires default to "api"); cli tool / fake variant verbatim
	Auth        string // openai azure only: apikey|entra (empty elsewhere)
	Enabled     bool
	NeedsKey    bool
	KeySecret   string
	KeyResolved bool
}

// Inspect returns a readiness snapshot for every configured backend, reusing the
// same enabledDefault()/needsAPIKey()/provider() rules the registry builder uses.
// keyPresent reports whether a named secret resolves in the store (it must return
// false for an empty name); envFallback is whether WARDYN_COMPOSER_API_KEY is set.
// KeyResolved = needsKey ? (keyPresent(secret) || envFallback) : true.
func Inspect(cfg RegistryConfig, keyPresent func(secretName string) bool, envFallback bool) []BackendReadiness {
	out := make([]BackendReadiness, 0, len(cfg.Backends))
	for _, spec := range cfg.Backends {
		needsKey := spec.needsAPIKey()
		resolved := true
		if needsKey {
			resolved = keyPresent(spec.APIKeySecret) || envFallback
		}
		out = append(out, BackendReadiness{
			Name:        spec.Name,
			Provider:    spec.provider(),
			Model:       spec.Model,
			Wire:        spec.Wire,
			Transport:   spec.normalizedTransport(),
			Auth:        spec.Auth,
			Enabled:     spec.enabledDefault(),
			NeedsKey:    needsKey,
			KeySecret:   spec.APIKeySecret,
			KeyResolved: resolved,
		})
	}
	return out
}

// newFake returns a deterministic Composer for local dev and the e2e seeded
// backend (no network, no keys). It never reads the prompt for grading (advisory
// + Wardyn re-grades anyway). The variant (from the spec's transport field) selects:
//   - "high":      a deliberately HIGH-risk CC1 setup so the UI acknowledgment
//     gate can be exercised end to end;
//   - "interview": the least-privilege CC2 setup PLUS the interactive clarify step
//     (asks one round of questions, then proposes) so the Q&A flow can be tested;
//   - anything else: a sane least-privilege CC2 setup (one-shot).
func newFake(model, variant string) composer.Composer {
	if variant == "high" {
		return &composer.FakeComposer{Result: composer.Proposal{
			Run: composer.RunInput{Agent: "claude-code", Repo: "acme/widgets", Task: "composed by the fake (high-risk) backend", ConfinementClass: "CC1"},
			InlinePolicy: types.RunPolicySpec{
				AllowedDomains:      []string{"github.com"},
				AllowAllEgress:      true, // clamped off by operator policy; CC1 still grades HIGH
				MinConfinementClass: types.CC1,
				FirstUseApproval:    types.FirstUseDenyWithReview,
				AutoStopAfterSec:    1800,
				EligibleGrants: []types.GrantSpec{
					{Kind: types.GrantGitHubToken, RequiresApproval: false,
						Scope: []byte(`{"repos":["acme/widgets"],"permissions":{"contents":"write"}}`)},
				},
			},
			Summary: fmt.Sprintf("Deterministic HIGH-risk proposal from the fake composer backend (model %q): a Fence run (weakest isolation) — graded high so the acknowledgment gate is exercised.", model),
		}}
	}
	f := &composer.FakeComposer{Result: composer.Proposal{
		Run: composer.RunInput{Agent: "claude-code", Repo: "acme/widgets", Task: "composed by the fake backend", ConfinementClass: "CC2"},
		InlinePolicy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "github.com"},
			MinConfinementClass: types.CC2,
			FirstUseApproval:    types.FirstUseDenyWithReview,
			AutoStopAfterSec:    1800,
			EligibleGrants: []types.GrantSpec{
				{Kind: types.GrantGitHubToken, RequiresApproval: true,
					Scope: []byte(`{"repos":["acme/widgets"],"permissions":{"contents":"read"}}`)},
			},
		},
		Summary: fmt.Sprintf("Deterministic proposal from the fake composer backend (model %q): a least-privilege Wall run with default-deny egress and a read-only GitHub grant.", model),
	}}
	if variant == "interview" {
		f.ClarifyEnabled = true
		f.ClarifyResult = composer.Clarification{
			Ready: false,
			Questions: []composer.Question{{
				ID:       "gh_access",
				Question: "What GitHub access does this task need?",
				Why:      "Determines whether to request a read-only or write-capable token.",
				Options:  []string{"Read-only", "Read + write (open a PR)", "No GitHub access"},
				Multi:    false,
			}},
			Assumptions: []string{"Targeting the acme/widgets repository."},
			Notes:       "A couple of details will sharpen the proposal.",
		}
	}
	return f
}

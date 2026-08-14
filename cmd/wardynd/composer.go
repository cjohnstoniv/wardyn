// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// buildComposerRegistry constructs the AI Run Composer backend registry.
//
// WARDYN_COMPOSER_CONFIG set (cfgVal non-empty) WINS WHOLESALE: it is parsed
// and built exactly as before — no merging with Integrations, ever. Only when
// it is EMPTY does the registry derive from Integrations instead (Task 3): the
// operator-marked DefaultFor: wardyn_features integration whose wardyn_features
// capability reads "available" right now (api.WardynFeaturesBackend). Zero
// eligible integrations returns (nil, nil, nil) — the ORIGINAL "unconfigured"
// behavior — so an operator who never opens the Integrations surface keeps
// exactly today's boot behavior (compose 404s honestly).
//
// Credential resolution (operator decision: secret store + env fallback): a
// backend's api_key_secret is read from the at-rest secret store; if a backend
// needs a key but names no secret (or the lookup fails), WARDYN_COMPOSER_API_KEY is
// used as a fallback. Bedrock/Azure-Entra use cloud credential chains and CLI
// subscriptions use resident CLI creds — neither needs a key here.
func buildComposerRegistry(cfgVal string, secrets secretstore.Store, sc types.SiteConfig, bedrockRegion, bedrockModel string) (*composer.Registry, []backends.BackendReadiness, error) {
	cfgVal = strings.TrimSpace(cfgVal)
	if cfgVal != "" {
		raw, err := composerConfigBytes(cfgVal)
		if err != nil {
			return nil, nil, err
		}
		var cfg backends.RegistryConfig
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return nil, nil, fmt.Errorf("parse composer config: %w", err)
		}
		return buildRegistryFromConfig(cfg, secrets)
	}

	// WARDYN_COMPOSER_CONFIG unset: derive from Integrations (Task 3).
	spec, ok := composerSpecFromIntegrations(secrets, sc, bedrockRegion, bedrockModel)
	if !ok {
		return nil, nil, nil // zero eligible integrations ⇒ nil registry ⇒ compose 404s honestly, unchanged
	}
	reg, readiness, err := buildRegistryFromConfig(backends.RegistryConfig{
		Default: spec.Name, Backends: []backends.BackendSpec{spec},
	}, secrets)
	if err != nil {
		// Best-effort derivation: an under-configured DefaultFor:wardyn_features
		// integration (e.g. an azure_openai row with no api_key secret yet — its
		// wardyn_features cell reads unconditionally available; that's
		// capabilitiesFor's call, not this file's) must never hard-crash boot the
		// way a hand-authored -composer-config typo rightly does. Degrade to "no
		// composer" and say why, loudly, in the log.
		slog.Warn("wardynd: composer registry derived from an Integration is misconfigured; composer disabled",
			slog.String("integration_id", spec.Name), slog.Any("err", err))
		return nil, nil, nil
	}
	return reg, readiness, nil
}

// buildRegistryFromConfig is buildComposerRegistry's shared tail: given an
// already-resolved RegistryConfig (parsed from -composer-config JSON, or
// synthesized from a single derived Integration), resolve API keys, compute
// the boot-snapshot readiness, and build the registry. Extracted verbatim from
// the pre-Task-3 buildComposerRegistry so the WARDYN_COMPOSER_CONFIG-set path
// stays byte-for-byte the original behavior.
func buildRegistryFromConfig(cfg backends.RegistryConfig, secrets secretstore.Store) (*composer.Registry, []backends.BackendReadiness, error) {
	envFallback := strings.TrimSpace(os.Getenv("WARDYN_COMPOSER_API_KEY"))
	resolveKey := func(spec backends.BackendSpec) (string, error) {
		if spec.APIKeySecret != "" && secrets != nil {
			v, gerr := secrets.Get(context.Background(), spec.APIKeySecret)
			if gerr == nil && len(v) > 0 {
				return string(v), nil
			}
			// fall through to env fallback on miss/empty
		}
		if envFallback != "" {
			return envFallback, nil
		}
		return "", fmt.Errorf("no API key: set api_key_secret %q in the secret store or WARDYN_COMPOSER_API_KEY", spec.APIKeySecret)
	}

	// keyPresent mirrors resolveKey's secret lookup (present iff Get returns a
	// non-empty value) so the /setup/status readiness snapshot matches what the
	// registry builder would resolve. It funds the boot-snapshot ONLY — the live
	// registry is still what serves compose requests.
	keyPresent := func(secretName string) bool {
		if secretName == "" || secrets == nil {
			return false
		}
		v, gerr := secrets.Get(context.Background(), secretName)
		return gerr == nil && len(v) > 0
	}
	readiness := backends.Inspect(cfg, keyPresent, envFallback != "")

	reg, warnings, err := backends.BuildRegistry(cfg, resolveKey)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range warnings {
		slog.Warn("wardynd: composer registry warning", slog.String("warning", w))
	}
	return reg, readiness, nil
}

// composerSpecFromIntegrations picks the operator-marked DefaultFor:
// wardyn_features integration (api.WardynFeaturesBackend) and maps it onto the
// backends.BackendSpec shape buildRegistryFromConfig already knows how to
// build a Composer from. ok=false means no eligible integration — the caller
// keeps the registry nil.
//
// secretPresent/managedBlobPresent are built directly from secrets (the same
// store buildRegistryFromConfig resolves keys from) rather than reusing
// api.Server's presentSecretNames/readManagedBlob: this runs at BOOT, before
// the api.Server exists (Config.Composer is late-bound INTO it once built).
func composerSpecFromIntegrations(secrets secretstore.Store, sc types.SiteConfig, bedrockRegion, bedrockModel string) (backends.BackendSpec, bool) {
	secretPresent := func(name string) bool {
		if name == "" || secrets == nil {
			return false
		}
		v, err := secrets.Get(context.Background(), name)
		return err == nil && len(v) > 0
	}
	// managedBlobPresent approximates api.Server.readManagedBlob's "connected"
	// check (harnesscred.go: secret "wardyn-harness-<provider>-oauth", non-empty)
	// without a Server to call it on. A non-empty-bytes check rather than full
	// JSON/token validation is a deliberate, honest approximation: this only
	// gates a BOOT-TIME REGISTRY DERIVATION convenience, not a security decision
	// — the real dispatch-time injection path re-validates the blob properly.
	managedBlobPresent := func(provider string) bool {
		return secretPresent("wardyn-harness-" + provider + "-oauth")
	}
	in, ok := api.WardynFeaturesBackend(sc, secretPresent, bedrockRegion != "", bedrockModel != "", managedBlobPresent)
	if !ok {
		return backends.BackendSpec{}, false
	}
	return wardynFeaturesBackendSpec(in)
}

// wardynFeaturesBackendSpec maps ONE AI-provider Integration onto the
// backends.BackendSpec shape:
//
//	anthropic_api_key      -> wire "anthropic"
//	openai_api_key         -> wire "openai"
//	azure_openai           -> wire "openai", transport "azure" (api-key auth;
//	                          entra is credential-chain-only and has no
//	                          Integration credential slot to carry it)
//	bedrock                -> wire "anthropic", transport "bedrock" (the SAME
//	                          wire the -composer-config path already uses for
//	                          Bedrock; needs no api key)
//	anthropic_subscription -> wire "sandbox" (the container-mode subscription
//	                          composer), regardless of Config.lane.
//
// PLATFORM-API-6: the resident_host lane (the host CLI login, wire "cli") has
// NO mapping here on purpose — deleted, not stubbed. WardynFeaturesBackend
// only ever picks an integration whose wardyn_features capability reads
// "available", and capabilitiesFor's subscriptionCaps hard-codes that cell
// OFF for resident_host with no operator-facing switch that turns it on
// (integrations.go) — so this function can never actually be called with
// one. A mapping (and its "Enabled carries the operator's opt-in through"
// comment) claiming otherwise is worse than none: it reads as a working path
// to whoever wires the real opt-in signal next, when there is nothing here
// to reach it.
//
// ok=false for any other type (there is no sandbox lane to map it to).
func wardynFeaturesBackendSpec(in types.Integration) (backends.BackendSpec, bool) {
	region, _ := in.Config["region"].(string)
	model, _ := in.Config["model"].(string)
	spec := backends.BackendSpec{Name: in.ID, Model: model}
	switch in.Kind {
	case types.IntegrationKindAnthropicAPIKey:
		spec.Wire = "anthropic"
		spec.APIKeySecret = in.RoleSecret("api_key")
	case types.IntegrationKindOpenAIAPIKey:
		spec.Wire = "openai"
		spec.APIKeySecret = in.RoleSecret("api_key")
	case types.IntegrationKindAzureOpenAI:
		spec.Wire, spec.Transport, spec.Auth = "openai", "azure", "apikey"
		spec.APIKeySecret = in.RoleSecret("api_key")
	case types.IntegrationKindBedrock:
		spec.Wire, spec.Transport, spec.Region = "anthropic", "bedrock", region
	case types.IntegrationKindAnthropicSubscription:
		spec.Wire = "sandbox"
	default:
		return backends.BackendSpec{}, false
	}
	return spec, true
}

// composerConfigBytes returns the config JSON: if cfgVal looks like inline JSON
// (starts with '{') it is used directly; otherwise it is treated as a file path.
func composerConfigBytes(cfgVal string) ([]byte, error) {
	if strings.HasPrefix(cfgVal, "{") {
		return []byte(cfgVal), nil
	}
	b, err := os.ReadFile(cfgVal)
	if err != nil {
		return nil, fmt.Errorf("read composer config %q: %w", cfgVal, err)
	}
	if len(b) == 0 {
		return nil, errors.New("composer config file is empty")
	}
	return b, nil
}

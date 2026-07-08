// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// buildComposerRegistry constructs the AI Run Composer backend registry from the
// -composer-config value, which is either a path to a JSON file or inline JSON of
// the shape {"default":"name","backends":[{...}]}. Returns nil when unconfigured
// (the compose endpoints then fail closed / 404).
//
// Credential resolution (operator decision: secret store + env fallback): a
// backend's api_key_secret is read from the at-rest secret store; if a backend
// needs a key but names no secret (or the lookup fails), WARDYN_COMPOSER_API_KEY is
// used as a fallback. Bedrock/Azure-Entra use cloud credential chains and CLI
// subscriptions use resident CLI creds — neither needs a key here.
func buildComposerRegistry(cfgVal string, secrets secretstore.Store) (composer.Registry, []backends.BackendReadiness, error) {
	cfgVal = strings.TrimSpace(cfgVal)
	if cfgVal == "" {
		return nil, nil, nil
	}
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
		log.Printf("wardynd: WARNING %s", w)
	}
	return reg, readiness, nil
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

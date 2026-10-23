// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"
)

// ValidateCLIArtifactBase validates WARDYN_CLI_ARTIFACT_BASE, the internal
// mirror an operator names for the release assets (the installer fetches
// <base>/v<version>/<asset>), and returns its normalized form for
// api.Config.CLIArtifactBase. Empty => ("", nil): no mirror, nothing published.
//
// The value is published on the anonymous /healthz, so it must hold no
// credential: https:// only, no user name or password, no query, no fragment.
// It delegates to the same seven-rule check ValidateDemoVideoBaseURL uses
// rather than growing a second rule set.
func ValidateCLIArtifactBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	norm, err := validateOneLLMGateway("", raw, false)
	if err != nil {
		return "", fmt.Errorf("WARDYN_CLI_ARTIFACT_BASE: %w", err)
	}
	return norm, nil
}

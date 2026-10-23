// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
)

// A runner too old for a run's proxy setting ends the run with its own reason and a hint naming the setting.
func TestProxySettingRefusalIsItsOwnFailure(t *testing.T) {
	err := fmt.Errorf("orchestrator: %w", &remote.ProxySettingError{Setting: "scrub_responses"})
	hint, reason := sandboxCreateFailure(err)
	if reason != reasonRunnerProxySetting || !strings.Contains(hint, "wardyn-runnerd is too old for scrub_responses; upgrade it") {
		t.Fatalf("hint %q reason %q", hint, reason)
	}
}

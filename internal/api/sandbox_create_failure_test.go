// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A runner too old for a run's proxy setting ends the run with its own reason and a hint naming the setting.
func TestProxySettingRefusalIsItsOwnFailure(t *testing.T) {
	err := fmt.Errorf("orchestrator: %w", &remote.ProxySettingError{Setting: "scrub_responses"})
	hint, reason := sandboxCreateFailure(err)
	if reason != reasonRunnerProxySetting || !strings.Contains(hint, "wardyn-runnerd is too old for scrub_responses; upgrade it") {
		t.Fatalf("hint %q reason %q", hint, reason)
	}
}

// A run refused because its disk drive is in another macOS VM fails with the runner's sentence as written:
// no wire prefix and no "could not be created" lead-in.
func TestDiskDriveHeldElsewhereFailsWithItsSentence(t *testing.T) {
	want := runner.MacVMDiskLocked("Notes")
	err := fmt.Errorf("orchestrator: %w", runnerwire.ErrorFor(errors.New(want)).Err())
	if hint, reason := sandboxCreateFailure(err); hint != want || reason != "sandbox_create" {
		t.Fatalf("hint %q reason %q", hint, reason)
	}
}

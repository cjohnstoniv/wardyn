// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The "none" placeholder stays out of the launcher's harness list; the real harnesses and the start mark ride on.
func TestApplyHarnessSetEnv_OmitsTheNonePlaceholder(t *testing.T) {
	env := map[string]string{}
	applyHarnessSetEnv(env, []types.RunHarness{{Harness: noHarnessCLI}, {Harness: "claude-code", Starts: true}, {Harness: "codex"}})
	if env["WARDYN_HARNESSES"] != "claude-code codex" || env["WARDYN_START_HARNESS"] != "claude-code" {
		t.Errorf("env = %v, want the two real harnesses and claude-code as the start", env)
	}
	env = map[string]string{}
	applyHarnessSetEnv(env, []types.RunHarness{{Harness: noHarnessCLI, Starts: true}})
	if len(env) != 0 {
		t.Errorf("a set of only none wrote %v, want nothing", env)
	}
}

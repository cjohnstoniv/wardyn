// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestTerminalRunStates_UIParity set-diffs RunState.IsTerminal against the UI's
// hand-written TERMINAL_RUN_STATES array. Go and TypeScript cannot share the
// set, and the two HAVE already drifted: omitting COMPLETED from the UI array
// left the Kill button enabled on finished runs (the backend then 409s). Both
// sides are read from source, so a state added to one and not the other fails
// here instead of shipping.
func TestTerminalRunStates_UIParity(t *testing.T) {
	src, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatalf("read types.go: %v", err)
	}
	goStates := map[string]bool{} // wire value -> IsTerminal
	for _, m := range regexp.MustCompile(`Run\w+\s+RunState\s*=\s*"(\w+)"`).FindAllStringSubmatch(string(src), -1) {
		goStates[m[1]] = RunState(m[1]).IsTerminal()
	}
	if len(goStates) < 5 {
		t.Fatalf("found only %d RunState constants in types.go; the scan regex is stale", len(goStates))
	}

	uiPath := filepath.Join("..", "..", "ui", "src", "app", "lib", "types", "runs.ts")
	ts, err := os.ReadFile(uiPath)
	if err != nil {
		t.Fatalf("read %s: %v", uiPath, err)
	}
	arr := regexp.MustCompile(`(?s)TERMINAL_RUN_STATES[^=]*=\s*\[(.*?)]`).FindStringSubmatch(string(ts))
	if arr == nil {
		t.Fatalf("no TERMINAL_RUN_STATES array in %s", uiPath)
	}
	uiTerminal := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(\w+)"`).FindAllStringSubmatch(arr[1], -1) {
		uiTerminal[m[1]] = true
	}

	for state, want := range goStates {
		if uiTerminal[state] != want {
			t.Errorf("terminal drift on %s: Go IsTerminal=%v, UI TERMINAL_RUN_STATES lists it=%v", state, want, uiTerminal[state])
		}
		delete(uiTerminal, state)
	}
	for state := range uiTerminal {
		t.Errorf("UI TERMINAL_RUN_STATES lists %q, which is not a RunState constant", state)
	}
}

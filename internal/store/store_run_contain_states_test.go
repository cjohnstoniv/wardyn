// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestTerminalRunStatesMatchesTheTypes: terminalRunStates, the SQL copy of types.RunState.IsTerminal, holds exactly the states IsTerminal
// does and none of NonTerminalRunStates, and every RunState constant is in one list or the other.
func TestTerminalRunStatesMatchesTheTypes(t *testing.T) {
	src, err := os.ReadFile("../types/types.go")
	if err != nil {
		t.Fatalf("read types.go: %v", err)
	}
	var all []types.RunState
	for _, m := range regexp.MustCompile(`Run\w+\s+RunState\s*=\s*"(\w+)"`).FindAllStringSubmatch(string(src), -1) {
		all = append(all, types.RunState(m[1]))
	}
	if len(all) < 5 {
		t.Fatalf("found only %d RunState constants in types.go; the scan regex is stale", len(all))
	}
	inSQL := func(s types.RunState) bool { return slices.Contains(terminalRunStates, string(s)) }
	for _, s := range all {
		nonTerminal := slices.Contains(types.NonTerminalRunStates, s)
		if inSQL(s) != s.IsTerminal() {
			t.Errorf("%s: in terminalRunStates = %v, IsTerminal() = %v", s, inSQL(s), s.IsTerminal())
		}
		if inSQL(s) == nonTerminal {
			t.Errorf("%s: in terminalRunStates = %v and in NonTerminalRunStates = %v; want exactly one", s, inSQL(s), nonTerminal)
		}
	}
	for _, name := range terminalRunStates {
		if !slices.Contains(all, types.RunState(name)) {
			t.Errorf("terminalRunStates lists %q, which is not a RunState constant", name)
		}
	}
}

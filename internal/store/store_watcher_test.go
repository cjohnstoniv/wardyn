// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Drift guard for the watcher sweep's state list. No database needed: the whole
// hazard is a hand-copied set, which is a source-text property.
package store

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

var quotedTokenRe = regexp.MustCompile(`'(\w+)'`)

func quotedSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, m := range quotedTokenRe.FindAllStringSubmatch(s, -1) {
		set[m[1]] = true
	}
	return set
}

// TestNonTerminalRunStates_MatchesTypes pins both SQL copies of the sweep's
// state list to types.RunState.IsTerminal, which declares itself the single
// source of the terminal set. Two ways this drifts and neither is loud:
// a new non-terminal state omitted here is simply never adopted (runs strand
// exactly as they did before the lease existed), and a list that stops matching
// migration 0027's partial-index predicate silently loses the index, turning
// every sweep tick into a seq scan of unbounded run history.
//
// The state universe is read from types.go's source, not a copy, so a state
// added there but not here fails HERE rather than in production.
func TestNonTerminalRunStates_MatchesTypes(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "types", "types.go"))
	if err != nil {
		t.Fatalf("read types.go: %v", err)
	}
	want := map[string]bool{}
	all := regexp.MustCompile(`Run\w+\s+RunState\s*=\s*"(\w+)"`).FindAllStringSubmatch(string(src), -1)
	for _, m := range all {
		if !types.RunState(m[1]).IsTerminal() {
			want[m[1]] = true
		}
	}
	if len(all) < 5 || len(want) == 0 {
		t.Fatalf("found %d RunState constants (%d non-terminal) in types.go; the scan regex is stale", len(all), len(want))
	}

	mig, err := os.ReadFile(filepath.Join("..", "db", "migrations", "0027_run_watcher_lease.sql"))
	if err != nil {
		t.Fatalf("read migration 0027: %v", err)
	}
	idx := regexp.MustCompile(`(?is)CREATE\s+INDEX[^;]*agent_runs_watcher_sweep_idx[^;]*state\s+IN\s*\(([^)]*)\)`).FindStringSubmatch(string(mig))
	if idx == nil {
		t.Fatal("no agent_runs_watcher_sweep_idx ... WHERE state IN (...) predicate in migration 0027")
	}

	for name, got := range map[string]map[string]bool{
		"store.nonTerminalRunStates":        quotedSet(nonTerminalRunStates),
		"0027 agent_runs_watcher_sweep_idx": quotedSet(idx[1]),
	} {
		for state := range want {
			if !got[state] {
				t.Errorf("%s omits non-terminal state %q: the sweep can never adopt a run in it", name, state)
			}
		}
		for state := range got {
			if !want[state] {
				t.Errorf("%s lists %q, which is terminal or not a RunState: the sweep would re-adopt a finished run", name, state)
			}
		}
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// allRunStates is the authoritative set of run states the control plane can
// persist. It is derived from the types constants so adding a new RunState
// without widening the DB CHECK constraint fails this test.
var allRunStates = []types.RunState{
	types.RunPending,
	types.RunStarting,
	types.RunRunning,
	types.RunWaiting,
	types.RunCompleted,
	types.RunStopped,
	types.RunArchived,
	types.RunFailed,
	types.RunKilled,
}

// stateInCheckRe captures the parenthesized value list of a `state IN ( ... )`
// CHECK clause in a migration (used for both the initial inline CHECK and any
// later ALTER ... ADD CONSTRAINT ... CHECK (state IN (...))).
var stateInCheckRe = regexp.MustCompile(`(?is)state\s+IN\s*\(([^)]*)\)`)

// quotedRe extracts single-quoted tokens like 'COMPLETED'.
var quotedRe = regexp.MustCompile(`'([^']+)'`)

// targetsAgentRunsRe matches a statement whose TARGET table is agent_runs
// (CREATE TABLE agent_runs / ALTER TABLE agent_runs). This must NOT match
// statements that merely FK-reference agent_runs (e.g. approval_requests's
// `REFERENCES agent_runs(id)`), which also carry their own state CHECK.
var targetsAgentRunsRe = regexp.MustCompile(
	`(?is)(?:CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?|ALTER\s+TABLE\s+(?:ONLY\s+)?)agent_runs\b`)

// effectiveAgentRunStates returns the set of agent_runs.state values allowed
// after ALL migrations are applied in lexical order. The LAST migration that
// (re)defines a `state IN (...)` CHECK wins, mirroring how an ALTER replaces
// the prior constraint at runtime.
func effectiveAgentRunStates(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var allowed map[string]bool
	for _, name := range names {
		data, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		// Process statement-by-statement (split on ';') so a `state IN (...)`
		// CHECK is attributed to its OWNING table. The approval_requests table
		// also has a state CHECK; only statements that mention agent_runs count.
		for _, stmt := range strings.Split(string(data), ";") {
			if !targetsAgentRunsRe.MatchString(stmt) {
				continue
			}
			loc := stateInCheckRe.FindStringSubmatch(stmt)
			if loc == nil {
				continue
			}
			vals := map[string]bool{}
			for _, q := range quotedRe.FindAllStringSubmatch(loc[1], -1) {
				vals[q[1]] = true
			}
			if len(vals) > 0 {
				allowed = vals // last writer (lexically-latest migration) wins
			}
		}
	}
	return allowed
}

// TestAgentRunStateCheckCoversAllStates is the always-on regression guard for
// the COMPLETED-state cluster: the completion watcher transitions runs to
// COMPLETED, but the original CHECK omitted it, so the UPDATE was rejected by
// Postgres and runs never reached terminal (credentials never revoked). This
// test fails if any types.RunState is not permitted by the effective CHECK.
func TestAgentRunStateCheckCoversAllStates(t *testing.T) {
	allowed := effectiveAgentRunStates(t)
	if allowed == nil {
		t.Fatal("no agent_runs state CHECK found in migrations")
	}
	for _, s := range allRunStates {
		if !allowed[string(s)] {
			t.Errorf("run state %q is not allowed by the agent_runs.state CHECK constraint; "+
				"add it to a migration (a run in this state cannot be persisted)", s)
		}
	}
	// Guard the other direction too: the CHECK must not allow states the code
	// does not define (catches typos in migrations).
	known := map[string]bool{}
	for _, s := range allRunStates {
		known[string(s)] = true
	}
	for s := range allowed {
		if !known[s] {
			t.Errorf("agent_runs.state CHECK allows %q which is not a defined types.RunState", s)
		}
	}
}

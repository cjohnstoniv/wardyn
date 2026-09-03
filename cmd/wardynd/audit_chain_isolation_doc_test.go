// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// snapshotEscapes are the constructs that would let the trigger's head lookup
// read OUTSIDE the calling transaction's snapshot. While none of them is
// present, the head a writer chains onto is whatever that writer's snapshot
// shows — which is the whole isolation caveat.
var snapshotEscapes = []string{
	"SET TRANSACTION", "ISOLATION LEVEL", "dblink", "pg_export_snapshot", "AUTONOMOUS",
}

// TestAuditChainSerializationCaveatIsDocumented pins what the chain's
// serialization actually guarantees, against the trigger that implements it.
//
// 0056 moved seq allocation and the chain link under one advisory lock, and the
// same commit told operators "Every writer is serialized, including one that is
// not Wardyn … takes its place in line rather than reading the same head as a
// concurrent Wardyn write", with one named cost. The lock does order writers.
// What it cannot decide is which head a writer READS: the lookup is an ordinary
// SELECT in the caller's transaction, so a REPEATABLE READ or SERIALIZABLE
// writer whose snapshot predates the previous commit chains onto a stale head.
// Two rows then share a prev_hash and the sweep reports "deleted or reordered"
// — permanently, per the latch — with nothing tampered with. An operator
// reading the unqualified claim could not have known to keep an external writer
// at READ COMMITTED, which is the one thing that avoids it.
func TestAuditChainSerializationCaveatIsDocumented(t *testing.T) {
	file, body := chainTriggerBody(t)

	// (1) The premise: a lock (so the ORDER claim is true) and a head lookup
	// with nothing that escapes the caller's snapshot (so the caveat is real).
	if !strings.Contains(body, "pg_advisory_xact_lock") {
		t.Errorf("%s no longer takes the advisory lock — the serialization claim in the docs is now unfounded, not merely narrow", file)
	}
	if !strings.Contains(body, "SELECT row_hash") {
		t.Fatalf("%s no longer reads the head with SELECT row_hash — re-derive this guard against the new shape", file)
	}
	for _, esc := range snapshotEscapes {
		if strings.Contains(body, esc) {
			t.Errorf("%s now contains %q: the head may no longer be read in the caller's snapshot, so the isolation caveat "+
				"in docs/OPERATIONS.md and threatmodel/THREAT-MODEL.md may be stale — re-check it rather than deleting it", file, esc)
		}
	}

	// (2) The runbook states the guarantee AND the residual, with the operator
	// instruction that follows from it.
	ops := readDoc(t, "docs/OPERATIONS.md")
	for _, want := range []string{
		"the link is correct for a writer at `READ COMMITTED`",
		"running in the CALLER's transaction",
		"An external writer to `audit_events` must use `READ COMMITTED`.",
		"`REPEATABLE READ` or `SERIALIZABLE`",
	} {
		if !strings.Contains(ops, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("docs/OPERATIONS.md's hash-chain section is missing %q", want)
		}
	}
	if strings.Contains(ops, "Every writer is serialized, including one that is not Wardyn") {
		t.Error("docs/OPERATIONS.md is back to the unqualified serialization claim; a REPEATABLE READ writer still forks the chain")
	}

	// (3) The threat model carries it as a residual: a break the chain reports
	// is not by itself proof of tampering.
	tm := readDoc(t, "threatmodel/THREAT-MODEL.md")
	for _, want := range []string{
		"a false one is reachable with no tampering at all",
		"head lookup runs in the CALLER's transaction snapshot",
	} {
		if !strings.Contains(tm, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("threatmodel/THREAT-MODEL.md §4.5 does not publish the residual: missing %q", want)
		}
	}
}

// chainTriggerBody returns the LAST migration that (re)defines
// audit_events_chain and its function body — the definition that actually ships.
func chainTriggerBody(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	def := regexp.MustCompile(`(?i)CREATE OR REPLACE FUNCTION audit_events_chain`)
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if def.Match(b) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no migration defines audit_events_chain — revisit this guard")
	}
	sort.Strings(names) // zero-padded numeric prefixes sort chronologically
	last := names[len(names)-1]
	b, err := os.ReadFile(filepath.Join(dir, last))
	if err != nil {
		t.Fatalf("read %s: %v", last, err)
	}
	src := string(b)
	i := def.FindStringIndex(src)
	return last, src[i[0]:]
}

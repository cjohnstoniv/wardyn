// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPGConcurrencyProofsRunUnderRace is the pin for F137.
//
// internal/broker/concurrency_pg_test.go's exactly-once proofs
// (TestPG_ConcurrentMint_ExactlyOnceWins,
// TestPG_ConcurrentMint_AutoApprovalGrant_Independent,
// TestPG_ConcurrentMintOnApproval_ExactlyOnce) spawn goroutines racing on a
// credential mint — the tests most in need of the race detector in the tree.
// No gate ran them under it:
//
//   - `make test-race`, the ONLY race job in ci.yml, strips the DSN in both of
//     its passes (`WARDYN_TEST_PG= go test -race ./...`), so every
//     WARDYN_TEST_PG-gated test is skipped there;
//   - `make test-report-pg`, the only target that SETS the DSN, shells out to
//     scripts/test-report.sh, whose `go test -json -covermode=atomic ...`
//     carries no -race.
//
// This guard holds the property in both directions, so neither half can be
// removed quietly: the pg job must run a race pass over the pg lane, and
// test-race must keep stripping the DSN (if it ever stopped, the pg lane would
// run in PARALLEL packages against one shared database — the very race
// test-report-pg's -p 1 exists to avoid).
func TestPGConcurrencyProofsRunUnderRace(t *testing.T) {
	root := repoRoot(t)

	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(mk)

	// (a) A target exists that runs the pg lane under -race.
	raceTarget := makeTargetBody(t, makefile, "test-race-pg")
	if !strings.Contains(raceTarget, "-race") {
		t.Errorf("the Makefile's test-race-pg recipe does not pass -race:\n%s", raceTarget)
	}
	if !strings.Contains(raceTarget, "./internal/broker/") {
		t.Errorf("test-race-pg does not cover ./internal/broker/, where the exactly-once mint proofs live:\n%s", raceTarget)
	}
	if strings.Contains(raceTarget, "WARDYN_TEST_PG=") {
		t.Errorf("test-race-pg STRIPS the DSN, so every pg-gated test it names would skip:\n%s", raceTarget)
	}

	// (b) test-race still strips the DSN — the reason a separate target is needed.
	if body := makeTargetBody(t, makefile, "test-race"); !strings.Contains(body, "WARDYN_TEST_PG=") {
		t.Errorf("test-race no longer strips WARDYN_TEST_PG; the pg lane would then run under "+
			"parallel packages against ONE shared database:\n%s", body)
	}

	// (c) CI actually runs it, in the job that has a Postgres service and sets
	// the DSN. A target nothing invokes is not a gate.
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	job := ciJobBlock(t, string(wf), "test-pg")
	if !strings.Contains(job, "make test-race-pg") {
		t.Errorf("ci.yml's test-pg job never runs `make test-race-pg`, so the broker's exactly-once "+
			"concurrency proofs are still never race-checked by any gate:\n%s", job)
	}
	if strings.Count(job, "WARDYN_TEST_PG:") < 2 {
		t.Errorf("the race step in ci.yml's test-pg job does not set WARDYN_TEST_PG, so every pg-gated "+
			"test it runs would SKIP and the step would pass vacuously:\n%s", job)
	}
}

// makeTargetBody returns the recipe lines of one Makefile target (the target
// line plus every following line that is a comment or tab-indented).
func makeTargetBody(t *testing.T, makefile, target string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, target+":") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("Makefile has no %q target — the guard cannot pass vacuously", target)
	}
	var b strings.Builder
	for _, ln := range lines[start:] {
		if ln != lines[start] && ln != "" && !strings.HasPrefix(ln, "\t") && !strings.HasPrefix(ln, "#") {
			break
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return b.String()
}

// ciJobBlock returns the YAML block of one top-level ci.yml job. Deliberately
// not a YAML parse (same rationale as TestReleasingDocNamesEveryCIJob): no YAML
// library is vendored and the shape is stable.
func ciJobBlock(t *testing.T, wf, job string) string {
	t.Helper()
	head := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(job) + `:\s*$`)
	loc := head.FindStringIndex(wf)
	if loc == nil {
		t.Fatalf("ci.yml has no top-level job %q — the guard cannot pass vacuously", job)
	}
	rest := wf[loc[1]:]
	next := regexp.MustCompile(`(?m)^  [a-z][a-z0-9-]*:\s*$`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}

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
//   - the race passes in ci.yml's build job — test-report, test-report-docker
//     and test-report-k8s, all three run by `make cover-check` — strip the DSN
//     (`WARDYN_TEST_PG= ./scripts/test-report.sh <suite> -race ...`), so every
//     WARDYN_TEST_PG-gated test is skipped there;
//   - `make test-report-pg`, the only target that SETS the DSN, shells out to
//     scripts/test-report.sh, whose `go test -json -covermode=atomic ...`
//     carries no -race.
//
// This guard holds the property in both directions, so neither half can be
// removed quietly: the pg job must run a race pass over the pg lane, and
// the three cover-check suites must keep both -race (they are the tree's only
// race passes; dropping it from one silently removes race detection for that
// tag set) and the DSN strip (if they ever stopped stripping it, the pg lane
// would run in PARALLEL packages against one shared database — the very race
// test-report-pg's -p 1 exists to avoid).
func TestPGConcurrencyProofsRunUnderRace(t *testing.T) {
	root := repoRoot(t)

	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(mk)
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}

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

	// (b) The three cover-check suites run under -race and strip the DSN — the
	// reason a separate pg race target is needed. Recipe lines only, so a
	// neighbouring target's comment prose cannot satisfy either check.
	for _, target := range []string{"test-report", "test-report-docker", "test-report-k8s"} {
		recipe := makeRecipeLines(t, makefile, target)
		if !strings.Contains(recipe, " -race") {
			t.Errorf("%s no longer passes -race; that tag set then has no race pass in CI at all:\n%s", target, recipe)
		}
		if !strings.Contains(recipe, "WARDYN_TEST_PG=") {
			t.Errorf("%s no longer strips WARDYN_TEST_PG; the pg lane would then run under "+
				"parallel packages against ONE shared database:\n%s", target, recipe)
		}
	}
	if body := makeTargetBody(t, makefile, "cover-check"); !strings.Contains(body, "test-report test-report-docker test-report-k8s") {
		t.Errorf("cover-check no longer depends on all three race suites:\n%s", body)
	}

	// ...and CI runs cover-check, or none of those race passes gate anything.
	if build := ciJobBlock(t, string(wf), "build"); !strings.Contains(build, "run: make cover-check") {
		t.Errorf("ci.yml's build job never runs `make cover-check`, so no race pass gates a PR:\n%s", build)
	}

	// (c) CI actually runs it, in the job that has a Postgres service and sets
	// the DSN. A target nothing invokes is not a gate.
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

// makeRecipeLines returns only the tab-indented recipe lines of one Makefile
// target, dropping the comments makeTargetBody also collects.
func makeRecipeLines(t *testing.T, makefile, target string) string {
	t.Helper()
	var b strings.Builder
	for _, ln := range strings.Split(makeTargetBody(t, makefile, target), "\n") {
		if strings.HasPrefix(ln, "\t") {
			b.WriteString(ln)
			b.WriteString("\n")
		}
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

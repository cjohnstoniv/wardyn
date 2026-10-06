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

// goroutineSpawn matches a goroutine launch statement — `go func(...)` or
// `go launch(i)` — at the start of a line.
var goroutineSpawn = regexp.MustCompile(`(?m)^\s+go [A-Za-z_(]`)

// pgTestFunc matches the opening line of a top-level TestPG_ function; gofmt
// puts its closing brace alone at column 0.
var pgTestFunc = regexp.MustCompile(`(?m)^func (TestPG_\w*)\(`)

// raceLine is one `go test` line of test-race-pg's recipe.
type raceLine struct {
	run  *regexp.Regexp // the -run filter; nil means every test
	pkgs []string       // package globs, e.g. ./internal/store/...
}

// raceLines parses every `go test` line of test-race-pg's recipe.
func raceLines(t *testing.T, raceTarget string) []raceLine {
	t.Helper()
	runFlag := regexp.MustCompile(`-run[= ]'?([^'\s]+)'?`)
	var out []raceLine
	for _, ln := range strings.Split(raceTarget, "\n") {
		if !strings.HasPrefix(ln, "\t") || !strings.Contains(ln, "go test") {
			continue
		}
		var rl raceLine
		if m := runFlag.FindStringSubmatch(ln); m != nil {
			rl.run = regexp.MustCompile(m[1])
		}
		for _, f := range strings.Fields(ln) {
			if strings.HasPrefix(f, "./") {
				rl.pkgs = append(rl.pkgs, f)
			}
		}
		out = append(out, rl)
	}
	if len(out) == 0 {
		t.Fatalf("test-race-pg has no `go test` line:\n%s", raceTarget)
	}
	return out
}

// covers reports whether a package glob covers dir (both ./-relative):
// ./x/... covers ./x and every package under it; a plain path only itself.
func covers(glob, dir string) bool {
	prefix, recursive := strings.CutSuffix(glob, "/...")
	if !recursive {
		return dir == glob
	}
	return prefix == "." || dir == prefix || strings.HasPrefix(dir, prefix+"/")
}

// internal/broker/concurrency_pg_test.go's exactly-once proofs
// (TestPG_ConcurrentMint_ExactlyOnceWins,
// TestPG_ConcurrentMint_AutoApprovalGrant_Independent,
// TestPG_ConcurrentMintOnApproval_ExactlyOnce) spawn goroutines racing on a
// credential mint — the tests most in need of the race detector in the tree.
// No gate ran them under it:
//
//   - the race passes — test-report, test-report-docker and test-report-k8s,
//     all three run by `make cover-check` and by ci.yml's go legs — strip the DSN
//     (`WARDYN_TEST_PG= ./scripts/test-report.sh <suite> -race ...`), so every
//     WARDYN_TEST_PG-gated test is skipped there;
//   - `make test-report-pg`, the only target that SETS the DSN, shells out to
//     scripts/test-report.sh, whose `go test -json -covermode=atomic ...`
//     carries no -race.
//
// This guard holds the property in both directions, so neither half can be
// removed quietly: a pg shard must run a race pass over the pg lane, and
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

	// ...and CI runs all three (one `go` matrix leg each) and the required
	// `build` job enforces the floor over their profiles, or none of those race
	// passes gate anything.
	goJob := ciJobBlock(t, string(wf), "go")
	for _, target := range []string{"test-report", "test-report-docker", "test-report-k8s"} {
		if !strings.Contains(goJob, "target: "+target+"\n") {
			t.Errorf("ci.yml's go job has no leg running `make %s`, so that race pass gates no PR:\n%s", target, goJob)
		}
	}
	// Only an explicit docs- or console-only classification (backend 'false')
	// swaps the suites for the guard packages, so a missing or failed
	// classification runs them. The
	// executing line swallows no failure (an exact line, so no `|| true`), and
	// no step may continue on error: a leg whose profile did not upload has not
	// done its job, since build unions those profiles.
	const fullSuites = "        if: matrix.suite == 'lint' || needs.changes.outputs.backend != 'false'\n" +
		"        run: make ${{ matrix.target }}\n"
	if !strings.Contains(goJob, fullSuites) {
		t.Errorf("ci.yml's go job no longer runs `make ${{ matrix.target }}` unless the change is explicitly docs- or console-only:\n%s", goJob)
	}
	if strings.Contains(goJob, "continue-on-error:") {
		t.Errorf("a step in ci.yml's go job continues on error, so a failed suite or a missing profile can pass:\n%s", goJob)
	}
	// The guard-package replacement is a gate too: it runs only on an explicit
	// 'false', its `go test` line is pinned whole (so nothing can be appended
	// to swallow a failure), and a tagless leg that finds no guard package
	// fails instead of passing empty.
	for _, want := range []string{
		"\n        if: matrix.suite != 'lint' && needs.changes.outputs.backend == 'false'\n",
		"\n" + `          WARDYN_TEST_PG='' go test -count=1 ${TAGS:+-tags "$TAGS"} $pkgs` + "\n",
		"\n" + `            [ -n "$TAGS" ] || { echo "::error::no doc-reading guard test found; the discovery pattern regressed"; exit 1; }` + "\n",
	} {
		if !strings.Contains(goJob, want) {
			t.Errorf("ci.yml's go job no longer carries the guard-package step's line %q:\n%s", want, goJob)
		}
	}
	build := ciJobBlock(t, string(wf), "build")
	for _, want := range []string{
		"needs: [changes, go]",
		"LEGS: ${{ needs.go.result }}",
		"        if: needs.changes.outputs.backend != 'false'\n        run: make cover-union\n",
	} {
		if !strings.Contains(build, want) {
			t.Errorf("ci.yml's build job must need every go leg and run `make cover-union` over their "+
				"profiles unless the change is explicitly docs- or console-only; missing %q:\n%s", want, build)
		}
	}

	// (c) CI actually runs it, in a shard of the test-pg-shard matrix, which has
	// a Postgres service and sets the DSN. A target nothing invokes is not a gate.
	job := ciJobBlock(t, string(wf), "test-pg-shard")
	if !strings.Contains(job, "run: make test-race-pg\n") {
		t.Errorf("ci.yml's test-pg-shard job has no race shard running `make test-race-pg`, so the broker's "+
			"exactly-once concurrency proofs are still never race-checked by any gate:\n%s", job)
	}
	if !strings.Contains(job, "WARDYN_TEST_PG:") {
		t.Errorf("the shards in ci.yml's test-pg-shard job do not set WARDYN_TEST_PG, so every pg-gated "+
			"test they run would SKIP and the race shard would pass vacuously:\n%s", job)
	}

	// (d) every TestPG_ function that spawns a goroutine is actually raced: some
	// recipe line both covers its package and matches its name with -run. A
	// goroutine-spawning pg test in an un-raced package (I-3: internal/api held
	// two), or one a narrowed -run filters out, would otherwise never run under
	// the detector.
	lines := raceLines(t, raceTarget)
	var unraced []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "node_modules" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := "./" + filepath.ToSlash(rel)
		code := string(src)
		for _, m := range pgTestFunc.FindAllStringSubmatchIndex(code, -1) {
			name := code[m[2]:m[3]]
			body := code[m[0]:]
			if end := strings.Index(body, "\n}\n"); end >= 0 {
				body = body[:end]
			}
			if !goroutineSpawn.MatchString(body) {
				continue
			}
			raced := false
			for _, rl := range lines {
				for _, g := range rl.pkgs {
					if covers(g, pkg) && (rl.run == nil || rl.run.MatchString(name)) {
						raced = true
					}
				}
			}
			if !raced {
				unraced = append(unraced, pkg+"."+name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for TestPG_ functions: %v", root, err)
	}
	if len(unraced) > 0 {
		t.Errorf("these TestPG_ functions spawn goroutines but no test-race-pg line races them "+
			"(package not covered, or name filtered out by -run):\n%s\nrecipe:\n%s",
			strings.Join(unraced, "\n"), raceTarget)
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

// TestCIClassifierPutsWhereAFileLivesBeforeItsExtension pins the order of the
// path rules in ci.yml's `changes` job. Its docs rule once read
// `docs/*|threatmodel/*|*.md` and came first, so ANY .md was documentation, and
// a docs-only change skips helm, helm-install-test, both conformance jobs,
// test-pg, the image scans and notices. Two things made that a hole:
//
//   - Helm renders every file under deploy/helm/wardyn/templates whatever its
//     extension, so a NetworkPolicy written in templates/x.md would have merged
//     with none of those jobs run;
//   - every Dockerfile COPYs LICENSING.md and deploy/images/README.md, so
//     deleting or renaming one broke each image build, with no image built on
//     the pull request to say so.
//
// So the directory rules lead and only a .md at the repository root is
// documentation by its extension: `deploy/*|LICENSING.md` is the first arm,
// and `*/*` (every other directory) precedes `*.md`. The template half is also
// held without the classifier, by scripts/check-helm-templates.sh, which this
// guard keeps wired into both `make lint` (every change) and `make helm-lint`.
func TestCIClassifierPutsWhereAFileLivesBeforeItsExtension(t *testing.T) {
	root := repoRoot(t)
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	changes := ciJobBlock(t, string(wf), "changes")

	const firstArm = "            case \"$f\" in\n" +
		"              deploy/*|LICENSING.md) class=backend; code=true; backend=true ;;\n"
	if !strings.Contains(changes, firstArm) {
		t.Errorf("the changes job's path rules no longer open with `deploy/*|LICENSING.md) class=backend`: "+
			"a file Helm renders or a Dockerfile copies could be classed by its extension first:\n%s", changes)
	}
	arms := []string{
		"\n              deploy/*|LICENSING.md) class=backend; code=true; backend=true ;;\n",
		"\n              docs/*|threatmodel/*) class=docs ;;\n",
		"\n              */*) class=backend; code=true; backend=true ;;\n",
		"\n              *.md) class=docs ;;\n",
	}
	last := -1
	for _, arm := range arms {
		if n := strings.Count(changes, arm); n != 1 {
			t.Fatalf("the changes job must carry the path rule %q exactly once, found %d:\n%s", arm, n, changes)
		}
		at := strings.Index(changes, arm)
		if at < last {
			t.Errorf("the path rule %q comes too early: the order must be deploy/ and LICENSING.md, the docs "+
				"directories, every other directory, then root-level *.md:\n%s", arm, changes)
		}
		last = at
	}
	// Two rules name the docs class, and both are pinned above. A third could
	// only be a way to call some other path documentation.
	if n := strings.Count(changes, "class=docs"); n != 2 {
		t.Errorf("the changes job classes a path as docs in %d places, want the 2 pinned above (docs/ and "+
			"threatmodel/, and root-level *.md):\n%s", n, changes)
	}

	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for _, target := range []string{"lint", "helm-lint"} {
		if recipe := makeRecipeLines(t, string(mk), target); !strings.Contains(recipe, "./scripts/check-helm-templates.sh\n") {
			t.Errorf("`make %s` no longer runs scripts/check-helm-templates.sh, so a manifest in a "+
				"non-template file under the chart's templates/ is not refused there:\n%s", target, recipe)
		}
	}
}

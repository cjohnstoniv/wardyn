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

// RELEASING.md's tag gate names the ci.yml jobs a maintainer must see green
// before pushing a tag. That list drifted in BOTH directions before 0.7:
//
//   - `sbom-stub` was named but had been DELETED along with `make sbom`, so a
//     maintainer following the list literally waited on a job that can never
//     report.
//   - `notices` — the copyleft / unreviewed-dependency gate — was missing
//     entirely, so the list told them to skip the one job that catches a GPL
//     regression. On a release that adds an X stack, that is the expensive half.
//
// Neither is a typo; both are drift, and drift recurs. This is the check that
// makes it fail loudly instead.
func TestReleasingDocNamesEveryCIJob(t *testing.T) {
	root := repoRoot(t)

	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	// Top-level job ids: exactly two spaces of indent, then `<id>:`. Deliberately
	// not a YAML parse — this guard must not depend on a YAML library being
	// vendored, and the shape is stable.
	jobRe := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\s*$`)
	var jobs []string
	inJobs := false
	for _, line := range strings.Split(string(wf), "\n") {
		if line == "jobs:" {
			inJobs = true
			continue
		}
		if inJobs && len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			break // a new top-level key ended the jobs block
		}
		if !inJobs {
			continue
		}
		if m := jobRe.FindStringSubmatch(line); m != nil {
			jobs = append(jobs, m[1])
		}
	}
	if len(jobs) < 10 {
		t.Fatalf("parsed only %d ci.yml jobs (%v) — the parse regressed, and this guard would pass vacuously", len(jobs), jobs)
	}

	doc, err := os.ReadFile(filepath.Join(root, "RELEASING.md"))
	if err != nil {
		t.Fatalf("read RELEASING.md: %v", err)
	}
	text := string(doc)

	var missing []string
	for _, j := range jobs {
		if !strings.Contains(text, "`"+j+"`") {
			missing = append(missing, j)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("ci.yml jobs absent from RELEASING.md: %v\n"+
			"A maintainer reads that list to decide what must be green before tagging; a job missing from it is a gate they will skip.", missing)
	}

	// The other direction: a name in the list that is no longer a ci.yml job —
	// what `sbom-stub` was, a phantom the maintainer waits on forever because
	// GitHub never reports a job that cannot run.
	//
	// Scoped to the ENUMERATION, not to every backticked token in the file: the
	// prose around it deliberately names things that are not ci.yml jobs (the
	// deleted `sbom-stub` it records, the `publish-image` / `release` workflows
	// it excludes by name), and a whole-file scan therefore had to allowlist
	// them — which it did by hardcoding `sbom-stub` as the ONLY name it would
	// ever complain about, so deleting a real job the list still names left
	// this arm silent. The enumeration runs from the "job list:" marker to the
	// em-dash that ends it; `gates`' matrix parenthetical is cut because those
	// are matrix entries, not top-level job ids, and jobRe above never sees them.
	present := map[string]bool{}
	for _, j := range jobs {
		present[j] = true
	}
	const marker = "ci.yml` job list:"
	at := strings.Index(text, marker)
	if at < 0 {
		t.Fatal("RELEASING.md no longer introduces the ci.yml job list with \"ci.yml` job list:\" — this guard can no longer see the list it exists to check")
	}
	list := text[at+len(marker):]
	if end := strings.Index(list, " — "); end >= 0 {
		list = list[:end]
	}
	list = regexp.MustCompile(`\(a matrix job:[^)]*\)`).ReplaceAllString(list, "")

	named := regexp.MustCompile("`([a-z][a-z0-9-]*)`").FindAllStringSubmatch(list, -1)
	if len(named) < 10 {
		t.Fatalf("parsed only %d job names from RELEASING.md's list — the parse regressed, and this arm would pass vacuously", len(named))
	}
	seen := map[string]bool{}
	var phantom []string
	for _, m := range named {
		name := m[1]
		if seen[name] || present[name] {
			continue
		}
		seen[name] = true
		phantom = append(phantom, name)
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Errorf("RELEASING.md's ci.yml job list names jobs that no longer exist: %v\n"+
			"GitHub never reports a job that cannot run, so a maintainer waits on it forever.", phantom)
	}
}

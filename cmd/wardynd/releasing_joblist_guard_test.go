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

	// The other direction: a job named in the doc that no longer exists. This is
	// what `sbom-stub` was — a phantom the maintainer waits on forever.
	present := map[string]bool{}
	for _, j := range jobs {
		present[j] = true
	}
	// Only check names that LOOK like job ids inside backticks and are not
	// obviously something else (a make target, a file, a context cell).
	backtick := regexp.MustCompile("`([a-z][a-z0-9-]{2,})`")
	seen := map[string]bool{}
	var phantom []string
	for _, m := range backtick.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if seen[name] || present[name] {
			continue
		}
		seen[name] = true
		// A name is only a phantom if the doc presents it AS a ci.yml job. The
		// cheap, low-false-positive signal: it used to be one. Keep this list
		// empty; it exists so a deletion has somewhere to be recorded.
		for _, dead := range []string{"sbom-stub"} {
			if name == dead && !strings.Contains(text, "`"+dead+"` used to be named here") &&
				!strings.Contains(text, "used\nto cite `"+dead+"`") {
				phantom = append(phantom, name)
			}
		}
	}
	if len(phantom) > 0 {
		t.Errorf("RELEASING.md names ci.yml jobs that no longer exist: %v\n"+
			"GitHub never reports a job that cannot run, so a maintainer waits on it forever.", phantom)
	}
}

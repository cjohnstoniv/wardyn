// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestReleasingDocNamesEveryCIJob(t *testing.T) {
	root := repoRoot(t)
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// Job ids have exactly two spaces; stop at the next top-level key.
	jobRe := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\s*$`)
	jobs := map[string]bool{}
	inJobs := false
	for _, line := range strings.Split(string(wf), "\n") {
		if line == "jobs:" {
			inJobs = true
			continue
		}
		if inJobs && len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			break
		}
		if inJobs {
			if m := jobRe.FindStringSubmatch(line); m != nil {
				jobs[m[1]] = true
			}
		}
	}
	if len(jobs) == 0 {
		t.Fatal("parsed no ci.yml jobs")
	}

	for _, tc := range []struct{ name, path, start, end, pattern string }{
		{"RELEASING", "RELEASING.md", "ci.yml` job list:", " — ", "`([a-z][a-z0-9-]*)`"},
		{"docs_CI", "docs/CI.md", "| Check | Runs | Median | Max | Timeout |", "\n\n", "(?m)^\\| `([^`]+)`[^|]*\\|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := os.ReadFile(filepath.Join(root, tc.path))
			if err != nil {
				t.Fatal(err)
			}
			_, list, found := strings.Cut(string(doc), tc.start)
			if !found {
				t.Fatalf("%s: job-list anchor missing", tc.path)
			}
			list, _, found = strings.Cut(list, tc.end)
			if !found {
				t.Fatalf("%s: job-list end missing", tc.path)
			}
			// Matrix cells are check names, not additional top-level jobs. The budget
			// table also retains explicitly marked nightly measurements.
			list = regexp.MustCompile(`\(a matrix job:[^)]*\)`).ReplaceAllString(list, "")
			named := map[string]bool{}
			for _, m := range regexp.MustCompile(tc.pattern).FindAllStringSubmatch(list, -1) {
				if strings.Contains(m[0], ", nightly") {
					continue
				}
				name, _, _ := strings.Cut(m[1], " (")
				named[name] = true
			}
			if len(named) == 0 {
				t.Fatalf("%s: parsed no job names", tc.path)
			}
			var missing, phantom []string
			for name := range jobs {
				if !named[name] {
					missing = append(missing, name)
				}
			}
			for name := range named {
				if !jobs[name] {
					phantom = append(phantom, name)
				}
			}
			slices.Sort(missing)
			slices.Sort(phantom)
			if len(missing) > 0 {
				t.Errorf("%s omits ci.yml jobs: %v", tc.path, missing)
			}
			if len(phantom) > 0 {
				t.Errorf("%s names nonexistent ci.yml jobs: %v", tc.path, phantom)
			}
		})
	}
}

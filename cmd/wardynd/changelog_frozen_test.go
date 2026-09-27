// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Changelog guard, beside the doc guards in docs_r3_guard_test.go and
// docs_r3_egress_guard_test.go: a released section of CHANGELOG.md must read
// exactly as the tag that shipped it, so a stray edit — the #368 entry
// duplicated between [Unreleased] and 0.7.9's own section was exactly this —
// reds instead of riding along silently (#750, SF-18).

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// changelogVersionHeading matches one "## [X.Y.Z]" release heading, capturing
// the version. "## [Unreleased]" never matches — there is no tag to compare
// it against.
var changelogVersionHeading = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)

// changelogSections splits a CHANGELOG.md text into one body per released
// heading, from the heading line up to (not including) the next "## ["
// heading or EOF, keyed by version.
func changelogSections(doc string) map[string]string {
	lines := strings.Split(doc, "\n")
	out := map[string]string{}
	for i, line := range lines {
		m := changelogVersionHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "## [") {
				end = j
				break
			}
		}
		out[m[1]] = strings.Join(lines[i:end], "\n")
	}
	return out
}

// changelogPreexistingCorrections lists released sections whose text
// legitimately differs from the tag that shipped them, because a LATER commit
// corrected a factual error the release itself got wrong — not because a
// duplicate or stray entry drifted in unnoticed, which is the defect this
// guard exists to catch. Each is keyed to the commit that made the
// correction, and its section is pinned to that commit's text instead of the
// tag's: the correction is allowed, a further edit is not. Each commit must
// be an ancestor of main, so a full-history checkout carries it. A version is
// only ever added here for a documented correction; never to silence this
// guard.
var changelogPreexistingCorrections = map[string]string{
	"0.4.0": "66aa886f2a0703c8c05435cd205d42e3c850a7f6", // docs: consolidate the doc set and fix the claims that were wrong
	"0.4.1": "66aa886f2a0703c8c05435cd205d42e3c850a7f6", // (same commit)
	"0.4.2": "66aa886f2a0703c8c05435cd205d42e3c850a7f6", // (same commit)
	"0.6.2": "7fc9d85f68b10169a9077ac82a66497dae7ff6a1", // fix(sbom): merge the UI lockfile, and correct what 0.6.2 claimed
	"0.7.0": "89762aa766f52bbb3af7e1d14350be8cd1f14a86", // fix(docs): address blind-review findings D-1..D-10
	// fix(compose): correct the writable-member-mount claim, add /srv/src
	// bind — authored as 3c64194c0 off main; this is the squash that landed it.
	"0.7.2": "8a69d8d2325b824a576d6fa03288014ff9ae0018", // Backport 17 merged fixes from main into 0.7.10 (#404)
}

// TestChangelogReleasedSectionsAreFrozen pins every released CHANGELOG.md
// section to what the tag that shipped it actually carries — or, for a
// version in changelogPreexistingCorrections, to what its correcting commit
// carries.
//
// A version whose baseline commit is not fetched in this checkout is
// skipped, not failed: a shallow clone does not carry the tag history, and
// this guard's job is to catch a real edit, not to make git-fetch depth a
// build requirement. CI's build job checks out full history with tags, so
// every subtest runs there.
func TestChangelogReleasedSectionsAreFrozen(t *testing.T) {
	root := repoRoot(t)
	sections := changelogSections(readRepo(t, "CHANGELOG.md"))
	if len(sections) == 0 {
		t.Fatal("no \"## [X.Y.Z]\" release heading found in CHANGELOG.md — the guard's anchor moved, so it is asserting nothing")
	}

	for version, body := range sections {
		version, body := version, body
		t.Run(version, func(t *testing.T) {
			rev := "v" + version
			if fix, corrected := changelogPreexistingCorrections[version]; corrected {
				rev = fix
			}
			if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", rev+"^{commit}").Run(); err != nil {
				t.Skipf("%s is not fetched in this checkout — cannot compare", rev)
			}
			out, err := exec.Command("git", "-C", root, "show", rev+":CHANGELOG.md").Output()
			if err != nil {
				t.Fatalf("git show %s:CHANGELOG.md: %v", rev, err)
			}
			want, ok := changelogSections(string(out))[version]
			if !ok {
				t.Fatalf("%s's own CHANGELOG.md has no \"## [%s]\" heading — the guard's anchor moved, so it is asserting nothing", rev, version)
			}
			if want != body {
				t.Errorf("CHANGELOG.md's [%s] section no longer matches %s — a released section only changes for a documented correction (key the correcting commit in changelogPreexistingCorrections), never a silent edit", version, rev)
			}
		})
	}
}

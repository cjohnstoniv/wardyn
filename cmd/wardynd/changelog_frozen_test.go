// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Changelog guard, beside the doc guards in docs_r3_guard_test.go and
// docs_r3_egress_guard_test.go: a released section must read exactly as the tag
// that shipped it, so a stray edit — the #368 entry duplicated between
// [Unreleased] and 0.7.9's own section was exactly this — reds instead of riding
// along silently (#750, SF-18). CHANGELOG.md keeps the newest minor line; older
// minors live in docs/releases/<minor>.md (#466), and both are frozen.

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// changelogVersion is the one version grammar the heading and the tag share: a
// final "X.Y.Z" or a release candidate "X.Y.Z-rc.N" (RELEASING.md).
const changelogVersion = `\d+\.\d+\.\d+(?:-rc\.\d+)?`

// changelogVersionHeading matches one "## [X.Y.Z]" or "## [X.Y.Z-rc.N]" release
// heading, capturing the version. "## [Unreleased]" never matches — there is no
// tag to compare it against.
var changelogVersionHeading = regexp.MustCompile(`(?m)^## \[(` + changelogVersion + `)\]`)

// changelogTag matches a release tag, final or candidate, capturing the version.
var changelogTag = regexp.MustCompile(`^v(` + changelogVersion + `)$`)

// changelogSections splits a CHANGELOG.md text into one body per released
// heading, from the heading line up to (not including) the next "## ["
// heading — a release candidate's included, so it ends the section above it —
// or EOF, keyed by version.
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
	// rc.1 cited docs/build-images.md, which had moved to docs/operations/.
	"0.9.3-rc.1": "565fd76784b3802175db427c2ebf048ca3326ec1", // docs(changelog): rc.1 line cites docs/operations/build-images.md (#2464)
	// #461 dropped both sections' private ~/.claude / local/ review-ledger
	// citations (the R1-R7 hardening-pass table, the 0.7.2 triage-sheet
	// references) in favour of prose and one Known-gaps list — an
	// issue-directed edit to a released section, not a silent drift.
	"0.7.0": "44df95c4483504a2001301e0abb42b16c0fcf17d", // docs(461): drop private-path citations from the hardening-pass section
	// fix(compose): correct the writable-member-mount claim, add /srv/src
	// bind — authored as 3c64194c0 off main; this is the squash that landed it.
	"0.7.2": "44df95c4483504a2001301e0abb42b16c0fcf17d", // docs(461): drop private-path citations from the 0.7.2 triage references
}

// changelogKnownGaps matches the spellings the #466 move unified into one
// "### Known gaps" heading: "### Known gaps and deferrals" and the nested
// "#### Known gaps". Only releases up to 0.8 used them, so the rewrite stops
// there: a newer section spelled that way is a real edit, not a shipped form.
var changelogKnownGaps = regexp.MustCompile(`(?m)^(?:### Known gaps and deferrals|#### Known gaps)$`)

// changelogProcessHeadings are the only text the #466 move removed from a
// released section: two process headings (and the second one's lead paragraph)
// in 0.7.6 whose bullets stay as ordinary release notes. Each must apply to the
// tag's text exactly once. Beside the Known-gaps unification this is the whole
// difference between a released section and the tag that shipped it.
var changelogProcessHeadings = map[string][]string{
	"0.7.6": {
		"### Retired (from 0.7.5's Known gaps)\n\n",
		"### From the 0.7.x readiness review\n\n" +
			"Independent documentation/security-lane patches from the owner's separate `review/0.7x-ledger`\n" +
			"readiness campaign, cherry-picked individually onto this release (the campaign's own ledger is kept\n" +
			"on its private review branch; none is a schema, configured-cap or deployment-requirement change; two\n" +
			"tighten an existing input check and one turns a relayed oversized upload into a 413 — see below).\n\n",
	},
}

// changelogLinkTarget matches an inline link's relative target: not an anchor,
// an absolute path or a URL scheme.
var changelogLinkTarget = regexp.MustCompile(`\]\(([^)\s]+)`)

var changelogLinkScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// changelogRebaseLinks is the one edit the move to docs/releases/ makes to a
// section's links: a relative target is written from the repo root, so it gains
// "../../" and nothing else changes.
func changelogRebaseLinks(text string) string {
	return changelogLinkTarget.ReplaceAllStringFunc(text, func(m string) string {
		target := m[len("]("):]
		if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "/") || changelogLinkScheme.MatchString(target) {
			return m
		}
		return "](../../" + target
	})
}

// changelogUpTo08 reports whether version is a 0.x release with x <= 8.
func changelogUpTo08(version string) bool {
	var major, minor int
	_, err := fmt.Sscanf(version, "%d.%d.", &major, &minor)
	return err == nil && major == 0 && minor <= 8
}

// changelogBaseline is the text a released section must carry now: the
// shipped (or correcting-commit) text with the #466 editorial edits applied.
// moved is true for a section that lives in docs/releases/, where its links are
// rebased.
func changelogBaseline(t *testing.T, version, shipped string, moved bool) string {
	t.Helper()
	for _, cut := range changelogProcessHeadings[version] {
		if strings.Count(shipped, cut) != 1 {
			t.Fatalf("[%s]: process-heading edit %.40q does not apply exactly once to the shipped text", version, cut)
		}
		shipped = strings.Replace(shipped, cut, "", 1)
	}
	if changelogUpTo08(version) {
		shipped = changelogKnownGaps.ReplaceAllString(shipped, "### Known gaps")
	}
	if moved {
		shipped = changelogRebaseLinks(shipped)
	}
	return shipped
}

// changelogFiles lists every file that holds released sections: CHANGELOG.md
// and docs/releases/*.md.
func changelogFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"CHANGELOG.md"}
	more, err := filepath.Glob(filepath.Join(repoRoot(t), "docs", "releases", "*.md"))
	if err != nil || len(more) == 0 {
		t.Fatalf("docs/releases/*.md: %d files, %v — the moved sections are asserting nothing", len(more), err)
	}
	sort.Strings(more)
	for _, m := range more {
		files = append(files, "docs/releases/"+filepath.Base(m))
	}
	return files
}

// TestChangelogReleasedSectionsAreFrozen pins every released section, in
// CHANGELOG.md and docs/releases/*.md, to what the tag that shipped it actually carries — or, for a
// version in changelogPreexistingCorrections, to what its correcting commit
// carries.
//
// A version whose baseline commit is not fetched in this checkout is
// skipped, not failed: a shallow clone does not carry the tag history, and
// this guard's job is to catch a real edit, not to make git-fetch depth a
// build requirement. CI's build job checks out full history with tags, so
// every subtest runs there.
type changelogSection struct{ file, body string }

// allChangelogSections reads every released section from CHANGELOG.md and
// docs/releases/*.md, failing if one version is recorded in two places.
func allChangelogSections(t *testing.T) map[string]changelogSection {
	t.Helper()
	sections := map[string]changelogSection{}
	for _, file := range changelogFiles(t) {
		for version, body := range changelogSections(readRepo(t, file)) {
			if prev, dup := sections[version]; dup {
				t.Fatalf("[%s] is in both %s and %s — a release is recorded once", version, prev.file, file)
			}
			sections[version] = changelogSection{file, body}
		}
	}
	return sections
}

// changelogTagsMissingSection returns the release tags, final or candidate,
// from v0.4.0 on that have no section in sections. Tags older than v0.4.0 live
// in CHANGELOG-ARCHIVE.md, and anything that is not a release tag is ignored.
func changelogTagsMissingSection(tags []string, sections map[string]changelogSection) []string {
	var missing []string
	for _, tag := range tags {
		m := changelogTag.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		var major, minor int
		_, _ = fmt.Sscanf(m[1], "%d.%d.", &major, &minor)
		if major == 0 && minor < 4 {
			continue // older than v0.4.0: CHANGELOG-ARCHIVE.md
		}
		if _, ok := sections[m[1]]; !ok {
			missing = append(missing, tag)
		}
	}
	return missing
}

// TestChangelogEveryTaggedReleaseHasASection fails when a release tag, final or
// candidate, from v0.4.0 on has no section in CHANGELOG.md or docs/releases/:
// the freeze compares only the sections that are present, so a section lost in
// a move would otherwise go unnoticed. Tags this checkout does not carry are
// not listed, so a shallow clone checks fewer.
func TestChangelogEveryTaggedReleaseHasASection(t *testing.T) {
	out, err := exec.Command("git", "-C", repoRoot(t), "tag", "-l", "v*").Output()
	if err != nil {
		t.Fatalf("git tag -l: %v", err)
	}
	for _, tag := range changelogTagsMissingSection(strings.Fields(string(out)), allChangelogSections(t)) {
		t.Errorf("tag %s has no \"## [%s]\" section in CHANGELOG.md or docs/releases/*.md — a released section was dropped", tag, tag[1:])
	}
}

// changelogFixture is a CHANGELOG.md in miniature: a final above its own
// release candidate, so the candidate's heading is the boundary under the final.
const changelogFixture = `## [Unreleased]

## [1.2.3] — 2026-01-02

Final notes.

## [1.2.3-rc.1] — 2026-01-01

Candidate notes.
`

// TestChangelogRCSectionsAreSectionsAndFrozen proves the guard's own logic on
// the fixture: a release candidate is a section boundary, a changed candidate
// section differs from the tag's, and a candidate tag without a section is
// reported — while finals, pre-0.4.0 tags and non-release tags stay quiet.
func TestChangelogRCSectionsAreSectionsAndFrozen(t *testing.T) {
	got := changelogSections(changelogFixture)
	if len(got) != 2 {
		t.Fatalf("fixture yields %d sections, want the final and the candidate: %v", len(got), got)
	}
	if strings.Contains(got["1.2.3"], "Candidate notes.") {
		t.Errorf("the candidate heading did not end the final's section above it: %q", got["1.2.3"])
	}
	if !strings.Contains(got["1.2.3-rc.1"], "Candidate notes.") {
		t.Errorf("the candidate has no section of its own: %q", got["1.2.3-rc.1"])
	}

	edited := changelogSections(strings.Replace(changelogFixture, "Candidate notes.", "Candidate notes, edited.", 1))
	if edited["1.2.3-rc.1"] == got["1.2.3-rc.1"] {
		t.Error("an edited candidate section reads the same as the tag's, so the freeze would not red")
	}
	if edited["1.2.3"] != got["1.2.3"] {
		t.Error("editing the candidate changed the final's section")
	}

	have := map[string]changelogSection{"1.2.3": {}, "1.2.3-rc.1": {}}
	tags := []string{"v1.2.3", "v1.2.3-rc.1", "v0.3.9", "v0.3.9-rc.1", "v1.2.3-beta.1", "latest"}
	if missing := changelogTagsMissingSection(tags, have); len(missing) != 0 {
		t.Errorf("tags with sections, pre-0.4.0 tags or non-release tags reported missing: %v", missing)
	}
	delete(have, "1.2.3-rc.1")
	if missing := changelogTagsMissingSection(tags, have); len(missing) != 1 || missing[0] != "v1.2.3-rc.1" {
		t.Errorf("a candidate tag without a section: missing = %v, want [v1.2.3-rc.1]", missing)
	}
}

func TestChangelogReleasedSectionsAreFrozen(t *testing.T) {
	root := repoRoot(t)
	sections := allChangelogSections(t)
	if len(sections) == 0 {
		t.Fatal("no \"## [X.Y.Z]\" release heading found in CHANGELOG.md — the guard's anchor moved, so it is asserting nothing")
	}

	for version, sec := range sections {
		version, sec := version, sec
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
			// A moved section ends at its file's end, so only the trailing newlines differ.
			want = strings.TrimRight(changelogBaseline(t, version, want, sec.file != "CHANGELOG.md"), "\n")
			if got := strings.TrimRight(sec.body, "\n"); want != got {
				t.Errorf("%s's [%s] section no longer matches %s — a released section only changes for a documented correction (key the correcting commit in changelogPreexistingCorrections), never a silent edit", sec.file, version, rev)
			}
		})
	}
}

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
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	shippedversion "github.com/cjohnstoniv/wardyn/internal/version"
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
	// rc.2 said a Mac can run two macOS VM sandboxes; that count was never run on a real Mac, so the line now
	// says it is built for two and links the guide's untested list.
	"0.10.0-rc.2": "0d17b871bdcb7b13fd6b356bfebfd342210fa0ea", // docs: a pending leaver purge is retried by the sweeper; link the macOS VM untested list
	// rc.12 said the guest turns a macOS VM's screen lock off at the start of each run; it only tries, and when
	// that fails it logs why and the run starts anyway, so the line now says so.
	"0.10.0-rc.12": "ed9061ab94a5cb6b3a59675a7f205c6a322d7e6f", // Docs: the screen-lock changelog line says the guest tries
	// 0.10.2 said only that an empty or unknown reply holds a drive delete; the held cases, what the owner then sees
	// and who finishes the delete were missing, so the line now says them.
	"0.10.2": "1255b0d2a9e0e8b382d2f3b3e360f8e179cd3378", // changelog: correct the 0.10.2 line on held drive deletes
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

// changelogLine is a release's minor line, X.Y.
type changelogLine struct{ major, minor int }

// below reports whether line a is older than line b.
func (a changelogLine) below(b changelogLine) bool {
	return a.major < b.major || (a.major == b.major && a.minor < b.minor)
}

// changelogRelease is a parsed X.Y.Z or X.Y.Z-rc.N; rc is 0 for a final.
type changelogRelease struct {
	changelogLine
	patch, rc int
}

var changelogReleaseParts = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?$`)

// parseChangelogRelease reads a version in the changelogVersion grammar.
func parseChangelogRelease(v string) (changelogRelease, bool) {
	m := changelogReleaseParts.FindStringSubmatch(v)
	if m == nil {
		return changelogRelease{}, false
	}
	n := func(s string) int {
		i, _ := strconv.Atoi(s)
		return i
	}
	r := changelogRelease{changelogLine: changelogLine{n(m[1]), n(m[2])}, patch: n(m[3])}
	if m[4] != "" {
		r.rc = n(m[4])
	}
	return r, true
}

// after reports whether release a is newer than b. A final is newer than each of its candidates.
func (a changelogRelease) after(b changelogRelease) bool {
	if a.changelogLine != b.changelogLine {
		return b.changelogLine.below(a.changelogLine)
	}
	if a.patch != b.patch {
		return a.patch > b.patch
	}
	key := func(rc int) int {
		if rc == 0 {
			return math.MaxInt
		}
		return rc
	}
	return key(a.rc) > key(b.rc)
}

// changelogMaintainedLines are the minor lines below this tree's own that still get patch releases. A tag of such a
// line, like a tag of the tree's own line, needs its section only once this tree has merged it or a newer tag of its
// line: a patch tagged since the last forward-port is not ported yet. "0.10" joins in the release commit that moves
// this tree to 0.11 (0.11.0-rc.1) and leaves when 0.10.x stops getting patches, about four weeks after 0.11.0. When
// the tree moves to 0.12, "0.11" joins in the commit that opens 0.12, with a case in
// TestChangelogTagGuardFollowsVersionLines. A line forgotten here is loud: its next unported tag reds the guard.
var changelogMaintainedLines = []string{}

// changelogLines parses this tree's version and the maintained list, which may name only lines below it.
func changelogLines(own string, maintained []string) (changelogRelease, []changelogLine, error) {
	r, ok := parseChangelogRelease(own)
	if !ok {
		return r, nil, fmt.Errorf("this tree's version %q is not X.Y.Z or X.Y.Z-rc.N", own)
	}
	var lines []changelogLine
	for _, s := range maintained {
		var l changelogLine
		if n, err := fmt.Sscanf(s, "%d.%d", &l.major, &l.minor); err != nil || n != 2 || fmt.Sprintf("%d.%d", l.major, l.minor) != s {
			return r, nil, fmt.Errorf("changelogMaintainedLines entry %q is not X.Y", s)
		}
		if !l.below(r.changelogLine) {
			return r, nil, fmt.Errorf("changelogMaintainedLines entry %q is not below this tree's line %d.%d", s, r.major, r.minor)
		}
		lines = append(lines, l)
	}
	return r, lines, nil
}

// changelogTagsMissingSection returns the release tags, final or candidate, from v0.4.0 on, that need a section in
// sections and have none. Tags older than v0.4.0 live in CHANGELOG-ARCHIVE.md, and anything that is not a release
// tag is ignored. own is this tree's version and merged the tags HEAD has merged.
//   - A tag of a line above own's is skipped: a newer tree released it.
//   - A tag of own's line, or of a maintained line, needs its section at or below that line's watermark, the newest
//     merged tag of the line. Above it the tag is not ported yet; with no merged tag of the line, none of its tags is.
//   - A tag of any other lower line (a closed line) always needs its section, merged or not. Ancestry alone is not
//     the rule: released tags of old release branches (v0.7.11-13, v0.8.1-9, v0.9.4-9) are not ancestors of
//     v0.10.2, and their sections are exactly what a move could lose.
func changelogTagsMissingSection(tags, merged []string, own changelogRelease, maintained []changelogLine, sections map[string]changelogSection) []string {
	tracked := map[changelogLine]bool{own.changelogLine: true}
	for _, l := range maintained {
		tracked[l] = true
	}
	watermark := map[changelogLine]changelogRelease{}
	for _, tag := range merged {
		m := changelogTag.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		if r, ok := parseChangelogRelease(m[1]); ok {
			if w, seen := watermark[r.changelogLine]; !seen || r.after(w) {
				watermark[r.changelogLine] = r
			}
		}
	}
	var missing []string
	for _, tag := range tags {
		m := changelogTag.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		r, ok := parseChangelogRelease(m[1])
		if !ok || (r.major == 0 && r.minor < 4) {
			continue // older than v0.4.0: CHANGELOG-ARCHIVE.md
		}
		if own.changelogLine.below(r.changelogLine) {
			continue
		}
		if tracked[r.changelogLine] {
			if w, ok := watermark[r.changelogLine]; !ok || r.after(w) {
				continue
			}
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
// Tags of a newer line, and tags of this tree's own or a maintained line it has not merged yet, are
// skipped (changelogTagsMissingSection).
func TestChangelogEveryTaggedReleaseHasASection(t *testing.T) {
	root := repoRoot(t)
	all, err := exec.Command("git", "-C", root, "tag", "-l", "v*").Output()
	if err != nil {
		t.Fatalf("git tag -l: %v", err)
	}
	merged, err := exec.Command("git", "-C", root, "tag", "--merged", "HEAD", "-l", "v*").Output()
	if err != nil {
		t.Fatalf("git tag --merged HEAD: %v", err)
	}
	own, maintained, err := changelogLines(shippedversion.Version, changelogMaintainedLines)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range changelogTagsMissingSection(strings.Fields(string(all)), strings.Fields(string(merged)), own, maintained, allChangelogSections(t)) {
		t.Errorf("tag %s has no \"## [%s]\" section in CHANGELOG.md or docs/releases/*.md — a released section was dropped, "+
			"or the tag's line still gets patches and belongs in changelogMaintainedLines", tag, tag[1:])
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
	if missing := changelogTagsMissingSection(tags, tags, mustChangelogRelease(t, "1.2.3"), nil, have); len(missing) != 0 {
		t.Errorf("tags with sections, pre-0.4.0 tags or non-release tags reported missing: %v", missing)
	}
	delete(have, "1.2.3-rc.1")
	if missing := changelogTagsMissingSection(tags, tags, mustChangelogRelease(t, "1.2.3"), nil, have); len(missing) != 1 || missing[0] != "v1.2.3-rc.1" {
		t.Errorf("a candidate tag without a section: missing = %v, want [v1.2.3-rc.1]", missing)
	}
}

func mustChangelogRelease(t *testing.T, v string) changelogRelease {
	t.Helper()
	r, ok := parseChangelogRelease(v)
	if !ok {
		t.Fatalf("%q is not a release version", v)
	}
	return r
}

// TestChangelogTagGuardFollowsVersionLines proves the line rule and the watermark: a tag of a newer line is skipped,
// an unported tag of the tree's own or a maintained line is skipped, and everything else still needs its section.
func TestChangelogTagGuardFollowsVersionLines(t *testing.T) {
	cases := []struct {
		name       string
		own        string
		maintained []string
		tags       string
		merged     string
		sections   string
		want       string
	}{
		{"release/0.10 with an 0.11 candidate", "0.10.4", nil, "v0.10.3 v0.10.4 v0.11.0-rc.1", "v0.10.3 v0.10.4", "0.10.3 0.10.4", ""},
		{"main with an unported patch", "0.10.2", nil, "v0.10.2 v0.10.3", "v0.10.2", "0.10.2", ""},
		{"the patch merged, its section lost", "0.10.2", nil, "v0.10.2 v0.10.3", "v0.10.2 v0.10.3", "0.10.2", "v0.10.3"},
		{"a closed line's non-ancestor tag with its section", "0.11.0-rc.1", []string{"0.10"}, "v0.9.9 v0.10.4 v0.11.0-rc.1", "v0.10.4 v0.11.0-rc.1", "0.9.9 0.10.4 0.11.0-rc.1", ""},
		{"the same without its section", "0.11.0-rc.1", []string{"0.10"}, "v0.9.9 v0.10.4 v0.11.0-rc.1", "v0.10.4 v0.11.0-rc.1", "0.10.4 0.11.0-rc.1", "v0.9.9"},
		{"a maintained line's unported tag", "0.11.0-rc.1", []string{"0.10"}, "v0.10.4 v0.10.5 v0.11.0-rc.1", "v0.10.4 v0.11.0-rc.1", "0.10.4 0.11.0-rc.1", ""},
		{"the same tag merged without its section", "0.11.0-rc.1", []string{"0.10"}, "v0.10.4 v0.10.5 v0.11.0-rc.1", "v0.10.4 v0.10.5 v0.11.0-rc.1", "0.10.4 0.11.0-rc.1", "v0.10.5"},
		{"0.10 left off the list is loud", "0.11.0-rc.1", nil, "v0.10.4 v0.10.5 v0.11.0-rc.1", "v0.10.4 v0.11.0-rc.1", "0.10.4 0.11.0-rc.1", "v0.10.5"},
		{"an own-line candidate below the watermark, not an ancestor, without its section", "0.10.2", nil, "v0.10.0-rc.26 v0.10.2", "v0.10.2", "0.10.2", "v0.10.0-rc.26"},
		{"the release commit before its tag", "0.11.0-rc.1", []string{"0.10"}, "v0.10.4", "v0.10.4", "0.10.4", ""},
		{"a final outranks its candidates", "0.10.2", nil, "v0.10.2-rc.1 v0.10.2", "v0.10.2-rc.1", "0.10.2-rc.1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			own, maintained, err := changelogLines(c.own, c.maintained)
			if err != nil {
				t.Fatal(err)
			}
			have := map[string]changelogSection{}
			for _, v := range strings.Fields(c.sections) {
				have[v] = changelogSection{}
			}
			got := changelogTagsMissingSection(strings.Fields(c.tags), strings.Fields(c.merged), own, maintained, have)
			if strings.Join(got, " ") != c.want {
				t.Errorf("missing = %v, want [%s]", got, c.want)
			}
		})
	}
}

// TestChangelogMaintainedLinesAreBelowTheTree keeps the maintained list valid: only X.Y entries below the tree's line.
func TestChangelogMaintainedLinesAreBelowTheTree(t *testing.T) {
	for _, c := range []struct {
		own        string
		maintained []string
		wantErr    bool
	}{
		{"0.10.2", []string{"0.10"}, true},
		{"0.11.0-rc.1", []string{"0.10"}, false},
		{"0.11.0-rc.1", []string{"0.1x"}, true},
		{"0.11.0-rc.1", []string{"0.12"}, true},
		{"0.11", nil, true},
	} {
		_, lines, err := changelogLines(c.own, c.maintained)
		if (err != nil) != c.wantErr {
			t.Errorf("changelogLines(%q, %v) error = %v, want error %v", c.own, c.maintained, err, c.wantErr)
		}
		if c.own == "0.11.0-rc.1" && !c.wantErr && (len(lines) != 1 || lines[0] != (changelogLine{0, 10})) {
			t.Errorf("changelogLines(%q, %v) lines = %v, want [0.10]", c.own, c.maintained, lines)
		}
	}
	if _, _, err := changelogLines(shippedversion.Version, changelogMaintainedLines); err != nil {
		t.Errorf("the live maintained list is invalid: %v", err)
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

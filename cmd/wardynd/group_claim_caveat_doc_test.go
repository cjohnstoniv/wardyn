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

// shrinkTheClaimRE matches any recommendation of the Entra option that emits a
// SMALLER group claim. Both spellings, because operators meet the manifest value
// and the portal label in different places.
var shrinkTheClaimRE = regexp.MustCompile(`(?i)ApplicationGroup|groups assigned to the application`)

// caveatWindow is how far either side of such a mention the nested-membership
// caveat must appear. One markdown paragraph is well inside this.
const caveatWindow = 1600

// reKeyProcedureRE opens docs/OPERATIONS.md's re-key-before-you-change
// procedure: a bolded lead-in bullet, capturing its indent so the block ends at
// the next sibling bullet and not at the sub-list nested inside it.
var reKeyProcedureRE = regexp.MustCompile(`(?m)^([ \t]*)-[ \t]+\*\*[^*]*re-key[^*]*\*\*`)

// TestGroupClaimWorkaroundKeepsItsCaveat asserts, over EVERY markdown file in
// the repository, that nothing recommends shrinking the Entra group claim
// without saying what it drops.
//
// This is deliberately repo-wide rather than a list of files to check. A guard
// that names the documents it knows about is blind to the next one added, and
// this claim is exactly the sort that gets restated in a new runbook, a chart
// README or an onboarding guide. The invariant is stateable over all markdown,
// so it is stated that way; the excluded directories are vendored trees, not
// content of ours.
//
// Why it matters here specifically: an overage is a DETECTED failure (the claim
// is withheld, sessionGroups marks the snapshot truncated, the resolver refuses).
// A claim the IdP was configured to narrow is complete by the IdP's account and
// sets no bit at all, so a governance assignment or group DENY row keyed on a
// group that is no longer emitted stops applying in silence. Our own operator
// advice recommended that switch bare — the one failure mode the truncation bit
// exists to make visible, recommended with the detector off.
func TestGroupClaimWorkaroundKeepsItsCaveat(t *testing.T) {
	root := repoRoot(t)
	checked, mentions := 0, 0

	// Tracked files, not a filesystem walk: see trackedMarkdown. A gitignored
	// planning note that recommends the workaround is not documentation this
	// product ships, and grading it is how a guard earns a reputation for noise.
	for _, rel := range trackedMarkdown(t, root) {
		b, rerr := os.ReadFile(filepath.Join(root, rel))
		if os.IsNotExist(rerr) {
			continue // tracked but deleted in this worktree
		}
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		checked++
		src := string(b)
		for _, loc := range shrinkTheClaimRE.FindAllStringIndex(src, -1) {
			mentions++
			if !strings.Contains(strings.ToLower(windowAt(src, loc[0], caveatWindow)), "nested") {
				t.Errorf("%s:%d recommends the smaller Entra group claim without the nested-membership caveat — "+
					"that option excludes nested groups, and a claim the IdP filtered sets NO truncation bit, so a "+
					"group-keyed grant or assignment stops applying silently",
					rel, 1+strings.Count(src[:loc[0]], "\n"))
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d tracked markdown files scanned — the enumeration, not the docs, is what changed", checked)
	}
	if mentions == 0 {
		t.Skip("no document mentions the workaround any more; nothing to caveat")
	}
}

// TestGroupClaimOverageWarningAndProcedure is the rest of what the caveat above
// protects, stated in terms a rewording keeps: the warning says a filtered claim
// is silent, the re-key-before-you-change procedure is present, and the §5
// residual publishes the fact and links that procedure.
//
// Every requirement below is a bullet or an API identifier, never a sentence. An
// operator who has already reviewed the warning can reword it without failing a
// build; deleting the warning, or the procedure, does fail.
//
// The code premise these documents rest on — a claim the IdP FILTERED stamps no
// bit — is pinned next to the code by BEHAVIOUR
// (TestFilteredGroupClaimReadsComplete, internal/auth/oidc) rather than by
// reading sessionGroups' source: a comment inside it is not a detection
// algorithm, and a guard that reads source prose can only ever be satisfied by
// leaving the source alone.
func TestGroupClaimOverageWarningAndProcedure(t *testing.T) {
	ops := readDoc(t, "docs/OPERATIONS.md")

	// The WARNING, read around the option's own value so an unrelated paragraph
	// about groups cannot satisfy it: a claim the IdP filtered is COMPLETE by its
	// account, which is exactly why losing a group from it is silent.
	if !slices.ContainsFunc(shrinkTheClaimRE.FindAllStringIndex(ops, -1), func(loc []int) bool {
		around := strings.ToLower(windowAt(ops, loc[0], caveatWindow))
		return strings.Contains(around, "filter") && strings.Contains(around, "complete")
	}) {
		t.Error("docs/OPERATIONS.md no longer says a claim the IdP FILTERED is COMPLETE by its account — that " +
			"silence is what the nested-membership caveat and the re-key procedure exist for")
	}

	// The PROCEDURE: the bolded lead-in bullet opens a block of ordered steps,
	// and each term is an identifier the operator reads back rather than prose
	// they can rephrase.
	proc := overageProcedure(t)
	for _, want := range []string{
		"subject_type=group", // enumerate what resolves by group today
		"deny",               // deny rows first — a lost DENY widens somebody
		"get /permissions",
		"get /governance",      // and every group-tier assignment
		"get /me/capabilities", // verify with a real login, not by reading the IdP's UI
		"session_groups",       // ...and read the snapshot that login's token produced
		"post /governance/preview",
	} {
		if !strings.Contains(proc, want) {
			t.Errorf("docs/OPERATIONS.md's re-key procedure no longer names %q — this is the burden "+
				"threatmodel/THREAT-MODEL.md §5 shifts to the operator", want)
		}
	}

	// And the residual that publishes the ceiling points at that procedure.
	for _, block := range strings.Split(readRepo(t, "threatmodel/THREAT-MODEL.md"), "\n\n") {
		flat := strings.ToLower(strings.Join(strings.Fields(block), " "))
		if strings.Contains(flat, "filter") && strings.Contains(flat, "re-key") &&
			strings.Contains(flat, "docs/operations.md") {
			return
		}
	}
	t.Error("threatmodel/THREAT-MODEL.md §5 has no residual that publishes the filtered-claim ceiling AND " +
		"links docs/OPERATIONS.md for the procedure")
}

// overageProcedure returns docs/OPERATIONS.md's re-key-before-you-change
// procedure: the bolded lead-in bullet and everything indented under it, up to
// the next sibling bullet. A bolded lead-in is this document's sub-heading, so
// matching one keeps the guard on the block rather than on the prose.
func overageProcedure(t *testing.T) string {
	t.Helper()
	doc := readRepo(t, "docs/OPERATIONS.md")
	m := reKeyProcedureRE.FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("docs/OPERATIONS.md no longer leads the re-key procedure with a bolded bullet — re-read the overage remedy")
	}
	rest := doc[strings.Index(doc, m[0])+len(m[0]):]
	block := m[0] + rest
	if end := strings.Index(rest, "\n"+m[1]+"- "); end >= 0 {
		block = m[0] + rest[:end]
	}
	return strings.ToLower(strings.Join(strings.Fields(block), " "))
}

// windowAt returns the bytes of src within w either side of loc — the window a
// caveat, or a key term, has to fall inside.
func windowAt(src string, loc, w int) string {
	return src[max(0, loc-w):min(len(src), loc+w)]
}

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

// shrinkTheClaimRE matches any recommendation of the Entra option that emits a
// SMALLER group claim. Both spellings, because operators meet the manifest value
// and the portal label in different places.
var shrinkTheClaimRE = regexp.MustCompile(`(?i)ApplicationGroup|groups assigned to the application`)

// caveatWindow is how far either side of such a mention the nested-membership
// caveat must appear. One markdown paragraph is well inside this.
const caveatWindow = 1600

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
	skipDir := map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true}
	checked, mentions := 0, 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		checked++
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		for _, loc := range shrinkTheClaimRE.FindAllStringIndex(src, -1) {
			mentions++
			lo := max(0, loc[0]-caveatWindow)
			hi := min(len(src), loc[1]+caveatWindow)
			if !strings.Contains(strings.ToLower(src[lo:hi]), "nested") {
				t.Errorf("%s:%d recommends the smaller Entra group claim without the nested-membership caveat — "+
					"that option excludes nested groups, and a claim the IdP filtered sets NO truncation bit, so a "+
					"group-keyed grant or assignment stops applying silently",
					rel, 1+strings.Count(src[:loc[0]], "\n"))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if checked < 20 {
		t.Fatalf("only %d markdown files scanned — the walk, not the docs, is what changed", checked)
	}
	if mentions == 0 {
		t.Skip("no document mentions the workaround any more; nothing to caveat")
	}

	// The code premise: a FILTERED claim sets no bit. sessionGroups returns
	// truncated from the byte cap and the overage pointer, and from nothing
	// else — which is why the loss above is invisible rather than merely
	// undocumented.
	body := sessionGroupsBody(t, root)
	if !strings.Contains(body, "claimNames") {
		t.Error("sessionGroups no longer consults the overage pointer — re-read the overage paragraph before trusting it")
	}
	if !strings.Contains(body, "len(out) < len(uniq)") {
		t.Error("sessionGroups' byte-cap truncation signal changed shape; re-derive this guard")
	}
	if strings.Contains(body, "expected") || strings.Contains(body, "declared") {
		t.Error("sessionGroups appears to compare the claim against something expected — if a filtered claim is now " +
			"detectable, docs/OPERATIONS.md and residual #34 understate what Wardyn can see")
	}

	// And the operator document carries the procedure, not just the warning.
	ops := readDoc(t, "docs/OPERATIONS.md")
	for _, want := range []string{
		"trades a detected failure for an undetected one",
		"re-key before you change the claim",
		"`session_groups` is the snapshot their token actually produced",
	} {
		if !strings.Contains(ops, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("docs/OPERATIONS.md's overage remedy is missing %q", want)
		}
	}
	tm := readDoc(t, "threatmodel/THREAT-MODEL.md")
	if !strings.Contains(tm, "A group claim the IdP FILTERS is indistinguishable from a complete one") {
		t.Error("threatmodel/THREAT-MODEL.md §5 does not publish the filtered-claim residual")
	}
}

// sessionGroupsBody returns the source of oidc.sessionGroups.
func sessionGroupsBody(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "auth", "oidc", "derive.go"))
	if err != nil {
		t.Fatalf("read derive.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "func sessionGroups(")
	if i < 0 {
		t.Fatal("derive.go no longer defines sessionGroups — revisit this guard")
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

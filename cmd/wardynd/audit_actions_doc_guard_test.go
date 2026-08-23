// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// auditActionCitationWindow is how many lines of slack a citation gets around
// its cited line(s) before the guard calls it stale — generous enough that a
// comment shifting a few lines above the real emit site doesn't false-
// positive, tight enough that a genuine merge-shift (the drift class an
// integration verify caught 22 instances of by hand) still fails.
const auditActionCitationWindow = 6

// backtickSpan pulls every `...`-quoted token out of one table row, in order.
var backtickSpan = regexp.MustCompile("`([^`]+)`")

// fileLineCitation is what a citation span looks like: a path ending .go or
// .md, a colon, and one or more line numbers/ranges (comma-separated) — e.g. a
// bare "some/file.go", colon, line 267; or a comma list like SSH.md's
// 239,281,316; or a range like groundtruth.go's 44-64. A bare backtick span
// that doesn't match this (a symbol name, a constant, a plain doc-section
// reference with no line number) is never a citation — see anchors below.
// (Deliberately not written as a real "file.go:NNN" example in this comment —
// TestCommentsCiteSymbolsNotLineNumbers, citation_guard_test.go, bans exactly
// that shape tree-wide, and this file is no exception to its own neighbor.)
var fileLineCitation = regexp.MustCompile(`^([A-Za-z0-9_./-]+\.(?:go|md)):([0-9]+(?:-[0-9]+)?(?:,[0-9]+(?:-[0-9]+)?)*)$`)

// lineGroup is one cited line or line range, expanded from a citation's line
// spec ("135" or "44-64").
type lineGroup struct{ lo, hi int }

// expandLineSpec turns a citation's line spec into the groups it names — a
// comma-separated citation ("239,281,316") makes three independent
// single-line groups, each checked in its own small window, so three
// unrelated mentions in the same doc are each held to the window rather than
// one wide OR across all three that a genuinely stale citation could hide in.
// A range ("44-64") makes ONE group spanning the whole range plus padding.
func expandLineSpec(spec string) []lineGroup {
	var out []lineGroup
	for _, part := range strings.Split(spec, ",") {
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(lo)
			b, err2 := strconv.Atoi(hi)
			if err1 == nil && err2 == nil {
				out = append(out, lineGroup{a, b})
				continue
			}
		}
		if n, err := strconv.Atoi(part); err == nil {
			out = append(out, lineGroup{n, n})
		}
	}
	return out
}

// TestAuditActionsDocCitationsAreLive parses every table row of
// docs/AUDIT-ACTIONS.md, extracts each backtick `file:line` citation the row
// makes, and asserts the row's action literal — or another backtick-quoted
// symbol from the SAME CELL as the citation, the (`symbolName`,
// `file.go:N`) shape several rows use for a helper the action literal itself
// never appears next to (e.g. `sandbox.orphan_sweep`'s `stopSandboxOrAudit`
// citation) — appears within auditActionCitationWindow lines of the cited
// line, in the cited file.
//
// Anchors are scoped to the citation's OWN table cell, not the whole row: the
// Data-fields cell's `dropped`/`host`/`reason`-style field names are common
// enough English words that pooling them across the whole row let a genuinely
// stale Where-cell citation hide behind an unrelated coincidental match (an
// early version of this guard missed `authz.denied`'s stale citation into
// docs/OPERATIONS.md this exact way, because `reason` — one of ITS Data
// fields — happened to also appear near the wrong line the citation had
// drifted to).
//
// This closes the gap the doc's own header names ("There is no CI check
// tying this file to the source... a new action can go undocumented — that
// is a known gap"): a citation rots silently the moment the cited file grows
// or shrinks above the cited line, and nothing before this caught it (an
// integration verify caught 22 such instances by hand). The matcher is
// exact about the window, so a citation whose line has genuinely drifted out
// from under it still fails — which is what caught (and this file then
// fixed) the `ssh.auth`/`ssh.exec`/`ssh.sftp` and `authz.denied` citations
// into docs/SSH.md and docs/OPERATIONS.md at HEAD.
func TestAuditActionsDocCitationsAreLive(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "AUDIT-ACTIONS.md"))
	if err != nil {
		t.Fatalf("read docs/AUDIT-ACTIONS.md: %v", err)
	}
	docLines := strings.Split(string(raw), "\n")

	fileCache := map[string][]string{} // cited path -> its lines, loaded once
	getLines := func(rel string) ([]string, error) {
		if ls, ok := fileCache[rel]; ok {
			return ls, nil
		}
		b, rerr := os.ReadFile(filepath.Join(root, rel))
		if rerr != nil {
			return nil, rerr
		}
		ls := strings.Split(string(b), "\n")
		fileCache[rel] = ls
		return ls, nil
	}

	rowsChecked, citationsChecked := 0, 0
	for i, line := range docLines {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue // only table rows carry citations; prose is out of scope
		}
		cells := strings.Split(line, "|")
		// The row's FIRST cell carrying a backtick span is always
		// `action.literal` — every real data row leads with it; header
		// ("Action") and separator ("---") rows carry no backticks at all, so
		// they never set an action literal and are skipped below.
		var actionLiteral string
		for _, cell := range cells {
			if m := backtickSpan.FindStringSubmatch(cell); m != nil {
				actionLiteral = m[1]
				break
			}
		}
		if actionLiteral == "" {
			continue
		}
		docLineNo := i + 1
		// A "kind.*" wildcard action (llm.scan.*, egress.*) is never the literal
		// string on the wire — the real Action is "kind."+suffix — so match the
		// prefix, not the asterisk.
		searchTerm := strings.TrimSuffix(actionLiteral, "*")

		rowHasCitation := false
		for _, cell := range cells {
			spans := backtickSpan.FindAllStringSubmatch(cell, -1)
			if len(spans) == 0 {
				continue
			}
			var cellAnchors []string
			var cellCitations [][2]string // {path, lineSpec}
			for _, sp := range spans {
				if m := fileLineCitation.FindStringSubmatch(sp[1]); m != nil {
					cellCitations = append(cellCitations, [2]string{m[1], m[2]})
				} else {
					cellAnchors = append(cellAnchors, sp[1])
				}
			}
			if len(cellCitations) == 0 {
				continue
			}
			rowHasCitation = true

			for _, c := range cellCitations {
				path, lineSpec := c[0], c[1]
				cited, gerr := getLines(path)
				if gerr != nil {
					t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s:%s, but %s does not exist: %v",
						docLineNo, actionLiteral, path, lineSpec, path, gerr)
					continue
				}
				for _, g := range expandLineSpec(lineSpec) {
					citationsChecked++
					lo, hi := g.lo-auditActionCitationWindow, g.hi+auditActionCitationWindow
					if lo < 1 {
						lo = 1
					}
					if hi > len(cited) {
						hi = len(cited)
					}
					if lo > len(cited) {
						t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s:%d, past the end of %s (%d lines)",
							docLineNo, actionLiteral, path, g.lo, path, len(cited))
						continue
					}
					window := strings.Join(cited[lo-1:hi], "\n")
					if strings.Contains(window, searchTerm) {
						continue
					}
					found := false
					for _, a := range cellAnchors {
						// A dotted anchor (`egress.Decision`) is written
						// package-qualified in the doc's prose, but a symbol
						// defined IN that package never repeats its own
						// package name at the definition site — so the bare
						// tail after the last "." is accepted too.
						if strings.Contains(window, a) {
							found = true
							break
						}
						if idx := strings.LastIndex(a, "."); idx >= 0 && strings.Contains(window, a[idx+1:]) {
							found = true
							break
						}
					}
					if found {
						continue
					}
					spec := strconv.Itoa(g.lo)
					if g.hi != g.lo {
						spec = strconv.Itoa(g.lo) + "-" + strconv.Itoa(g.hi)
					}
					t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s:%s, but neither the action literal "+
						"%q nor any of that cell's other cited symbols %v appear within %d lines of it in %s — "+
						"the citation has rotted (re-point it at the real emit site)",
						docLineNo, actionLiteral, path, spec, actionLiteral, cellAnchors, auditActionCitationWindow, path)
				}
			}
		}
		if rowHasCitation {
			rowsChecked++
		}
	}
	if rowsChecked == 0 {
		t.Fatal("checked 0 rows with a file:line citation — the parser or the doc's table shape changed")
	}
	t.Logf("checked %d citations across %d rows", citationsChecked, rowsChecked)
}

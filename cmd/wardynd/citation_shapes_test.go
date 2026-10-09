// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"testing"
)

// TestCitationShapes drives checkDocCitations — the code the threat-model
// citation guard runs — over fixture lines, both ways: a link-wrapped citation
// is still resolved, and still fails when it names the wrong symbol or a file
// that is not there.
func TestCitationShapes(t *testing.T) {
	files := map[string]string{
		"internal/x/f.go": "package x\n\nfunc RealSym() {}\n",
	}
	resolve := func(cited string) (string, bool) {
		src, ok := files[cited]
		return src, ok
	}
	cases := []struct {
		name         string
		line         string
		paths, pairs int
		fails        bool
	}{
		{"plain symbol then path", "`RealSym` in `internal/x/f.go`", 1, 1, false},
		{"plain path then symbol", "(`internal/x/f.go`, `RealSym`)", 1, 1, false},
		{"linked symbol then path", "`RealSym` in [`internal/x/f.go`](../internal/x/f.go)", 1, 1, false},
		{"linked path then symbol", "([`internal/x/f.go`](../internal/x/f.go), `RealSym`)", 1, 1, false},
		{"linked, qualified symbol", "`x.RealSym` — [`internal/x/f.go`](../internal/x/f.go)", 1, 1, false},
		{"linked symbol then path, wrong symbol", "`GoneSym` in [`internal/x/f.go`](../internal/x/f.go)", 1, 1, true},
		{"linked path then symbol, wrong symbol", "([`internal/x/f.go`](../internal/x/f.go), `GoneSym`)", 1, 1, true},
		{"linked path to a missing file", "`RealSym` in [`internal/x/gone.go`](../internal/x/gone.go)", 1, 0, true},
		{"linked bare missing file", "see [`gone.go`](../internal/x/gone.go)", 1, 0, true},
		{"path#Sym span is no citation", "[`internal/x/f.go#RealSym`](../internal/x/f.go)", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errs []string
			errorf := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
			paths, pairs := checkDocCitations("fixture.md", tc.line, resolve, errorf)
			if paths != tc.paths || pairs != tc.pairs {
				t.Errorf("%q: checked %d paths and %d pairs, want %d and %d", tc.line, paths, pairs, tc.paths, tc.pairs)
			}
			if got := len(errs) > 0; got != tc.fails {
				t.Errorf("%q: failed=%v (%v), want failed=%v", tc.line, got, errs, tc.fails)
			}
		})
	}
}

// TestCitationFloorStillTrips proves the vacuity floor survives the widening:
// a doc whose only citations are in a shape the guard does not read checks
// nothing and must fail, and a doc of link-wrapped pairs counts.
func TestCitationFloorStillTrips(t *testing.T) {
	resolve := func(cited string) (string, bool) {
		if cited == "internal/x/f.go" {
			return "func RealSym() {}", true
		}
		return "", false
	}
	ignore := func(string, ...any) {}
	docs := map[string]bool{
		"no citations at all":         true,
		"`internal/x/f.go` path only": true,
		"[`internal/x/f.go#RealSym`](../internal/x/f.go) path#Sym only": true,
		"`RealSym` in [`internal/x/f.go`](../internal/x/f.go)":          false,
	}
	for doc, vacuous := range docs {
		paths, pairs := checkDocCitations("fixture.md", doc, resolve, ignore)
		if got := citationsVacuous(paths, pairs); got != vacuous {
			t.Errorf("%q: vacuous=%v (paths %d, pairs %d), want %v", doc, got, paths, pairs, vacuous)
		}
	}
}

// TestLineCitationStillBanned: link-wrapping does not hide a line number from
// the threat-model and USERS.md line guards, in the link text or its target.
func TestLineCitationStillBanned(t *testing.T) {
	for _, line := range []string{
		"see `internal/x/f.go:123`",
		"see [`internal/x/f.go:123`](../internal/x/f.go)",
		"`RealSym` in [`f.go:123`](../internal/x/f.go)",
		"`RealSym` in [`internal/x/f.go`](../internal/x/f.go#L120)",
	} {
		if lineCitation.FindString(line) == "" {
			t.Errorf("%q: line citation not caught", line)
		}
	}
	for _, line := range []string{
		"`RealSym` in [`internal/x/f.go`](../internal/x/f.go)",
		"[`internal/x/f.go#Load`](../internal/x/f.go)",
		"[`docs/x.md#L1-scope`](../docs/x.md#L1-scope)",
	} {
		if m := lineCitation.FindString(line); m != "" {
			t.Errorf("%q: symbol citation flagged as a line citation (%q)", line, m)
		}
	}
}

// TestHeadingSlugMatchesGitHub pins headingSlug to GitHub's anchors: "--" and
// "_" kept, nothing collapsed or trimmed, and the anchors the audit-actions
// guard resolves today spelled as before.
func TestHeadingSlugMatchesGitHub(t *testing.T) {
	for heading, want := range map[string]string{
		"Retention, erasure and GDPR — a residual, not a solved problem": "retention-erasure-and-gdpr--a-residual-not-a-solved-problem",
		"push_rules — PushRulesSpec":                                     "push_rules--pushrulesspec",
		"Bounds":                                                         "bounds",
		"Every denial that isn't a 404":                                  "every-denial-that-isnt-a-404",
		"`wardynd` (control plane)":                                      "wardynd-control-plane",
	} {
		if got := headingSlug(heading); got != want {
			t.Errorf("headingSlug(%q) = %q, want %q", heading, got, want)
		}
	}
}

// TestAuditActionsDocAnchorsResolve: the .md#anchor citations in
// docs/AUDIT-ACTIONS.md still name a section under the GitHub slugger.
func TestAuditActionsDocAnchorsResolve(t *testing.T) {
	for _, c := range []struct{ doc, anchor string }{
		{"docs/SSH.md", "bounds"},
		{"docs/OPERATIONS.md", "every-denial-that-isnt-a-404"},
		{"docs/ENV.md", "wardynd-control-plane"},
	} {
		bodies, _, _, err := citedSymbolBodies(c.doc, []byte(readRepo(t, c.doc)))
		if err != nil {
			t.Fatalf("%s: %v", c.doc, err)
		}
		if _, ok := bodies[c.anchor]; !ok {
			t.Errorf("%s#%s no longer resolves", c.doc, c.anchor)
		}
	}
}

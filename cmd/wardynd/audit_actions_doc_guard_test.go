// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// symbolCitation is what a citation in docs/AUDIT-ACTIONS.md looks like now: a
// path ending .go or .md, a "#", and the SYMBOL the claim is about — a
// top-level func (`resolveRunUpstreamProxy`), a method spelled
// `Type.Method` (`Server.reconcileOrphanedSandbox`), a package-level
// const/var/type, or, for a markdown file, the GitHub slug of the heading whose
// section carries the claim. Several symbols in one file are comma-separated,
// the way the old form comma-separated its line numbers.
//
// This replaced a line-anchored form, and the replacement is the whole point of
// the shape. The old citations were checked at a window of ZERO — a citation had
// to name the exact line carrying the action literal — which made the required
// build check a hostage of every insertion anywhere above any of 209 cited
// lines: a one-line comment added at the top of a heavily-cited file reddened a
// gate that had nothing to do with the change, and the repair was to re-point
// citations by hand in the same PR. Meanwhile the tree's own rule for Go
// comments and for threatmodel/ (TestCommentsCiteSymbolsNotLineNumbers,
// TestSecurityDocsCiteSymbolsNotLineNumbers) BANS pinning a claim to a line
// number, for the reason a refactor proves every time: a symbol name survives a
// move, a line number does not. This table was the one place in the tree that
// mandated the banned shape. It no longer does.
//
// What the gate loses, stated plainly: the claim weakens from "this exact LINE
// emits this action" to "this SYMBOL emits this action". What it keeps is the
// part that was load-bearing — the literal must appear INSIDE the cited
// symbol's own body, never merely somewhere in the file — so a citation that
// has drifted onto the wrong function still fails, which is the drift that
// actually happened (see the ssh.auth/authz.denied repairs this guard's
// line-anchored ancestor made). What it stops punishing is an edit that moved
// the symbol without changing it.
var symbolCitation = regexp.MustCompile(`^([A-Za-z0-9_./-]+\.(?:go|md))#([A-Za-z0-9_.-]+(?:,[A-Za-z0-9_.-]+)*)$`)

// backtickSpan pulls every `...`-quoted token out of one table row, in order.
var backtickSpan = regexp.MustCompile("`([^`]+)`")

// barePathCitation is the SAME claim with the "#symbol" left off — "this action is
// emitted in this file" — and it was invisible to this guard once. The suffix
// above is REQUIRED, so a backtick span holding a bare path matched nothing,
// cellCitations stayed empty and the whole row was `continue`d past: it counted
// in neither the rows nor the citations the guard reports, so a doc where EVERY
// citation lost its suffix would pass while claiming to check citations.
// Twenty-one rows cite this way today, 34 citations in all, and one had
// already rotted.
//
// It is checked in the only way a symbol-less citation can be: the file must
// EXIST, and the row's action literal (or one of that cell's other cited
// symbols) must appear SOMEWHERE in it — no symbol scoping, because there is no
// symbol to scope to. Weaker than the symbol-checked path by construction, and
// still the difference between "resolved" and "never looked at".
var barePathCitation = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.(?:go|md)$`)

// constNamesFor inverts the tree's package-level string constants: action value
// -> the names that hold it. An emit whose action arrives as a named constant
// (`ruleSourceGit`, `ruleSourcePATDenied`) spells nothing of the action at the
// emit site, so "the literal is in the body" would be false for a perfectly
// honest citation. The constant standing in for it is the same claim.
func constNamesFor(tr auditTree) map[string][]string {
	byValue := map[string][]string{}
	for name, values := range tr.consts {
		for _, v := range values {
			byValue[v] = append(byValue[v], name)
		}
	}
	return byValue
}

// TestAuditActionsDocCitationsAreLive parses every table row of
// docs/AUDIT-ACTIONS.md, extracts each backtick `file#symbol` citation the row
// makes, resolves the symbol with go/parser (or, in a markdown file, by heading
// slug), and asserts the row's action literal — or a constant holding it, or
// another backtick-quoted symbol from the SAME CELL as the citation, the
// (`symbolName`, `file.go#Symbol`) shape several rows use for a helper the
// action literal itself never appears next to — occurs INSIDE that symbol's
// body.
//
// Inside the body, not anywhere in the file, is the whole of what makes this a
// check rather than a formality: "some function in runs_dispatch.go emits
// run.dispatch" is true of the file no matter which function the citation
// names, and a citation that survives naming the wrong function is a citation
// nobody can use.
//
// An anchor that merely repeats the cited symbol's own name is NOT evidence —
// resolving the citation already proved that symbol exists — so it is dropped
// from the anchor set before the body is searched. Otherwise every
// (`fooHelper`, `pkg/f.go#fooHelper`) row would satisfy itself.
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
// is a known gap"): a citation rots silently the moment the symbol it names
// stops emitting the action, and nothing before this caught it (an
// integration verify caught 22 such instances by hand).
func TestAuditActionsDocCitationsAreLive(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "AUDIT-ACTIONS.md"))
	if err != nil {
		t.Fatalf("read docs/AUDIT-ACTIONS.md: %v", err)
	}
	docLines := strings.Split(string(raw), "\n")
	constHolders := constNamesFor(parseAuditTree(t, root))

	srcCache := map[string]string{}             // cited path -> whole file
	bodyCache := map[string]map[string]string{} // cited path -> symbol -> body
	load := func(rel string) (string, map[string]string, error) {
		if s, ok := srcCache[rel]; ok {
			return s, bodyCache[rel], nil
		}
		b, rerr := os.ReadFile(filepath.Join(root, rel))
		if rerr != nil {
			return "", nil, rerr
		}
		bodies, perr := citedSymbolBodies(rel, b)
		if perr != nil {
			return "", nil, fmt.Errorf("parse %s: %w", rel, perr)
		}
		srcCache[rel], bodyCache[rel] = string(b), bodies
		return srcCache[rel], bodies, nil
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
		// The names any constant holding this action goes by, so an emit that
		// passes `ruleSourcePrivateIP` counts as spelling builtin:private-ip.
		holders := constHolders[searchTerm]

		rowHasCitation := false
		for _, cell := range cells {
			spans := backtickSpan.FindAllStringSubmatch(cell, -1)
			if len(spans) == 0 {
				continue
			}
			var cellAnchors []string
			var cellCitations [][2]string // {path, symbolSpec}
			var cellBarePaths []string
			for _, sp := range spans {
				switch {
				case symbolCitation.MatchString(sp[1]):
					m := symbolCitation.FindStringSubmatch(sp[1])
					cellCitations = append(cellCitations, [2]string{m[1], m[2]})
				case barePathCitation.MatchString(sp[1]):
					cellBarePaths = append(cellBarePaths, sp[1])
					// ALSO an anchor: a bare path names a file, and the
					// symbol-checked branch already treats a path-shaped anchor
					// as a resolvable name rather than a symbol.
					cellAnchors = append(cellAnchors, sp[1])
				default:
					cellAnchors = append(cellAnchors, sp[1])
				}
			}
			// An anchor that just re-states a cited symbol proves nothing the
			// resolution has not already proved. Drop it (and its bare tail,
			// since `Server.foo` is cited and written `foo` in prose).
			cited := map[string]bool{}
			for _, c := range cellCitations {
				for _, sym := range strings.Split(c[1], ",") {
					cited[sym] = true
					if idx := strings.LastIndex(sym, "."); idx >= 0 {
						cited[sym[idx+1:]] = true
					}
				}
			}
			evidence := cellAnchors[:0:0]
			for _, a := range cellAnchors {
				if !cited[a] {
					evidence = append(evidence, a)
				}
			}

			for _, path := range cellBarePaths {
				rowHasCitation = true
				citationsChecked++
				body, _, gerr := load(path)
				if gerr != nil {
					t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s, but %s could not be read: %v",
						docLineNo, actionLiteral, path, path, gerr)
					continue
				}
				if strings.Contains(body, searchTerm) {
					continue
				}
				found := false
				for _, a := range evidence {
					if a != path && strings.Contains(body, a) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s (no symbol), but neither the action literal %q "+
						"nor any of that cell's other cited symbols %v appear ANYWHERE in it — the citation has rotted "+
						"(re-point it at the real emit site, and name the symbol)",
						docLineNo, actionLiteral, path, actionLiteral, evidence)
				}
			}
			if len(cellCitations) == 0 {
				continue
			}
			rowHasCitation = true

			for _, c := range cellCitations {
				path, spec := c[0], c[1]
				_, bodies, gerr := load(path)
				if gerr != nil {
					t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s#%s, but %s could not be read: %v",
						docLineNo, actionLiteral, path, spec, path, gerr)
					continue
				}
				for _, sym := range strings.Split(spec, ",") {
					citationsChecked++
					body, ok := bodies[sym]
					if !ok {
						t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s#%s, but %s declares no such top-level "+
							"symbol — a method is cited as Type.Method; re-point the citation at the symbol that "+
							"emits the action now",
							docLineNo, actionLiteral, path, sym, path)
						continue
					}
					if strings.Contains(body, searchTerm) {
						continue
					}
					found := false
					for _, h := range holders {
						if strings.Contains(body, h) {
							found = true
							break
						}
					}
					for _, a := range evidence {
						if found {
							break
						}
						if strings.Contains(body, a) {
							found = true
							break
						}
						// A dotted anchor (`egress.Decision`) is written
						// package-qualified in the doc's prose, but a symbol
						// defined IN that package never repeats its own
						// package name at the definition site — so the bare
						// tail after the last "." is accepted too.
						//
						// A file-path anchor (`docs/ENV.md`, `internal/foo/bar.go`)
						// is not a dotted symbol -- its "tail after the last dot" is
						// just an extension ("md", "go"), which matches almost any
						// body and would let a drifted citation on a file-path
						// anchor evade the guard entirely. Skip the bare-tail
						// fallback for anything that looks like a path.
						if strings.Contains(a, "/") || strings.HasSuffix(a, ".go") || strings.HasSuffix(a, ".md") {
							continue
						}
						if idx := strings.LastIndex(a, "."); idx >= 0 && strings.Contains(body, a[idx+1:]) {
							found = true
						}
					}
					if found {
						continue
					}
					t.Errorf("docs/AUDIT-ACTIONS.md:%d: row %q cites %s#%s, but neither the action literal %q, "+
						"nor a constant holding it %v, nor any of that cell's other cited symbols %v appears "+
						"inside that symbol's own body — the citation names a symbol that does not emit this "+
						"action (re-point it at the one that does)",
						docLineNo, actionLiteral, path, sym, actionLiteral, holders, evidence)
				}
			}
		}
		if rowHasCitation {
			rowsChecked++
		}
	}
	if rowsChecked == 0 {
		t.Fatal("checked 0 rows with a file#symbol citation — the parser or the doc's table shape changed")
	}
	// Guard the guard: 251 citations resolve today — 217 through a symbol (210
	// of them on the action literal itself inside that symbol's body, 3 on a
	// constant holding it, 4 on a cell anchor) and 34 through a bare path. A
	// floor far below that, and
	// far above zero, catches a doc whose citation shape drifted out from under
	// the matcher (which is how a guard comes to pass on nothing) without making
	// the deliberate deletion of a row's citation a test failure.
	if citationsChecked < 150 {
		t.Fatalf("resolved only %d citations across %d rows — the citation shape changed; teach the guard the new one rather than letting it pass on nearly nothing",
			citationsChecked, rowsChecked)
	}
	t.Logf("checked %d citations across %d rows", citationsChecked, rowsChecked)
}

// TestAuditActionsDocCitesSymbolsNotLineNumbers holds docs/AUDIT-ACTIONS.md to
// the rule its neighbours already enforce on Go comments, threatmodel/*.md and
// docs/MEMBERS.md (TestCommentsCiteSymbolsNotLineNumbers,
// TestSecurityDocsCiteSymbolsNotLineNumbers,
// TestMembersDocCitesSymbolsNotLineNumbers): a claim about code is pinned to a
// SYMBOL, never to a line number.
//
// This table was the one place in the tree that mandated the shape the rest of
// the tree bans, and the contradiction had a cost the ban was written to avoid:
// every insertion above a cited line reddened the required build check. The
// citation form is now `file#symbol`, so the ban applies here too — and this is
// the test that stops the old form from creeping back one row at a time.
func TestAuditActionsDocCitesSymbolsNotLineNumbers(t *testing.T) {
	raw := readRepoFile(t, "docs/AUDIT-ACTIONS.md")
	// Not anchored on a backtick: the shape is wrong in prose too, and this is
	// the same claim TestCommentsCiteSymbolsNotLineNumbers's lineCitation makes
	// about Go comments, widened to cover a cited .md as well as a cited .go.
	lineAnchored := regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:go|md):[0-9]`)
	for i, line := range strings.Split(raw, "\n") {
		if m := lineAnchored.FindString(line); m != "" {
			t.Errorf("docs/AUDIT-ACTIONS.md:%d pins a citation to a line number (%s…) — cite the symbol "+
				"instead (path/file.go#Symbol, path/file.go#Type.Method, or path/doc.md#heading-slug); a line "+
				"number is stale the moment anything above it moves, and every one of them is a hostage the "+
				"required build check does not need", i+1, m)
		}
	}
}

// dataFieldsCell returns docs/AUDIT-ACTIONS.md's Data-fields cell (the third
// `|`-delimited column) for a table row starting with `action`, tokenized into
// its backtick-quoted field names.
func dataFieldsCell(t *testing.T, root, action string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docs", "AUDIT-ACTIONS.md"))
	if err != nil {
		t.Fatalf("read docs/AUDIT-ACTIONS.md: %v", err)
	}
	prefix := "| `" + action + "`"
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			t.Fatalf("docs/AUDIT-ACTIONS.md row for %q has fewer than the expected 5 columns", action)
		}
		var fields []string
		for _, m := range backtickSpan.FindAllStringSubmatch(cells[3], -1) {
			fields = append(fields, m[1])
		}
		return fields
	}
	t.Fatalf("docs/AUDIT-ACTIONS.md has no row for %q — re-anchor this guard", action)
	return nil
}

// TestAuditActionsDoc_UIOpenCloseDataFieldsMatchTheEmit pins the ui.open and
// ui.close Data-fields cells to what the emit passes: `duration_sec` is
// computed only at close (s.auditUI's caller at internal/api/uigateway.go
// passes it in the ui.close call's map literal, not ui.open's), and
// TestAuditActionsDocCitationsAreLive only checks citation proximity, never
// the Data-fields column's content.
//
// Scoped to these two rows rather than a general derived parity check: the
// data argument arrives as a map literal at 223 emit call sites across four
// different wrapper shapes ([]byte, map[string]any, a typed EventData struct,
// one built by a helper), several behind indirection the existing forward
// guard's fixed-point wrapper resolution does not (and does not need to)
// follow for the ACTION argument. A general version would have to re-derive
// that whole shape for the data argument too; this pins the two rows with the
// same "read it back out of the source" method instead of hand
// re-typing a second copy of what the code passes.
func TestAuditActionsDoc_UIOpenCloseDataFieldsMatchTheEmit(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "api", "uigateway.go"))
	if err != nil {
		t.Fatalf("read internal/api/uigateway.go: %v", err)
	}
	mapLit := regexp.MustCompile("`([a-z0-9_]+)`|\"([a-z0-9_]+)\":")
	for _, tc := range []struct {
		action string
		call   *regexp.Regexp
	}{
		{"ui.open", regexp.MustCompile(`"ui\.open"[^\n]*\n\s*map\[string\]any\{([^}]*)\}`)},
		{"ui.close", regexp.MustCompile(`"ui\.close"[^\n]*\n\s*map\[string\]any\{([^}]*)\}`)},
	} {
		m := tc.call.FindSubmatch(src)
		if m == nil {
			t.Fatalf("could not find the %s emit's map[string]any{...} literal in internal/api/uigateway.go — re-anchor this guard", tc.action)
		}
		emitted := map[string]bool{}
		for _, kv := range mapLit.FindAllStringSubmatch(string(m[1]), -1) {
			for _, k := range kv[1:] {
				if k != "" {
					emitted[k] = true
				}
			}
		}
		if len(emitted) == 0 {
			t.Fatalf("parsed 0 keys out of the %s emit's map literal — re-anchor this guard", tc.action)
		}
		documented := dataFieldsCell(t, root, tc.action)
		docSet := map[string]bool{}
		for _, f := range documented {
			docSet[f] = true
		}
		for k := range emitted {
			if !docSet[k] {
				t.Errorf("%s emits data field %q but docs/AUDIT-ACTIONS.md's row does not list it", tc.action, k)
			}
		}
		for _, f := range documented {
			if !emitted[f] {
				t.Errorf("docs/AUDIT-ACTIONS.md's %s row lists data field %q but the emit call does not pass it", tc.action, f)
			}
		}
	}
}

// TestAuditActionsForwardGuardCoversEveryEmitShape is the anchor under the
// forward guard: a guard that cannot see an emit helper, or one of the six
// packages docs/AUDIT-ACTIONS.md itself names, passes for the wrong reason.
//
// A guard's FIELD OF VIEW is part of its correctness, and a gap in it is
// invisible precisely because everything passes. So this asserts the view, not
// the verdict:
//
//   - the emitter set is derived and contains all three in-tree emit helpers,
//     at the right parameter index: `auditEvent`, (*Provider).audit and
//     auditFor. A hardcoded set that misses one lets a brand-new action added
//     through it leave the whole suite green.
//   - every action outside a narrower scan's reach is inside this one's. These
//     seven are documented, so the forward guard says nothing about them
//     either way — deleting their rows must fail.
func TestAuditActionsForwardGuardCoversEveryEmitShape(t *testing.T) {
	root := repoRoot(t)
	tr := parseAuditTree(t, root)

	// The action parameter's index in each helper's own signature.
	for name, want := range map[string]int{
		"auditEvent": 3, // (*Server).auditEvent(runID, actorType, actor, ACTION, ...)
		"audit":      3, // (*Provider).audit(ctx, runID, actor, ACTION, ...)
		"auditFor":   1, // auditFor(runID, ACTION, target, outcome, data)
	} {
		got, ok := tr.actionEmitters()[name]
		if !ok {
			t.Errorf("the derived emitter set does not contain %q — an audit helper the guard cannot see is an "+
				"undocumented action it cannot demand a row for", name)
			continue
		}
		if got != want {
			t.Errorf("emitter %q: action parameter index %d, want %d", name, got, want)
		}
	}

	seen := map[string]bool{}
	for _, e := range emittedAuditActions(t, root) {
		seen[e.action] = true
	}
	for _, action := range []string{
		// internal/identity/embedded, through the (*Provider).audit wrapper.
		"identity.mint", "identity.revoke",
		// internal/groundtruth, through auditFor and through const-valued
		// Action fields in composite literals.
		"kernel.process.exec", "kernel.network.connect", "kernel.file.write",
		"kernel.sensor.heartbeat", "kernel.sensor.blind",
		// The shapes that were already covered, so a refactor cannot trade one
		// blind spot for another.
		"drive.delete", "credential.mint", "approval.decide", "run.autostop",
	} {
		if !seen[action] {
			t.Errorf("audit action %q is emitted by non-test Go but the forward guard's scan does not see it — "+
				"deleting its docs/AUDIT-ACTIONS.md row would fail nothing", action)
		}
	}
}

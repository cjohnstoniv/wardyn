// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// citationRoots are the Go trees whose rationale comments must stay true. These
// comments are load-bearing here — they are the only record of WHY a clamp, a
// floor or a fail-closed branch exists — so a reader has to be able to trust
// where they point.
var citationRoots = []string{"cmd", "internal", "pkg"}

// lineCitation matches a cross-file reference that pins a claim to a line number
// in another Go file. Banning the construct outright is far cheaper than
// validating it: checking that each citation still points at the code it claims
// would mean re-resolving every one on every run, and a citation that resolves
// to the WRONG code still passes such a check.
//
// Every instance this guard was written for had already rotted. The 2026-07-17
// god-function decomposition (6e789f5) moved the run-create path out of
// internal/api/runs.go into runs_create.go; six comments in
// internal/api/compose_setup.go went on citing line numbers in files that had
// since shrunk below them, so each one sent the reader to unrelated code. A
// symbol name survives that refactor; a line number never does. Cite the symbol
// (and the file, if it is not obvious) instead.
var lineCitation = regexp.MustCompile(`[a-zA-Z0-9_]+\.go:[0-9]+`)

// TestCommentsCiteSymbolsNotLineNumbers fails on any Go comment under
// citationRoots that references another file by line number.
//
// It reads comments from go/parser's AST rather than the raw file bytes, which
// matters twice: a byte scanner would trip on this guard's own pattern literal
// (and on any test fixture holding a compiler diagnostic), and it would also
// flag matches inside string literals — which are data, not claims about the
// code, and are none of this guard's business.
// TestSecurityDocsCiteSymbolsNotLineNumbers extends the same rule to the threat
// model, which is where the rot is most expensive: a security document that
// sends a reviewer to unrelated code reads as sloppy about exactly the thing it
// is asserting rigour about.
//
// The doc's own §8 already states the rule — "Citations below name SYMBOLS, not
// line ranges — an earlier pass pinned line numbers and six of nine had rotted
// onto unrelated code (one past EOF)". It stated it and then kept nine of them,
// two of which had rotted again by 0.7. A rule with no gate is a preference.
func TestSecurityDocsCiteSymbolsNotLineNumbers(t *testing.T) {
	root := repoRoot(t)
	// Markdown, so there is no AST to read comments from — but also no code, so
	// every match IS a claim about the code. A raw scan is correct here.
	docs, err := filepath.Glob(filepath.Join(root, "threatmodel", "*.md"))
	if err != nil {
		t.Fatalf("glob threatmodel: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no threatmodel/*.md found — this guard would pass vacuously")
	}
	for _, doc := range docs {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		rel, _ := filepath.Rel(root, doc)
		for i, line := range strings.Split(string(b), "\n") {
			if m := lineCitation.FindString(line); m != "" {
				t.Errorf("%s:%d cites %q by line number — cite the SYMBOL instead; a line number does not survive a refactor and sends a security reviewer to unrelated code", rel, i+1, m)
			}
		}
	}
}

// TestMembersDocCitesSymbolsNotLineNumbers extends the same rule to
// docs/MEMBERS.md, the first member-facing doc: a line citation there rots
// the same way it does everywhere else, and a member reader has even less
// use for one than a security reviewer does. Cite endpoints, env vars and
// doc anchors instead.
func TestMembersDocCitesSymbolsNotLineNumbers(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "MEMBERS.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read docs/MEMBERS.md: %v", err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		if m := lineCitation.FindString(line); m != "" {
			t.Errorf("docs/MEMBERS.md:%d cites %q by line number — cite the SYMBOL instead", i+1, m)
		}
	}
}

func TestCommentsCiteSymbolsNotLineNumbers(t *testing.T) {
	root := repoRoot(t)

	scanned := 0
	for _, sub := range citationRoots {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			fset := token.NewFileSet()
			// Parse unconditionally: build tags are a go/build concern, and the
			// -tags docker files (hardening.go, runner_docker.go) carry rationale
			// comments too, so they must be covered.
			f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			scanned++
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					for _, m := range lineCitation.FindAllString(c.Text, -1) {
						t.Errorf("%s:%d: comment pins a claim to a line number (%q) — cite the symbol instead "+
							"(e.g. \"see unionWorkspaceEgress in workspace_run.go\"); a line number is stale the "+
							"moment anything above it moves", rel, fset.Position(c.Pos()).Line, m)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	// Guard the guard: a wrong root would silently scan nothing and pass.
	if scanned == 0 {
		t.Fatalf("scanned 0 files across %v — the root list is wrong", citationRoots)
	}
	t.Logf("scanned %d files across %v", scanned, citationRoots)
}

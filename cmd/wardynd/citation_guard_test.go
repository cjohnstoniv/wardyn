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

// goPathSpan is a backticked path or filename ending .go inside a markdown
// document.
var goPathSpan = regexp.MustCompile("`([A-Za-z0-9_./-]+\\.go)`")

// symbolThenPath and pathThenSymbol are the two shapes the threat model uses to
// pin a claim to code now that line numbers are banned: "`Sym` in `pkg/f.go`",
// "(`Sym`, `pkg/f.go`)", and the reverse. Deliberately ADJACENT-only — a
// sentence naming four symbols and two files gives no evidence about which
// belongs to which, and a cross-product would invent claims the document never
// made and then fail on them.
var (
	symbolThenPath = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_.]*)`(?:\\s*(?:,|—|-|in|is in|lives in|,? see)\\s*)`([A-Za-z0-9_./-]+\\.go)`")
	pathThenSymbol = regexp.MustCompile("`([A-Za-z0-9_./-]+\\.go)`\\s*(?:,|—|-|:)?\\s*`([A-Za-z_][A-Za-z0-9_.]*)`")
)

// TestSecurityDocsCitationsResolve is the other half of the citation rule. Its
// neighbour above bans the one citation SHAPE that always rots (a line number);
// nothing checked that a surviving citation points at anything real, so a
// symbol cited to the WRONG FILE — or a file that no longer exists — read as a
// perfectly well-formed citation. In a document whose entire value is that a
// reviewer can check it, "well-formed" is not the property that matters.
//
// Two rules, both narrow enough to make every failure a real one:
//
//  1. every backticked .go path must RESOLVE — as a repo-relative path, or, for
//     a bare filename, as some file under cmd/ internal/ pkg/;
//  2. for an ADJACENT (symbol, path) pair, the symbol must appear in that file.
//
// The symbol is matched on its bare tail after the last "." because a symbol
// written package-qualified in prose (`egress.Decision`) never repeats its own
// package name at the definition site — the same reading the audit-actions
// guard's anchors use.
func TestSecurityDocsCitationsResolve(t *testing.T) {
	root := repoRoot(t)
	docs, err := filepath.Glob(filepath.Join(root, "threatmodel", "*.md"))
	if err != nil {
		t.Fatalf("glob threatmodel: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no threatmodel/*.md found — this guard would pass vacuously")
	}

	// Bare filenames (`sshkeys.go`) resolve against the Go trees, not the repo
	// root. Indexed once.
	byBase := map[string]string{}
	for _, sub := range citationRoots {
		_ = filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") {
				byBase[filepath.Base(path)] = path
			}
			return nil
		})
	}
	if len(byBase) == 0 {
		t.Fatalf("indexed 0 .go files across %v — the root list is wrong", citationRoots)
	}
	body := map[string]string{}
	resolve := func(cited string) (string, bool) {
		if src, ok := body[cited]; ok {
			return src, src != ""
		}
		abs := filepath.Join(root, cited)
		if !strings.Contains(cited, "/") {
			p, ok := byBase[cited]
			if !ok {
				body[cited] = ""
				return "", false
			}
			abs = p
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			body[cited] = ""
			return "", false
		}
		body[cited] = string(b)
		return string(b), true
	}

	paths, pairs := 0, 0
	for _, doc := range docs {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		rel, _ := filepath.Rel(root, doc)
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range goPathSpan.FindAllStringSubmatch(line, -1) {
				paths++
				if _, ok := resolve(m[1]); !ok {
					t.Errorf("%s:%d cites %s, which does not exist — a security document that sends a reviewer to a file that is not there is asserting rigour it does not have", rel, i+1, m[1])
				}
			}
			for _, re := range []*regexp.Regexp{symbolThenPath, pathThenSymbol} {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					sym, cited := m[1], m[2]
					if strings.HasSuffix(sym, ".go") {
						sym, cited = m[2], m[1]
					}
					src, ok := resolve(cited)
					if !ok {
						continue // already reported by the path rule above
					}
					pairs++
					tail := sym
					if idx := strings.LastIndex(tail, "."); idx >= 0 {
						tail = tail[idx+1:]
					}
					if !strings.Contains(src, tail) {
						t.Errorf("%s:%d cites %s in %s, but %q appears nowhere in that file — the citation names the wrong file (cite the symbol AND the file it is actually in)", rel, i+1, sym, cited, tail)
					}
				}
			}
		}
	}
	// Guard the guard: a doc whose citations all changed shape would pass
	// vacuously, which is the failure this whole file exists to refuse.
	if paths == 0 || pairs == 0 {
		t.Fatalf("resolved %d .go paths and %d (symbol, file) pairs — one of the two citation shapes vanished from the threat model; teach the guard the new one rather than letting it pass on nothing", paths, pairs)
	}
	t.Logf("resolved %d .go path citations and %d (symbol, file) pairs across %d documents", paths, pairs, len(docs))
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

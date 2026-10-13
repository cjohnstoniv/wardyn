// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// retiredEverywhere are the phrases no command, console string or user doc may use any more: a runner is
// self-hosted or remote-hosted, a pool is an organization pool or a user pool, and a pool's limits come from
// its runners. docs/design/ (history), docs/adoption/ (a reply quoting a third party's brief) and CHANGELOG.md
// (which names the old words to explain the upgrade) are outside the guard.
var retiredEverywhere = regexp.MustCompile(`(?i)executor|remote[ -]provided|self-hosted pool|no pool limit`)

// codeSpan matches an inline code span; a doc may name a Go symbol or a retired wire field inside one.
var codeSpan = regexp.MustCompile("`[^`]*`")

// TestRetiredPoolWordsAbsentFromCommandsConsoleAndDocs scans what a person reads across the whole tree: the
// string literals of every cmd/ Go file, the console's source with its tests, fixtures and browser specs (they
// assert and render the same strings; comment lines excepted, which are not copy), and every Markdown file
// under docs/ outside the exempt directories, with code spans removed.
func TestRetiredPoolWordsAbsentFromCommandsConsoleAndDocs(t *testing.T) {
	root := filepath.Join("..", "..")
	walk := func(dir string, keep func(path string) bool, visit func(path string)) {
		t.Helper()
		seen := 0
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || (dir == "docs" && (path == filepath.Join(root, "docs", "design") || path == filepath.Join(root, "docs", "adoption"))) {
					return filepath.SkipDir
				}
				return nil
			}
			if keep(path) {
				seen++
				visit(path)
			}
			return nil
		})
		if err != nil || seen == 0 {
			t.Fatalf("walk %s: %d files, %v", dir, seen, err)
		}
	}
	hit := func(path string, line int, text string) {
		if m := retiredEverywhere.FindString(text); m != "" {
			t.Errorf("%s:%d says %q: %q", filepath.ToSlash(path), line, m, strings.TrimSpace(text))
		}
	}

	walk("cmd", func(p string) bool { return strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") }, func(path string) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				hit(path, fset.Position(lit.Pos()).Line, lit.Value)
			}
			return true
		})
	})

	console := func(p string) bool {
		return strings.HasSuffix(p, ".ts") || strings.HasSuffix(p, ".tsx") || strings.HasSuffix(p, ".json")
	}
	for _, dir := range []string{filepath.Join("ui", "src"), filepath.Join("ui", "e2e")} {
		walk(dir, console, func(path string) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(raw), "\n") {
				if c := strings.TrimSpace(line); strings.HasPrefix(c, "//") || strings.HasPrefix(c, "/*") || strings.HasPrefix(c, "*") {
					continue
				}
				hit(path, i+1, line)
			}
		})
	}

	walk("docs", func(p string) bool { return strings.HasSuffix(p, ".md") }, func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			hit(path, i+1, codeSpan.ReplaceAllString(line, ""))
		}
	})
}

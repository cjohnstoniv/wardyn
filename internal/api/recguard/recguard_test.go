// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recguard refuses new raw reads of the audit recorder's events slice
// in internal/api tests. recRecorder appends from the detached create-run launch
// after the 201 while a test is still reading, so a test reads through the
// locked snapshot() or races under -race.
//
// It is its own package so it runs in seconds without compiling internal/api.
// A selector counts as a raw read when it is `.events` on a value named audit,
// rec or siteAudit (or a field of that name) and is not a method call; the
// legacy sites are frozen per file in testdata/allow.txt and may only shrink.
package recguard

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/allow.txt from the current tree")

func isRecorder(e ast.Expr) bool {
	name := ""
	switch x := e.(type) {
	case *ast.Ident:
		name = x.Name
	case *ast.SelectorExpr:
		name = x.Sel.Name
	}
	return name == "audit" || name == "rec" || name == "siteAudit"
}

// rawReads counts the raw `.events` accesses per *_test.go file in dir.
func rawReads(t *testing.T, dir string) map[string]int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		calls := map[ast.Expr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				calls[c.Fun] = true
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "events" && !calls[sel] && isRecorder(sel.X) {
				counts[filepath.Base(f)]++
			}
			return true
		})
	}
	return counts
}

func readAllow(t *testing.T, path string) map[string]int {
	t.Helper()
	allow := map[string]int{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return allow
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name, n, ok := strings.Cut(strings.TrimSpace(sc.Text()), " "); ok {
			v, err := strconv.Atoi(n)
			if err != nil {
				t.Fatalf("%s: %q: %v", path, sc.Text(), err)
			}
			allow[name] = v
		}
	}
	return allow
}

// TestNoNewRawRecorderReads fails when a test file reads the recorder's raw
// events more often than testdata/allow.txt grants it. RECGUARD_DIR points the
// scan at another directory (the planted-violation twin).
func TestNoNewRawRecorderReads(t *testing.T) {
	dir, allowPath := "..", "testdata/allow.txt"
	if d := os.Getenv("RECGUARD_DIR"); d != "" {
		dir, allowPath = d, filepath.Join(d, "allow.txt")
	}
	got := rawReads(t, dir)
	if *update {
		var lines []string
		for name, n := range got {
			lines = append(lines, fmt.Sprintf("%s %d", name, n))
		}
		sort.Strings(lines)
		if err := os.WriteFile(allowPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	allow := readAllow(t, allowPath)
	for name, n := range got {
		switch {
		case n > allow[name]:
			t.Errorf("%s: %d raw read(s) of the recorder's events, %d allowed; read snapshot() instead", name, n, allow[name])
		case n < allow[name]:
			t.Errorf("%s: %d raw read(s) left, allow.txt grants %d; lower it", name, n, allow[name])
		}
	}
	for name, n := range allow {
		if _, ok := got[name]; !ok {
			t.Errorf("%s: no raw reads left, allow.txt grants %d; delete the line", name, n)
		}
	}
}

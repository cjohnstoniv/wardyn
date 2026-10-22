// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recguard refuses new raw reads of the audit recorder's events slice
// in internal/api tests. recRecorder appends from the detached create-run launch
// after the 201 while a test is still reading, so a test reads through the
// locked snapshot() or races under -race.
//
// It is its own package so it runs in seconds without compiling internal/api.
// A selector counts as a raw read when it is `.events` on a value named audit,
// rec or siteAudit (or a field of that name), or on an assertion to
// *recRecorder (srv.cfg.Audit.(*recRecorder).events), and is not a method call.
// This is a name heuristic, not a type check: a recorder held under another
// variable name (for example pvRec) is not seen.
//
// The rule: a file fails only when its count grows above its line in
// testdata/allow.txt, or when a file with no line has any raw read. A count that
// shrinks passes; stale lines are pruned in a cleanup lane. -update may only
// lower a count or delete a line: it refuses, naming the file and leaving
// allow.txt as it was, to raise a count or add a file.
package recguard

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
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
	case *ast.TypeAssertExpr:
		if star, ok := x.Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok {
				return id.Name == "recRecorder"
			}
		}
		return false
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
// scan at another directory (testdata/planted and testdata/planted_assert are the
// planted-violation twins).
func TestNoNewRawRecorderReads(t *testing.T) {
	dir, allowPath := "..", "testdata/allow.txt"
	if d := os.Getenv("RECGUARD_DIR"); d != "" {
		dir, allowPath = d, filepath.Join(d, "allow.txt")
	}
	got := rawReads(t, dir)
	if *update {
		if err := updateAllow(got, readAllow(t, allowPath), allowPath); err != nil {
			t.Fatal(err)
		}
		return
	}
	allow := readAllow(t, allowPath)
	for name, n := range got {
		if n > allow[name] {
			t.Errorf("%s: %d raw read(s) of the recorder's events, %d allowed; read snapshot() instead", name, n, allow[name])
		}
	}
}

// updateAllow rewrites allowPath to the scanned counts, but only downward: a
// count that rose, or a file with no line, is an error and nothing is written.
func updateAllow(got, allow map[string]int, allowPath string) error {
	var raised, lines []string
	for name, n := range got {
		if n > allow[name] {
			raised = append(raised, fmt.Sprintf("%s: %d raw read(s), %d allowed", name, n, allow[name]))
		}
		lines = append(lines, fmt.Sprintf("%s %d", name, n))
	}
	if len(raised) > 0 {
		sort.Strings(raised)
		return fmt.Errorf("-update refuses to raise a count or add a file; read snapshot() instead:\n%s", strings.Join(raised, "\n"))
	}
	sort.Strings(lines)
	return os.WriteFile(allowPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// runUpdate runs this test binary with -update against dir and returns its
// combined output and exit code.
func runUpdate(t *testing.T, dir string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNoNewRawRecorderReads$", "-update")
	cmd.Env = append(os.Environ(), "RECGUARD_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestUpdateRefusesToRaise: the fixture reads more than allow.txt grants
// (raise_test.go) and holds a file with no line (added_test.go). -update exits 1
// naming both and leaves allow.txt as it was.
func TestUpdateRefusesToRaise(t *testing.T) {
	dir := copyDir(t, "testdata/update_raise")
	before := readFile(t, filepath.Join(dir, "allow.txt"))
	out, code := runUpdate(t, dir)
	if code != 1 || !strings.Contains(out, "raise_test.go") || !strings.Contains(out, "added_test.go") {
		t.Fatalf("-update exit %d, want 1 naming raise_test.go and added_test.go:\n%s", code, out)
	}
	if after := readFile(t, filepath.Join(dir, "allow.txt")); after != before {
		t.Fatalf("allow.txt changed on a refused -update:\n%s", after)
	}
}

// TestUpdateLowers: the fixture reads fewer than allow.txt grants and has a
// line for a file with no reads. -update exits 0, lowers the one and deletes the other.
func TestUpdateLowers(t *testing.T) {
	dir := copyDir(t, "testdata/update_lower")
	out, code := runUpdate(t, dir)
	if code != 0 {
		t.Fatalf("-update exit %d, want 0:\n%s", code, out)
	}
	if got := readFile(t, filepath.Join(dir, "allow.txt")); got != "lower_test.go 1\n" {
		t.Fatalf("allow.txt = %q, want %q", got, "lower_test.go 1\n")
	}
}

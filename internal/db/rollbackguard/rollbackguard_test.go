// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package rollbackguard refuses, outside tests, a transaction rollback with no bound: X.Rollback(A) whose one
// argument calls context.WithoutCancel, context.Background or context.TODO. On a connection that stopped answering,
// such a rollback waits past every bound its caller set. db.Rollback(ctx, tx) is the bounded form.
//
// It is its own package so it runs in seconds without compiling internal/store or internal/api. The check reads
// syntax, not types: a detached context put in a variable first and then passed is not seen. db.Rollback takes two
// arguments and is never matched.
package rollbackguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var detaching = map[string]bool{"context.WithoutCancel": true, "context.Background": true, "context.TODO": true}

// unbounded answers the line of each rollback in path whose one argument calls a detaching function.
func unbounded(t *testing.T, path string) []int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Rollback" {
			return true
		}
		ast.Inspect(call.Args[0], func(m ast.Node) bool {
			if c, ok := m.(*ast.CallExpr); ok && detaching[types.ExprString(c.Fun)] {
				lines = append(lines, fset.Position(call.Pos()).Line)
				return false
			}
			return true
		})
		return true
	})
	return lines
}

// TestNoUnboundedRollback fails on any rollback with no bound in the repository's non-test Go.
func TestNoUnboundedRollback(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not at the repository root: %v", err)
	}
	scanned := 0
	for _, dir := range []string{"cmd", "deploy", "examples", "internal", "pkg", "test"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			rel, _ := filepath.Rel(root, path)
			for _, line := range unbounded(t, path) {
				t.Errorf("%s:%d: a rollback on a detached context has no bound when the connection stops answering; "+
					"use db.Rollback(ctx, tx), which ends by ctx's deadline or db.RollbackBound", filepath.ToSlash(rel), line)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if scanned < 1000 {
		t.Fatalf("scanned %d Go files; the walk lost the tree", scanned)
	}
}

// The guard's own pin: testdata/planted_test.go marks each line it must refuse "// refused" and each it must pass
// "// allowed".
func TestNoUnboundedRollback_RefusesEachPlantedForm(t *testing.T) {
	path := filepath.Join("testdata", "planted_test.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []int
	allowed := 0
	for i, l := range strings.Split(string(src), "\n") {
		switch {
		case strings.Contains(l, "// refused"):
			want = append(want, i+1)
		case strings.Contains(l, "// allowed"):
			allowed++
		}
	}
	if len(want) == 0 || allowed == 0 {
		t.Fatal("the planted file marks no refused or no allowed line")
	}
	if got := unbounded(t, path); !slices.Equal(got, want) {
		t.Errorf("refused lines %v, want %v", got, want)
	}
}

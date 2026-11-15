// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package rollbackguard refuses, outside tests, a transaction rollback with no bound: X.Rollback(A) whose one
// argument is a detached context. A context is detached when it comes from context.WithoutCancel,
// context.Background or context.TODO with no context.WithTimeout or context.WithDeadline around it. On a
// connection that stopped answering, such a rollback waits past every bound its caller set. db.Rollback(ctx, tx) is
// the bounded form; it takes two arguments, so it is never matched.
//
// It is its own package so it runs in seconds without compiling internal/store or internal/api. The check reads
// syntax, not types, one top-level declaration at a time. It follows the file's own name for the context package
// (an alias or a dot import), parentheses around the call, a method value such as undo := tx.Rollback, and a local
// name, ctx included, given a detached context anywhere in the declaration, whatever the order. A call to any other
// function is taken to pass a detached argument through. It does not see a context or a method value that reaches
// the declaration from outside it: a parameter, a struct field or a package-level variable.
package rollbackguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// detaching drops a context's cancellation and deadline; bounding gives a context a deadline of its own.
var (
	detaching = map[string]bool{"WithoutCancel": true, "Background": true, "TODO": true}
	bounding  = map[string]bool{"WithTimeout": true, "WithDeadline": true, "WithTimeoutCause": true, "WithDeadlineCause": true}
)

// file is one parsed file: its names for the context package and, for the declaration being read, the names that
// hold a detached context and the names that hold a Rollback method value.
type file struct {
	pkg, held, undo map[string]bool
}

// contextFunc answers the name of the context package function that fun names, or "" when it names none.
func (f *file) contextFunc(fun ast.Expr) string {
	switch fn := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok && f.pkg[x.Name] {
			return fn.Sel.Name
		}
	case *ast.Ident:
		if f.pkg["."] {
			return fn.Name
		}
	}
	return ""
}

// detached reports whether e yields a detached context: a detaching call or a held name, with no bounding call
// around it. A function literal is not looked into: its body runs elsewhere.
func (f *file) detached(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectorExpr:
			found = found || f.detached(n.X) // a field or method name is not a local name
			return false
		case *ast.CallExpr:
			name := f.contextFunc(n.Fun)
			if bounding[name] {
				return false
			}
			found = found || detaching[name]
		case *ast.Ident:
			found = found || f.held[n.Name]
		}
		return !found
	})
	return found
}

// mark records what each name on the left is given on the right. A multi-value call gives its first name the
// context, as ctx, cancel := context.WithCancel(parent) does.
func (f *file) mark(lhs, rhs []ast.Expr) {
	for i, l := range lhs {
		id, ok := l.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		var r ast.Expr
		switch {
		case len(rhs) == len(lhs):
			r = rhs[i]
		case len(rhs) == 1 && i == 0:
			r = rhs[0]
		default:
			continue
		}
		if sel, ok := ast.Unparen(r).(*ast.SelectorExpr); ok && sel.Sel.Name == "Rollback" {
			f.undo[id.Name] = true
		}
		if f.detached(r) {
			f.held[id.Name] = true
		}
	}
}

// names reads decl's assignments in source order, so a name given a held name is held too.
func (f *file) names(decl ast.Decl) {
	f.held, f.undo = map[string]bool{}, map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			f.mark(n.Lhs, n.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(n.Names))
			for i, id := range n.Names {
				lhs[i] = id
			}
			f.mark(lhs, n.Values)
		}
		return true
	})
}

// rollback reports whether fun is a Rollback method: tx.Rollback, (tx.Rollback), or a name that holds one.
func (f *file) rollback(fun ast.Expr) bool {
	switch fn := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name == "Rollback"
	case *ast.Ident:
		return f.undo[fn.Name]
	}
	return false
}

// unbounded answers the line of each rollback in path whose one argument is a detached context.
func unbounded(t *testing.T, path string) []int {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{pkg: map[string]bool{}}
	for _, imp := range af.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != "context" {
			continue
		}
		name := "context"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		f.pkg[name] = true
	}
	var lines []int
	for _, decl := range af.Decls {
		f.names(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 1 && f.rollback(call.Fun) && f.detached(call.Args[0]) {
				lines = append(lines, fset.Position(call.Pos()).Line)
			}
			return true
		})
	}
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

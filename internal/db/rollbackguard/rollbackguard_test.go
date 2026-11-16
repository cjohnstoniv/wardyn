// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package rollbackguard refuses, outside tests, a transaction rollback with no bound: X.Rollback(A) whose one
// argument is a detached context, or a method expression T.Rollback(tx, A) whose second argument is. A context is
// detached when it comes from context.WithoutCancel, context.Background, context.TODO, or a function or method of
// the same package, known by its name, that returns one, with no context.WithTimeout or context.WithDeadline around
// it. On a connection that stopped answering, such a rollback waits past every bound its caller set.
// db.Rollback(ctx, tx) is the bounded form; it is known by the file's own name for internal/db, so it is never
// matched. It takes two arguments, so a one-argument X.Rollback(A) through that name is a local that hides the
// package, and is matched; so is a one-argument call of a name given such a Rollback.
//
// It is its own package so it runs in seconds without compiling internal/store or internal/api. The check reads
// syntax, not types: it first finds the package's functions that return a context and answer a detached one, then
// reads one top-level declaration at a time. It follows the file's own name for the context package (an alias or a
// dot import), parentheses around the call, a method value such as undo := tx.Rollback and every name it is passed
// on to, a function literal called where it is written, and a local name, ctx and a range variable included, given
// a detached context anywhere in the declaration, whatever the order. A call to any other function is taken to pass
// a detached argument through. A name given a detached context anywhere stays held where a bound is put on it under
// the same name: the bounded context needs a name of its own. It does not see a context or a method value that
// reaches the declaration from outside it (a parameter, a struct field or a package-level variable), a method value
// kept in a slice, map or struct, a helper of another package, or a context package other than the standard one.
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

// dbPath is the import path of db.Rollback, the bounded form.
const dbPath = "github.com/cjohnstoniv/wardyn/internal/db"

// detaching drops a context's cancellation and deadline; bounding gives a context a deadline of its own.
var (
	detaching = map[string]bool{"WithoutCancel": true, "Background": true, "TODO": true}
	bounding  = map[string]bool{"WithTimeout": true, "WithDeadline": true, "WithTimeoutCause": true, "WithDeadlineCause": true}
)

// file is one parsed file: its names for the context package and for internal/db, the names of its package's
// functions that return a detached context (shared by the package's files), and, for the declaration being read, the
// names that hold a detached context and the names that hold a Rollback method value.
type file struct {
	pkg, db, detachers, held, undo, viaDB map[string]bool
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

// dbRollback reports whether fun is Rollback spelled through the file's name for internal/db, or a name given one.
func (f *file) dbRollback(fun ast.Expr) bool {
	switch fn := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		x, ok := fn.X.(*ast.Ident)
		return ok && fn.Sel.Name == "Rollback" && f.db[x.Name]
	case *ast.Ident:
		return f.viaDB[fn.Name]
	}
	return false
}

// hidesDB reports a one-argument call of a Rollback spelled through the file's name for internal/db, or of a name
// given one: db.Rollback takes two, so it is the method of a local name that hides the package.
func (f *file) hidesDB(call *ast.CallExpr) bool {
	return len(call.Args) == 1 && f.dbRollback(call.Fun)
}

// calleeName answers the name a call's function is spelled with: f for f(...) and x.f(...), "" for anything else.
func calleeName(fun ast.Expr) string {
	switch fn := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}

// detached reports whether e yields a detached context: a detaching call, a call to one of the package's detaching
// functions or to a function literal that returns a detached context, or a held name, with no bounding call around
// it. Any other function literal is not looked into: its body runs elsewhere.
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
			lit, isLit := ast.Unparen(n.Fun).(*ast.FuncLit)
			found = found || detaching[name] || f.detachers[calleeName(n.Fun)] || isLit && f.returns(lit.Body, lit.Type.Results)
		case *ast.Ident:
			found = found || f.held[n.Name]
		}
		return !found
	})
	return found
}

// returns reports whether a return statement of body, outside the function literals in it, answers a detached
// context; a bare return answers the named results.
func (f *file) returns(body *ast.BlockStmt, results *ast.FieldList) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			for _, r := range n.Results {
				found = found || f.detached(r)
			}
			if len(n.Results) == 0 && results != nil {
				for _, field := range results.List {
					for _, id := range field.Names {
						found = found || f.held[id.Name]
					}
				}
			}
		}
		return !found
	})
	return found
}

// returnsDetached reports whether fd is a function that returns a context and answers a detached one.
func (f *file) returnsDetached(fd *ast.FuncDecl) bool {
	if fd.Body == nil || fd.Type.Results == nil ||
		!slices.ContainsFunc(fd.Type.Results.List, func(r *ast.Field) bool { return f.contextFunc(r.Type) == "Context" }) {
		return false
	}
	f.names(fd)
	return f.returns(fd.Body, fd.Type.Results)
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
		if f.rollback(r) {
			f.undo[id.Name] = true
		}
		if f.dbRollback(r) {
			f.viaDB[id.Name] = true
		}
		if f.detached(r) {
			f.held[id.Name] = true
		}
	}
}

// names reads decl's assignments, range variables included, again until no name is added, so a name given a held
// name is held too whatever the order the two are written in.
func (f *file) names(decl ast.Decl) {
	f.held, f.undo, f.viaDB = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for seen := -1; seen != len(f.held)+len(f.undo)+len(f.viaDB); {
		seen = len(f.held) + len(f.undo) + len(f.viaDB)
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
			case *ast.RangeStmt:
				f.mark([]ast.Expr{n.Key, n.Value}, []ast.Expr{n.X, n.X})
			}
			return true
		})
	}
}

// rollback reports whether fun is a Rollback method: tx.Rollback, (tx.Rollback), the method expression T.Rollback,
// or a name that holds one. db.Rollback, the bounded form, is none.
func (f *file) rollback(fun ast.Expr) bool {
	switch fn := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		x, ok := fn.X.(*ast.Ident)
		return fn.Sel.Name == "Rollback" && !(ok && f.db[x.Name])
	case *ast.Ident:
		return f.undo[fn.Name]
	}
	return false
}

// detachedArg reports whether a rollback's context is detached: the one argument of X.Rollback(A), or the second
// of a method expression T.Rollback(tx, A).
func (f *file) detachedArg(args []ast.Expr) bool {
	switch len(args) {
	case 1:
		return f.detached(args[0])
	case 2:
		return f.detached(args[1])
	}
	return false
}

// unbounded answers, for each of paths (the files of one package), the line of each rollback in it whose context
// is detached.
func unbounded(t *testing.T, paths []string) map[string][]int {
	t.Helper()
	fset := token.NewFileSet()
	detachers := map[string]bool{}
	asts := make([]*ast.File, len(paths))
	files := make([]*file, len(paths))
	for i, path := range paths {
		af, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		f := &file{pkg: map[string]bool{}, db: map[string]bool{}, detachers: detachers}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p != "context" && p != dbPath {
				continue
			}
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if p == dbPath {
				f.db[name] = true
			} else {
				f.pkg[name] = true
			}
		}
		asts[i], files[i] = af, f
	}
	// Read the package's functions again until no new one is found, so a function that returns another's
	// detached context is found too.
	for seen := -1; seen != len(detachers); {
		seen = len(detachers)
		for i, af := range asts {
			for _, decl := range af.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && files[i].returnsDetached(fd) {
					detachers[fd.Name.Name] = true
				}
			}
		}
	}
	lines := map[string][]int{}
	for i, af := range asts {
		f := files[i]
		for _, decl := range af.Decls {
			f.names(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && (f.rollback(call.Fun) || f.hidesDB(call)) && f.detachedArg(call.Args) {
					lines[paths[i]] = append(lines[paths[i]], fset.Position(call.Pos()).Line)
				}
				return true
			})
		}
	}
	return lines
}

// TestNoUnboundedRollback fails on any rollback with no bound in the repository's non-test Go.
func TestNoUnboundedRollback(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("not at the repository root: %v", err)
	}
	pkgs := map[string][]string{} // a directory's non-test Go files
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
			pkgs[filepath.Dir(path)] = append(pkgs[filepath.Dir(path)], path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	scanned := 0
	for _, paths := range pkgs {
		scanned += len(paths)
		for path, lines := range unbounded(t, paths) {
			rel, _ := filepath.Rel(root, path)
			for _, line := range lines {
				t.Errorf("%s:%d: a rollback on a detached context has no bound when the connection stops answering; "+
					"use db.Rollback(ctx, tx), which ends by ctx's deadline or db.RollbackBound", filepath.ToSlash(rel), line)
			}
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
	if got := unbounded(t, []string{path})[path]; !slices.Equal(got, want) {
		t.Errorf("refused lines %v, want %v", got, want)
	}
}

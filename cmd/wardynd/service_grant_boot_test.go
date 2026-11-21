// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// The service grant's refresher starts on every replica whether or not the server has a runner
// (a headless API serves grants too), and its clean-up runs through leaderGo. Neither call may sit
// under a condition, and the grant's transport is handed to api.New. The behaviour of each is
// internal/api's; what only this package can break is the wiring.
func TestServeStartsServiceGrantRefresherWithoutRunner(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "wardynd", "boot_serve.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var workers *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "startBackgroundWorkers" {
			workers = fd
		}
	}
	if workers == nil {
		t.Fatal("cmd/wardynd/boot_serve.go: startBackgroundWorkers not found, the guard's target moved")
	}
	found := map[string]bool{}
	callPos := map[string]token.Pos{}
	var stack []ast.Node
	ast.Inspect(workers.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "StartServiceGrantRefresher" && sel.Sel.Name != "RunServiceGrantCleanup") {
			return true
		}
		found[sel.Sel.Name] = true
		callPos[sel.Sel.Name] = sel.Pos()
		for _, anc := range stack {
			switch anc.(type) {
			case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
				t.Errorf("boot_serve.go:%d: %s is under a condition; it must start on every replica, with or without a runner",
					fset.Position(sel.Pos()).Line, sel.Sel.Name)
			}
		}
		// The clean-up runs under leader election: the call expression around the selector is an
		// argument of leaderGo, not a bare go statement or a direct call on every replica.
		if sel.Sel.Name == "RunServiceGrantCleanup" && !insideLeaderGo(stack) {
			t.Errorf("boot_serve.go:%d: RunServiceGrantCleanup is not started through leaderGo", fset.Position(sel.Pos()).Line)
		}
		return true
	})
	for _, name := range []string{"StartServiceGrantRefresher", "RunServiceGrantCleanup"} {
		if !found[name] {
			t.Errorf("startBackgroundWorkers no longer starts srv.%s", name)
		}
	}
	// An early return above either call would skip it. A return inside a function literal
	// leaves only that literal, so the walk does not enter one.
	ast.Inspect(workers.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if r, ok := n.(*ast.ReturnStmt); ok {
			for name, pos := range callPos {
				if r.Pos() < pos {
					t.Errorf("boot_serve.go:%d: a return comes before the %s call at line %d", fset.Position(r.Pos()).Line, name, fset.Position(pos).Line)
				}
			}
		}
		return true
	})

	_, run, runFset := findRunFunc(t, root)
	wired := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "ServiceGrantTransport" {
				wired = true
			}
		}
		return true
	})
	if !wired {
		t.Errorf("cmd/wardynd/main.go (%s): run() does not set api.Config.ServiceGrantTransport", runFset.Position(run.Pos()))
	}
}

// insideLeaderGo reports whether the selector at the end of stack is within the argument list of
// a call to leaderGo (the innermost enclosing call expression's function is that name).
func insideLeaderGo(stack []ast.Node) bool {
	for i := len(stack) - 2; i >= 0; i-- {
		call, ok := stack[i].(*ast.CallExpr)
		if !ok {
			continue
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			return fn.Name == "leaderGo"
		case *ast.SelectorExpr:
			return fn.Sel.Name == "leaderGo"
		}
		return false
	}
	return false
}

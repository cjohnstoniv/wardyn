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
		for _, anc := range stack {
			switch anc.(type) {
			case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
				t.Errorf("boot_serve.go:%d: %s is under a condition; it must start on every replica, with or without a runner",
					fset.Position(sel.Pos()).Line, sel.Sel.Name)
			}
		}
		return true
	})
	for _, name := range []string{"StartServiceGrantRefresher", "RunServiceGrantCleanup"} {
		if !found[name] {
			t.Errorf("startBackgroundWorkers no longer starts srv.%s", name)
		}
	}

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

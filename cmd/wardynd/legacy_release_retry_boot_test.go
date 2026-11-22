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

// The retry of the legacy Docker drives' owed release audit rows runs on every replica, with portals on or off: a
// `go goSafe(...)` statement at the top level of startBackgroundWorkers (no condition, no leader gate) whose
// function calls RunLegacyDockerReleaseRetry. Its behaviour is internal/api's; what only this package can break is
// the wiring, and removing the line fails no other test.
func TestStartBackgroundWorkers_StartsTheLegacyDockerReleaseRetryOnEveryReplica(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), "cmd", "wardynd", "boot_serve.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != "startBackgroundWorkers" {
			continue
		}
		for _, stmt := range fd.Body.List { // top-level statements only: a call nested under an if is gated
			gs, ok := stmt.(*ast.GoStmt)
			if !ok || len(gs.Call.Args) != 2 {
				continue
			}
			if id, ok := gs.Call.Fun.(*ast.Ident); !ok || id.Name != "goSafe" {
				continue
			}
			lit, ok := gs.Call.Args[1].(*ast.FuncLit)
			if !ok {
				continue
			}
			ast.Inspect(lit.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RunLegacyDockerReleaseRetry" {
						started++
					}
				}
				return true
			})
		}
	}
	if started != 1 {
		t.Fatalf("startBackgroundWorkers starts RunLegacyDockerReleaseRetry %d times in a top-level go goSafe statement, want 1", started)
	}
}

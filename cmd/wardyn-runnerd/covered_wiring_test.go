// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestCoveredIsWiredToTheManager pins A2's wiring (#3667 F1): the dispatcher runnerRuntimeWith builds
// answers Status and AgentStatus of a sandbox its substrate cannot observe now (macOS VMs off) with an
// error, never "not found", only while its Config carries Covered: manager.CoversRef. Docker and k8s
// runnerd both build through runnerRuntimeWith (production_k8s.go), so this one literal covers both.
func TestCoveredIsWiredToTheManager(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "production_docker.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var configs, wired int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runnerRuntimeWith" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isSelectorOf(lit.Type, "runneragent", "Config") {
				return true
			}
			configs++
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Covered" && isSelectorOf(kv.Value, "manager", "CoversRef") {
					wired++
				}
			}
			return true
		})
	}
	if configs != 1 || wired != 1 {
		t.Fatalf("runnerRuntimeWith builds %d runneragent.Config literal(s), %d with Covered: manager.CoversRef; want exactly 1 of each", configs, wired)
	}
}

// isSelectorOf reports whether e is the selector expression x.sel.
func isSelectorOf(e ast.Expr, x, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == x && s.Sel.Name == sel
}

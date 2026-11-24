// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDockerRestartGetsProvisionalDeadlines pins that runnerRuntimeWith gives a restarted runner's runs their
// provisional deadlines before the startup lapse check: without the call, every proxy is contained at once
// on a Docker restart, and no manager test would fail. The k8s runner calls it again on its own and the
// second call adds nothing.
func TestDockerRestartGetsProvisionalDeadlines(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "production_docker.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var deadlines, checks []int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runnerRuntimeWith" || fn.Body == nil {
			continue
		}
		for i, stmt := range fn.Body.List {
			es, ok := stmt.(*ast.ExprStmt)
			if !ok {
				continue
			}
			call, ok := es.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			switch {
			case isSelectorOf(call.Fun, "manager", "ProvisionalDeadlines") && len(call.Args) == 0:
				deadlines = append(deadlines, i)
			case isIdent(call.Fun, "checkLapsesAtStartup") && len(call.Args) == 2 && isIdent(call.Args[0], "ctx") && isIdent(call.Args[1], "manager"):
				checks = append(checks, i)
			}
		}
	}
	if len(deadlines) != 1 || len(checks) != 1 {
		t.Fatalf("runnerRuntimeWith has %d top-level manager.ProvisionalDeadlines() and %d checkLapsesAtStartup(ctx, manager) statements, want exactly 1 of each", len(deadlines), len(checks))
	}
	if deadlines[0] >= checks[0] {
		t.Fatal("runnerRuntimeWith calls checkLapsesAtStartup before manager.ProvisionalDeadlines(): every proxy is contained before its restart deadline exists")
	}
}

// isIdent reports whether e is the identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

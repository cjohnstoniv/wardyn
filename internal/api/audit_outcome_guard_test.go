// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestAuditOutcomesAreStorable pins the audit table's outcome CHECK
// (success, failure, denied) at every producer in this package: an
// auditEvent call whose outcome argument is a string literal, and a
// types.AuditEvent literal whose Outcome is one. A row with any other outcome
// is refused by the table and never reaches the trail.
func TestAuditOutcomesAreStorable(t *testing.T) {
	storable := map[string]bool{"success": true, "failure": true, "denied": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	check := func(e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		checked++
		if v, _ := strconv.Unquote(lit.Value); !storable[v] {
			t.Errorf("%s: audit outcome %s is not success, failure or denied", fset.Position(lit.Pos()), lit.Value)
		}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// auditEvent(runID, actorType, actor, action, target, outcome, data)
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "auditEvent" && len(n.Args) == 7 {
					check(n.Args[5])
				}
			case *ast.CompositeLit:
				if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "AuditEvent" {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Outcome" {
								check(kv.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
	if checked < 100 {
		t.Fatalf("checked %d outcome literals; the guard is not seeing the producers", checked)
	}
}

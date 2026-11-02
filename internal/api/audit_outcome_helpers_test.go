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

// TestAuditOutcomeHelpersAreStorable is TestAuditOutcomesAreStorable one call
// further out: a helper that takes an outcome and hands it on (buildAudit,
// auditUI, finalizeRunTail, …) is found by following its string parameter into
// an auditEvent outcome, an AuditEvent Outcome or another such helper, and every
// string literal passed in that position must be success, failure or denied.
// recordAudit drops a refused row with a log line only, so a literal the table
// refuses loses the row.
func TestAuditOutcomeHelpersAreStorable(t *testing.T) {
	storable := map[string]bool{"success": true, "failure": true, "denied": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	type fn struct {
		key    string // a declared func's name, or file:name for a closure
		params []string
		body   *ast.BlockStmt
		file   string
	}
	fset := token.NewFileSet()
	var fns []fn
	files := map[string]*ast.File{}
	stringParams := func(ft *ast.FuncType) []string {
		var out []string
		for _, f := range ft.Params.List {
			typ, _ := f.Type.(*ast.Ident)
			for _, id := range f.Names {
				name := ""
				if typ != nil && typ.Name == "string" {
					name = id.Name
				}
				out = append(out, name)
			}
			if len(f.Names) == 0 {
				out = append(out, "")
			}
		}
		return out
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
		files[name] = f
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Body != nil {
					fns = append(fns, fn{key: n.Name.Name, params: stringParams(n.Type), body: n.Body, file: name})
				}
			case *ast.AssignStmt:
				if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
					id, ok := n.Lhs[0].(*ast.Ident)
					lit, isLit := n.Rhs[0].(*ast.FuncLit)
					if ok && isLit {
						fns = append(fns, fn{key: name + ":" + id.Name, params: stringParams(lit.Type), body: lit.Body, file: name})
					}
				}
			}
			return true
		})
	}

	// helpers[key][i]: argument i of key is an audit outcome.
	helpers := map[string]map[int]bool{"auditEvent": {5: true}}
	callee := func(call *ast.CallExpr, file string) string {
		switch f := call.Fun.(type) {
		case *ast.Ident:
			if _, ok := helpers[file+":"+f.Name]; ok {
				return file + ":" + f.Name
			}
			return f.Name
		case *ast.SelectorExpr:
			return f.Sel.Name
		}
		return ""
	}
	// sinks calls visit with every expression in an outcome position under n.
	sinks := func(n ast.Node, file string, visit func(ast.Expr)) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				for i := range helpers[callee(n, file)] {
					if i < len(n.Args) {
						visit(n.Args[i])
					}
				}
			case *ast.CompositeLit:
				if sel, ok := n.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "AuditEvent" {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Outcome" {
								visit(kv.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
	for grew := true; grew; {
		grew = false
		for _, f := range fns {
			sinks(f.body, f.file, func(e ast.Expr) {
				id, ok := e.(*ast.Ident)
				if !ok {
					return
				}
				for i, p := range f.params {
					if p != "" && p == id.Name && !helpers[f.key][i] {
						if helpers[f.key] == nil {
							helpers[f.key] = map[int]bool{}
						}
						helpers[f.key][i], grew = true, true
					}
				}
			})
		}
	}
	if !helpers["workspace_run_image.go:buildAudit"][0] || !helpers["auditUI"][5] {
		t.Fatalf("the guard no longer finds buildAudit or auditUI as outcome helpers: %v", helpers)
	}

	checked := 0
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			key := callee(call, name)
			if key == "auditEvent" { // TestAuditOutcomesAreStorable's
				return true
			}
			for i := range helpers[key] {
				if i >= len(call.Args) {
					continue
				}
				lit, ok := call.Args[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				checked++
				if v, _ := strconv.Unquote(lit.Value); !storable[v] {
					t.Errorf("%s: audit outcome %s passed to %s is not success, failure or denied",
						fset.Position(lit.Pos()), lit.Value, key)
				}
			}
			return true
		})
	}
	if checked < 50 {
		t.Fatalf("checked %d outcome literals passed to helpers; the guard is not seeing them", checked)
	}
}

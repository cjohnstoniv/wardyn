// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Create and the pool queue's start launch the same run: the package builds a createRunLaunch in one
// place (runFold.launch), that literal sets every field of the struct, and both doors hand
// finishCreateRunLaunch what it returns. A second literal, or a field the one literal leaves out, is
// the drift that once started a queued run without its start folder, harness set and main drive.
func TestCreateRunLaunchIsBuiltOnceFromTheFold(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	var literals []*ast.CompositeLit
	launchCalls := map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				if st, ok := n.Type.(*ast.StructType); ok && n.Name.Name == "createRunLaunch" {
					for _, fl := range st.Fields.List {
						for _, id := range fl.Names {
							fields = append(fields, id.Name)
						}
					}
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "createRunLaunch" {
					literals = append(literals, n)
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "finishCreateRunLaunch" && len(n.Args) == 2 {
					if call, ok := n.Args[1].(*ast.CallExpr); ok {
						if inner, ok := call.Fun.(*ast.SelectorExpr); ok && inner.Sel.Name == "launch" {
							launchCalls[name]++
							return true
						}
					}
					if id, ok := n.Args[1].(*ast.Ident); ok && id.Name == "launch" {
						launchCalls[name]++
						return true
					}
					t.Errorf("%s: finishCreateRunLaunch is handed something other than runFold.launch's result", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	if len(fields) == 0 {
		t.Fatal("no createRunLaunch struct found")
	}
	if len(literals) != 1 {
		for _, l := range literals {
			t.Logf("createRunLaunch literal at %s", fset.Position(l.Pos()))
		}
		t.Fatalf("%d createRunLaunch literals, want exactly one (runFold.launch)", len(literals))
	}
	var set []string
	for _, el := range literals[0].Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok {
				set = append(set, k.Name)
			}
		}
	}
	for _, f := range fields {
		if !slices.Contains(set, f) {
			t.Errorf("runFold.launch leaves createRunLaunch.%s unset", f)
		}
	}
	for _, door := range []string{"runs.go", "run_queue_start.go"} {
		if launchCalls[door] == 0 {
			t.Errorf("%s does not launch through runFold.launch", door)
		}
	}
}

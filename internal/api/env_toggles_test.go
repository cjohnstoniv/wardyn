// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnvTogglesListEveryEnvEnabledRead keeps envToggles complete. envEnabled
// runs per request and exits 2 on a garbage value, so a switch read through it
// but missing from envToggles is never checked at boot: the daemon would start,
// then die on the first request that reached the read (#202).
func TestEnvTogglesListEveryEnvEnabledRead(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	var reads []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				if len(n.Names) == 1 && n.Names[0].Name == "envToggles" && len(n.Values) == 1 {
					if lit, ok := n.Values[0].(*ast.CompositeLit); ok {
						for _, e := range lit.Elts {
							if id, ok := e.(*ast.Ident); ok {
								listed[id.Name] = true
							}
						}
					}
				}
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "envEnabled" && len(n.Args) == 1 {
					arg, ok := n.Args[0].(*ast.Ident)
					if !ok {
						t.Errorf("%s: envEnabled takes a named const listed in envToggles, not an expression", fset.Position(n.Pos()))
						return true
					}
					reads = append(reads, arg.Name)
				}
			}
			return true
		})
	}
	if len(listed) == 0 || len(reads) == 0 {
		t.Fatalf("found envToggles entries %v and envEnabled reads %v; the pattern moved", listed, reads)
	}
	for _, r := range reads {
		if !listed[r] {
			t.Errorf("envEnabled(%s) is read per request but %s is not in envToggles, so a garbage value is never refused at boot", r, r)
		}
	}
}

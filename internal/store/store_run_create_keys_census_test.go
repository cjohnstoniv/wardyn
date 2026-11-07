// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRunInsertWritersBindRunCreateKey: every function that executes createRunSQL also calls
// bindRunCreateKey, so a run writer added later cannot insert a run that an Idempotency-Key was meant to
// guard without binding the key in the same transaction.
func TestRunInsertWritersBindRunCreateKey(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	writers := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				inserts, binds := false, false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.Ident:
						inserts = inserts || n.Name == "createRunSQL"
					case *ast.CallExpr:
						if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "bindRunCreateKey" {
							binds = true
						}
					}
					return true
				})
				if !inserts {
					continue
				}
				writers++
				if !binds {
					t.Errorf("%s executes createRunSQL but never calls bindRunCreateKey", fn.Name.Name)
				}
			}
		}
	}
	if writers < 3 {
		t.Errorf("found %d functions executing createRunSQL, want at least CreateRun, CreateRunUnderCap and CreateQueuedRun", writers)
	}
}

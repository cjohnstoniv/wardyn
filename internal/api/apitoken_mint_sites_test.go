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

// TestAPITokenMintSites_OnlyTheSelfServiceMint is #1477's structural pin: no
// route can create a token that acts as another person, because the one door
// that writes an api_tokens row is the self-service mint. insertAPIToken has
// exactly one caller, handleCreateAPIToken; CreateAPIToken has exactly one,
// insertAPIToken; and no non-test code in this package sets MintedBy on a
// types.APIToken (the minted_by column is read here, never written).
func TestAPITokenMintSites_OnlyTheSelfServiceMint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	insert, create := map[string]int{}, map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						switch sel.Sel.Name {
						case "insertAPIToken":
							insert[fn.Name.Name]++
						case "CreateAPIToken":
							create[fn.Name.Name]++
						}
					}
				case *ast.CompositeLit:
					sel, ok := x.Type.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "APIToken" {
						return true
					}
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "MintedBy" {
								t.Errorf("%s: %s sets APIToken.MintedBy — only a pre-0.8.5 token carries one", f, fn.Name.Name)
							}
						}
					}
				}
				return true
			})
		}
	}
	if len(insert) != 1 || insert["handleCreateAPIToken"] != 1 {
		t.Errorf("insertAPIToken callers = %v, want exactly handleCreateAPIToken once", insert)
	}
	if len(create) != 1 || create["insertAPIToken"] != 1 {
		t.Errorf("Store.CreateAPIToken callers = %v, want exactly insertAPIToken once", create)
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// operatorNamespaceWriters are the only callers of a secret store's Put outside
// putOwned: the daemon's own boot keys, written into the operator namespace,
// which no erase reaches. A person's credential is never written from here.
var operatorNamespaceWriters = map[string]bool{
	"cmd/wardynd/boot_keys.go":   true,
	"cmd/wardynd/boot_hybrid.go": true,
}

// TestCredentialWritersRouteThroughPutOwned pins the one-path rule of #1511:
// every write into a person's secret namespace is made inside putOwned's
// callback, which hands it a context fenced to the owner's erase generation. A
// writer that called Put directly would not be refused after an erase.
func TestCredentialWritersRouteThroughPutOwned(t *testing.T) {
	root := filepath.Join("..", "..")
	var direct []string
	routed := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() && (strings.HasPrefix(rel, "internal/secretstore") || strings.Contains(rel, "testdata")) {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return perr
			}
			inside := 0
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isPutOwnedCall(call) {
					for _, a := range call.Args {
						if lit, ok := a.(*ast.FuncLit); ok {
							inside += countStorePuts(lit)
						}
					}
				}
				return true
			})
			routed += inside
			total := countStorePuts(f)
			if total > inside && !operatorNamespaceWriters[rel] {
				direct = append(direct, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range direct {
		t.Errorf("%s writes a secret with Put outside putOwned: route it through putOwned so an erase of its owner refuses it (secretstore.BeginWrite)", rel)
	}
	if routed == 0 {
		t.Fatal("found no Put inside putOwned: the guard is not looking at the writers")
	}
}

func isPutOwnedCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "putOwned"
}

// countStorePuts counts n's Put calls with a context, a name and a value: a
// secret store's, as opposed to a router's Put(path, handler).
func countStorePuts(n ast.Node) int {
	c := 0
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 3 {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Put" {
				c++
			}
		}
		return true
	})
	return c
}

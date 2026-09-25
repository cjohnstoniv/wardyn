// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// strictProviderReaders are the only functions besides ownSecret (which takes
// its name as a parameter, so the matcher never sees one there) that may hand a
// per-person model-provider credential name (wardyn-provider-<uid>-*) to a
// namespaced store view, each for the reason given. Store.For(owner).Get FALLS
// BACK to the operator's row by contract, so anything else would serve the
// operator's credential to a person who stored none (multi-provider rule 10).
var strictProviderReaders = map[string]string{
	"readAWSSSOBlob":        "List-then-Get under a per-user scope, the -sso twin of ownSecret",
	"deleteSpentAWSSSOBlob": "hands the view to deleteDeadCredential, a Delete, which never falls back",
}

// TestProviderCredentialReadsAreStrict walks internal/api and
// internal/secretstore and fails on any Get of a provider-derived name, or any
// call handing a For(...) store view and a provider-derived name to a helper,
// outside strictProviderReaders. "Provider-derived" is static: providerSecretName,
// the prefix constants, a "wardyn-provider-" literal, awsSSOScope.ssoSecret, or a
// local assigned from one. A name that only arrives at run time (a grant's
// SecretName) is out of its reach; those sinks are pinned by
// TestPG_ModelCredNeverFromOperatorNamespace instead.
func TestProviderCredentialReadsAreStrict(t *testing.T) {
	hits := map[string]int{}
	fset := token.NewFileSet()
	for _, root := range []string{".", "../secretstore"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				for _, pos := range bareProviderReads(fn) {
					if _, ok := strictProviderReaders[fn.Name.Name]; ok {
						hits[fn.Name.Name]++
						continue
					}
					t.Errorf("%s: %s reads a wardyn-provider- name through a store view that falls back to the operator's row; "+
						"read it with ownSecret (or readAWSSSOBlob for -sso)", fset.Position(pos), fn.Name.Name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// A guard that stopped seeing its own allowlisted sites has gone blind.
	for name := range strictProviderReaders {
		if hits[name] == 0 {
			t.Errorf("the guard no longer sees %s's provider read; update strictProviderReaders or the matcher", name)
		}
	}
}

// bareProviderReads returns the position of every call in fn that reads a
// provider-derived name: a Get, or a call that also carries a For(...) view.
func bareProviderReads(fn *ast.FuncDecl) []token.Pos {
	provider, views := map[string]bool{}, map[string]bool{}
	// Taint locals to a fixpoint: name := providerSecretName(...), st := x.For(o).
	for changed := true; changed; {
		changed = false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != len(as.Rhs) {
				return true
			}
			for i, lhs := range as.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if !provider[id.Name] && providerName(as.Rhs[i], provider) {
					provider[id.Name], changed = true, true
				}
				if !views[id.Name] && storeView(as.Rhs[i], views) {
					views[id.Name], changed = true, true
				}
			}
			return true
		})
	}
	var out []token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		named, viewArg := false, false
		for _, a := range call.Args {
			named = named || providerName(a, provider)
			viewArg = viewArg || storeView(a, views)
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if named && (viewArg || isSel && sel.Sel.Name == "Get") {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// providerName reports whether e evaluates to a wardyn-provider- name.
func providerName(e ast.Expr, tainted map[string]bool) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING && strings.Contains(e.Value, "wardyn-provider-")
	case *ast.Ident:
		return tainted[e.Name] || e.Name == "providerSecretPrefix"
	case *ast.SelectorExpr:
		return e.Sel.Name == "ModelProviderSecretPrefix"
	case *ast.ParenExpr:
		return providerName(e.X, tainted)
	case *ast.BinaryExpr:
		return providerName(e.X, tainted) || providerName(e.Y, tainted)
	case *ast.CallExpr:
		switch f := e.Fun.(type) {
		case *ast.Ident:
			return f.Name == "providerSecretName"
		case *ast.SelectorExpr:
			return f.Sel.Name == "ssoSecret" || f.Sel.Name == "Sprintf" && slices.ContainsFunc(e.Args,
				func(a ast.Expr) bool { return providerName(a, tainted) })
		}
	}
	return false
}

// storeView reports whether e is a For(...) store view, which falls back.
func storeView(e ast.Expr, views map[string]bool) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return views[e.Name]
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "For"
	}
	return false
}

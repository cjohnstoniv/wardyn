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

// strictProviderReaders may hand a per-person model-provider credential name
// (wardyn-provider-<uid>-*) to a namespaced store view, each for the reason
// given. Store.For(owner).Get FALLS BACK to the operator's row by contract, so
// anything else would serve the operator's credential to a person who stored
// none (multi-provider rule 10).
var strictProviderReaders = map[string]string{
	"ownSecret":             "List-then-Get under the owner's scope: the strict reader itself",
	"readAWSSSOBlob":        "List-then-Get under a per-user scope, the -sso twin of ownSecret",
	"deleteSpentAWSSSOBlob": "hands the view to deleteDeadCredential, a Delete, which never falls back",
	"readADOEntraBlob":      "List-then-Get under a per-user scope, the Azure DevOps twin of ownSecret",
}

// readersThatMustRefuseProviderNames is the register of every bare
// For(owner).Get whose name only arrives at run time, so the matcher cannot
// tell whether it is a provider name. Each falls back to the operator's row, so
// each must refuse a wardyn-provider- name before its Get; PR #1040 adds that
// refusal to the two dispatch resolvers. A new bare view Get fails the guard
// until it is read with ownSecret or listed here with its reason.
var readersThatMustRefuseProviderNames = map[string]string{
	"handleInternalInjection":     "the minted grant's SecretName, owner-then-operator by design for injections",
	"resolveLLMInspectionSecrets": "the policy's LLM-inspection secret names, owner-then-operator at dispatch",
	"resolveEnvSecretGrants":      "an env_secret grant's SecretName, owner-then-operator at dispatch",
}

// TestProviderCredentialReadsAreStrict walks internal/api and
// internal/secretstore and fails, outside the two registers above, on any Get
// through a For(...) store view (directly, through a local, or through a method
// value), any other Get of a provider-derived name, or any call handing a
// For(...) view and a provider-derived name to a helper. "Provider-derived" is
// static: providerSecretName, the prefix constants, a "wardyn-provider-"
// literal, awsSSOScope.ssoSecret, or a local assigned from one. For("") is the
// operator's own namespace, which has nothing to fall back to, so it is not a
// view here; the conformance suite (secretstoretest) asserts the fallback on
// purpose and is not walked. Run-time sinks are also pinned by
// TestPG_ModelCredNeverFromOperatorNamespace.
func TestProviderCredentialReadsAreStrict(t *testing.T) {
	hits := map[string]int{}
	fset := token.NewFileSet()
	for _, root := range []string{".", "../secretstore"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && d.Name() == "secretstoretest" {
				return filepath.SkipDir
			}
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
					_, strict := strictProviderReaders[fn.Name.Name]
					_, listed := readersThatMustRefuseProviderNames[fn.Name.Name]
					if strict || listed {
						hits[fn.Name.Name]++
						continue
					}
					t.Errorf("%s: %s reads through a store view that falls back to the operator's row; read a wardyn-provider- name "+
						"with ownSecret (or readAWSSSOBlob for -sso), or list a run-time-name reader in readersThatMustRefuseProviderNames",
						fset.Position(pos), fn.Name.Name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// A guard that stopped seeing its own allowlisted sites has gone blind.
	for _, register := range []map[string]string{strictProviderReaders, readersThatMustRefuseProviderNames} {
		for name := range register {
			if hits[name] == 0 {
				t.Errorf("the guard no longer sees %s's store read; update its register or the matcher", name)
			}
		}
	}
}

// bareProviderReads returns the position of every call in fn that may read a
// provider name through a fallback: any Get on a For(...) view, a Get of a
// provider-derived name, or a call carrying both a view and such a name.
func bareProviderReads(fn *ast.FuncDecl) []token.Pos {
	provider, views, getters := map[string]bool{}, map[string]bool{}, map[string]bool{}
	// Taint locals to a fixpoint: name := providerSecretName(...), st := x.For(o),
	// get := st.Get (a method value, so get(...) is a view Get under another name).
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
				if !getters[id.Name] && viewGet(as.Rhs[i], views) {
					getters[id.Name], changed = true, true
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
		// Every Get on a view counts, whatever its name argument: a name that
		// reaches it through a parameter, a struct field or a helper is
		// invisible here, so each such site is listed and reviewed instead.
		getter, isIdent := call.Fun.(*ast.Ident)
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if viewGet(call.Fun, views) || isIdent && getters[getter.Name] ||
			named && (viewArg || isSel && sel.Sel.Name == "Get") {
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

// viewGet reports whether e is the Get method of a For(...) store view.
func viewGet(e ast.Expr, views map[string]bool) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Get" && storeView(sel.X, views)
}

// storeView reports whether e is a For(...) store view, which falls back.
// For("") is the operator's namespace itself, so it is not one.
func storeView(e ast.Expr, views map[string]bool) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return views[e.Name]
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		operator := len(e.Args) == 1 && isEmptyString(e.Args[0])
		return ok && sel.Sel.Name == "For" && !operator
	}
	return false
}

func isEmptyString(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

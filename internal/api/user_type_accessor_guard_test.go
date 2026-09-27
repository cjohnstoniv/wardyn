// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestOIDCUserTypeFromContextReadOnlyInHTTP: the token lane publishes a
// caller's user type only under this package's own key, so a control reading
// oidc.UserTypeFromContext gets "" for every token caller — "no type", not
// "unknown type", which skips the user_type_unknown refusal. Only http.go,
// where the SSO branch copies the session's stamp into withHumanIdentity, may
// read it; everything else reads oidcUserTypeFromContext.
func TestOIDCUserTypeFromContextReadOnlyInHTTP(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "http.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "UserTypeFromContext" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "oidc" {
				t.Errorf("%s reads oidc.UserTypeFromContext, which is empty for every API-token caller; read oidcUserTypeFromContext", fset.Position(sel.Pos()))
			}
			return true
		})
	}
}

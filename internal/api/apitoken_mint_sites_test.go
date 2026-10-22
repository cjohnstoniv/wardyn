// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestAPITokenMintSites_OnlyTheSelfServiceMint is #1477's structural pin, widened for
// #1508: no route can create a token that acts as another person, because the doors
// that write an api_tokens row are the self-service mint and an invitation's claim,
// and both run the one mint core (mintGuards, prepareMint), so the caller's own
// session decides who the token belongs to. Each core function has exactly those two
// callers; insertAPIToken has exactly one, handleCreateAPIToken; ClaimInvitation
// exactly one, handleClaimInvitation; CreateAPIToken exactly one, insertAPIToken; a CLI sign-in's
// approval runs mintGuards and its tokens are built by mintCLIToken, prepareMint's third caller;
// and no non-test code in this package sets MintedBy on a types.APIToken (the
// minted_by column is read here, never written).
func TestAPITokenMintSites_OnlyTheSelfServiceMint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := map[string]map[string]int{}
	for _, name := range []string{"insertAPIToken", "CreateAPIToken", "ClaimInvitation", "CreateServiceToken", "mintGuards", "prepareMint",
		"CreateCLISession", "RotateCLISession", "CreateCLIAdminToken", "mintCLIToken"} {
		calls[name] = map[string]int{}
	}
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
						if m, ok := calls[sel.Sel.Name]; ok {
							m[fn.Name.Name]++
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
	for name, want := range map[string][]string{
		"insertAPIToken":     {"handleCreateAPIToken"},
		"CreateAPIToken":     {"insertAPIToken"},
		"ClaimInvitation":    {"handleClaimInvitation"},
		"mintGuards":         {"handleApproveCLISignIn", "handleClaimInvitation", "handleCreateAPIToken", "handleMintServiceToken"},
		"CreateServiceToken": {"handleMintServiceToken"}, // the sponsor's mint of a service identity's token (#1508)
		"prepareMint":        {"handleClaimInvitation", "handleCreateAPIToken", "mintCLIToken"},
		// CLI sign-in (cli_sessions.go): the approval runs mintGuards; every CLI token is built by
		// mintCLIToken through prepareMint from the approver's snapshot, and each store write has one door.
		"mintCLIToken":        {"createCLISession", "mintCLIAdmin", "rotateCLISession"},
		"CreateCLISession":    {"createCLISession"},
		"RotateCLISession":    {"rotateCLISession"},
		"CreateCLIAdminToken": {"mintCLIAdmin"},
	} {
		got := slices.Sorted(maps.Keys(calls[name]))
		if !slices.Equal(got, want) {
			t.Errorf("%s callers = %v, want exactly %v", name, got, want)
		}
		for caller, n := range calls[name] {
			if n != 1 {
				t.Errorf("%s calls %s %d times, want once", caller, name, n)
			}
		}
	}
}

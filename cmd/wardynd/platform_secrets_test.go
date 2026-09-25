// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// bootKeyNames returns the name every loadOrCreateSecret call in this
// package's non-test source bootstraps, read from the AST rather than kept by
// hand: a hand list is how wardyn-internal-ca, the fifth boot key, reached the
// reserved sets but not secretstore.PlatformNames. A name argument that is not
// a string literal or a package-level string constant fails the test, so a
// call site the derivation cannot read is never silently left out.
func bootKeyNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	consts := map[string]string{}
	var calls []*ast.CallExpr
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, spec := range g.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								consts[n.Name], _ = strconv.Unquote(lit.Value)
							}
						}
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "loadOrCreateSecret" {
					calls = append(calls, c)
				}
			}
			return true
		})
	}
	var names []string
	for _, c := range calls {
		if len(c.Args) < 3 {
			t.Fatalf("%s: loadOrCreateSecret call without a name argument", fset.Position(c.Pos()))
		}
		var name string
		switch a := c.Args[2].(type) {
		case *ast.BasicLit:
			name, _ = strconv.Unquote(a.Value)
		case *ast.Ident:
			name = consts[a.Name]
		}
		if name == "" {
			t.Fatalf("%s: loadOrCreateSecret name is not a string literal or package-level string constant; the platform-key derivation cannot read it", fset.Position(c.Pos()))
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatal("no loadOrCreateSecret call sites found: the derivation reads nothing and every check below would pass vacuously")
	}
	sort.Strings(names)
	return names
}

// TestPlatformSecretsAreReservedEverywhere is the parity check the reserved-name
// sets never had. Two packages keep their own map of names nobody may write or
// resolve — internal/api's reservedSecretNames and internal/broker's
// reservedBrokerSecretNames — and NEITHER can see the constants above, so every
// platform key so far has been added to the maps by hand and by memory.
// wardyn-ui-session-key was missed in both; wardyn-ssh-host-key was missed in
// one. This is the only package that can see all three, which is why the test
// lives here rather than beside either map.
//
// The list is the keys the daemon GENERATES for itself (loadOrCreateSecret,
// derived by bootKeyNames):
// nobody authors them, nobody may overwrite them, and no grant may name them, so
// both guards must refuse all of them. The GitHub App pair (secretGitHubAppID /
// secretGitHubAppKey) is deliberately NOT here — those are operator-PROVIDED
// credentials that must stay Put-able through the secrets API, and they are
// sealed broker-side only (see api.sinkReservedSecret's doc comment).
//
// Adding a boot key and not adding it to both maps fails here.
func TestPlatformSecretsAreReservedEverywhere(t *testing.T) {
	for _, name := range bootKeyNames(t) {
		if !api.ReservedPlatformSecret(name) {
			t.Errorf("%s: not in internal/api's reserved set — GET /secrets lists it and PUT/DELETE clobbers it", name)
		}
		if !broker.ReservedSecretName(name) {
			t.Errorf("%s: not in internal/broker's reserved set — a git_pat/ssh_key grant naming it hands the raw key into a sandbox", name)
		}
	}
}

// TestBootKeysAreThePlatformSet ties secretstore.PlatformNames to the boot keys
// themselves. That one map decides where an external store files a row
// (secretstore.Kind: Vault's <prefix>/platform/ path, Key Vault's -platform-
// name) and so which identity may read it; a boot key missing from it is filed
// as an operator credential, readable by the credentials role. The map must
// hold exactly the loadOrCreateSecret names: one missing leaves a boot key
// outside the platform domain, one extra pulls a credential into it.
func TestBootKeysAreThePlatformSet(t *testing.T) {
	boot := bootKeyNames(t)
	seen := map[string]bool{}
	for _, n := range boot {
		seen[n] = true
		if !secretstore.PlatformNames[n] {
			t.Errorf("boot key %q is not in secretstore.PlatformNames: an external store files it as an operator credential", n)
		}
		if secretstore.Kind("", n) != "platform" {
			t.Errorf("secretstore.Kind(\"\", %q) = %q, want platform", n, secretstore.Kind("", n))
		}
	}
	for n := range secretstore.PlatformNames {
		if !seen[n] {
			t.Errorf("secretstore.PlatformNames holds %q, which no loadOrCreateSecret call bootstraps", n)
		}
	}
}

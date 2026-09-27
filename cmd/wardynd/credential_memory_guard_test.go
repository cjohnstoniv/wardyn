// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Credential memory hygiene (credential-storage design §2.8, CS-4), pinned
// mechanically:
//
//   - Decrypt sites. A stored credential becomes plaintext in exactly the
//     places listed in decryptSites below: the envelope open, the key-service
//     unwrap, the one-time age conversion and the external store read. A new
//     call to any of those primitives anywhere else in cmd/ or internal/ is a
//     new place plaintext appears, and this guard makes it a reviewed change
//     instead of an accident. It does not pin who calls Store.Get: that is the
//     audit guard's question (every Get audited once, CS-13).
//   - Core dumps. Every binary that holds credentials calls nodump.Disable as
//     the first statement of its entry point.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// decryptSite is one call of a primitive that turns a stored credential into
// plaintext, found in a function.
type decryptSite struct {
	relFile, fn, kind string
	line              int
}

// fileScope is which decrypt primitives a file can reach. inKEK marks a file of
// package kek itself, whose own Open is called unqualified; the other three are
// set by importing the package that defines the method.
type fileScope struct{ inKEK, kek, aead, external bool }

func scopeOf(f *ast.File) fileScope {
	sc := fileScope{inKEK: f.Name.Name == "kek"}
	sc.kek = sc.inKEK
	for _, im := range f.Imports {
		switch strings.Trim(im.Path.Value, `"`) {
		case "crypto/cipher":
			sc.aead = true
		case "github.com/cjohnstoniv/wardyn/internal/secretstore/kek":
			sc.kek = true
		case "github.com/cjohnstoniv/wardyn/internal/secretstore":
			sc.external = true
		}
	}
	return sc
}

// classifyDecryptCall names the decrypt primitive call is, or "". Unwrap, Open
// and Get are matched by name and arity alone, so each counts only in a file
// that imports the package defining it.
func classifyDecryptCall(call *ast.CallExpr, sc fileScope) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if sc.inKEK && fun.Name == "Open" {
			return "kek.Open"
		}
	case *ast.SelectorExpr:
		x, _ := fun.X.(*ast.Ident)
		switch {
		case x != nil && x.Name == "kek" && fun.Sel.Name == "Open":
			return "kek.Open"
		case x != nil && x.Name == "age" && fun.Sel.Name == "Decrypt":
			return "age.Decrypt"
		case sc.kek && fun.Sel.Name == "Unwrap" && len(call.Args) == 3:
			return "KEK.Unwrap" // kek.KEK: Unwrap(ctx, wrapped, bind)
		case sc.aead && fun.Sel.Name == "Open" && len(call.Args) == 4:
			return "AEAD.Open" // cipher.AEAD: Open(dst, nonce, ciphertext, aad)
		case sc.external && fun.Sel.Name == "Get" && len(call.Args) == 4:
			return "External.Get" // secretstore.External: Get(ctx, owner, name, ref)
		}
	}
	return ""
}

// scanDecryptSites walks every non-test .go file under cmd/ and internal/.
func scanDecryptSites(t *testing.T, root string) (sites []decryptSite, scanned int) {
	t.Helper()
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			scanned++
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			sc := scopeOf(f)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					name = recvTypeName(fd.Recv.List[0].Type) + "." + name
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						if kind := classifyDecryptCall(call, sc); kind != "" {
							sites = append(sites, decryptSite{relFile: rel, fn: name, kind: kind, line: fset.Position(call.Pos()).Line})
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", top, err)
		}
	}
	return sites, scanned
}

func recvTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(v.X)
	case *ast.IndexExpr:
		return recvTypeName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return "?"
}

// decryptSites is every place a stored credential becomes plaintext, keyed
// "file|function|primitive", with why it may. Exactly what is there today;
// nothing added for headroom.
var decryptSites = map[string]string{
	"internal/secretstore/kek/kek.go|Open|AEAD.Open":                      "the envelope's AES-256-GCM open, the one definition every local-mode read goes through",
	"internal/secretstore/kek/kek.go|Local.Unwrap|kek.Open":               "the local KEK unwraps a row's data key",
	"internal/secretstore/pg/pg.go|Store.open|KEK.Unwrap":                 "a local-mode Get unwraps the row's data key, bound to the row's owner and name",
	"internal/secretstore/pg/pg.go|Store.open|kek.Open":                   "a local-mode Get opens the value, bound to the row's owner and name",
	"internal/secretstore/pg/pg.go|rewrap|KEK.Unwrap":                     "-rotate-age-key rewraps a data key; the value itself is never opened",
	"internal/secretstore/pg/convert.go|ageDecrypt|age.Decrypt":           "the one-time conversion of a pre-envelope (age) row to envelope v1",
	"internal/secretstore/pg/external.go|Store.openExternal|External.Get": "a store-mode Get reads the value from the organisation's store, after the pointer row is checked",
	"internal/secretstore/vaultkv/transit.go|Transit.selfTest|KEK.Unwrap": "the Transit boot self-test unwraps a random probe data key it just wrapped, never a stored one",
}

func TestDecryptSitesArePinned(t *testing.T) {
	sites, scanned := scanDecryptSites(t, repoRoot(t))
	if scanned == 0 || len(sites) == 0 {
		t.Fatalf("scanned %d files and matched %d decrypt sites — the root or the matcher is broken", scanned, len(sites))
	}
	seen := map[string]bool{}
	for _, s := range sites {
		key := s.relFile + "|" + s.fn + "|" + s.kind
		seen[key] = true
		if _, ok := decryptSites[key]; !ok {
			t.Errorf("%s:%d: %s calls %s, which turns a stored credential into plaintext, outside the pinned "+
				"decrypt sites. Read it through secretstore.Store.Get instead, or add it to decryptSites with why "+
				"plaintext has to appear there", s.relFile, s.line, s.fn, s.kind)
		}
	}
	var stale []string
	for key := range decryptSites {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("decryptSites pins %s, but no such call exists — drop the stale entry", key)
	}
}

// The primitives matched by method name and arity count only in a file that
// can reach them: an unrelated four-argument Get (a cache, a lookup) in a
// package that never touches a credential is not a decrypt site.
func TestDecryptMatcherNeedsTheDefiningImport(t *testing.T) {
	const src = `package x
import %s
func f() { c.Get(ctx, a, b, d); k.Unwrap(ctx, w, b); g.Open(nil, n, ct, aad) }`
	for _, tc := range []struct {
		imports string
		want    []string
	}{
		{`"sync"`, nil},
		{`("crypto/cipher"; "github.com/cjohnstoniv/wardyn/internal/secretstore"; "github.com/cjohnstoniv/wardyn/internal/secretstore/kek")`,
			[]string{"External.Get", "KEK.Unwrap", "AEAD.Open"}},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", fmt.Sprintf(src, tc.imports), 0)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if kind := classifyDecryptCall(call, scopeOf(f)); kind != "" {
					got = append(got, kind)
				}
			}
			return true
		})
		if !slices.Equal(got, tc.want) {
			t.Errorf("imports %s: matched %v, want %v", tc.imports, got, tc.want)
		}
	}
}

// credentialEntryPoints are the functions that start a process holding
// credentials: wardynd (every mode, maintenance ones included, reads them) and
// the proxy sidecar (it resolves and injects them).
var credentialEntryPoints = map[string]string{
	"cmd/wardynd/main.go":      "main",
	"cmd/wardyn-proxy/main.go": "main",
}

func TestCredentialBinariesDisableCoreDumpsFirst(t *testing.T) {
	root := repoRoot(t)
	for rel, fn := range credentialEntryPoints {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		var body *ast.BlockStmt
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
				body = fd.Body
			}
		}
		if body == nil || len(body.List) == 0 {
			t.Fatalf("%s: no %s() to check", rel, fn)
		}
		if !callsNodumpDisable(body.List[0]) {
			t.Errorf("%s: the first statement of %s() must be `if err := nodump.Disable(); err != nil {…}` — "+
				"this process holds credentials, and until it runs a crash can write them to a core file", rel, fn)
		}
	}
}

func callsNodumpDisable(stmt ast.Stmt) bool {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok {
		return false
	}
	as, ok := ifs.Init.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return false
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "nodump" && sel.Sel.Name == "Disable"
}

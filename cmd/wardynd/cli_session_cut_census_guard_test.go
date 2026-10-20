// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cutWriteRe matches SQL that writes a session revocation cutoff or a sessions-only cut.
var cutWriteRe = regexp.MustCompile(`(?is)(INSERT\s+INTO|UPDATE)\s+oidc_session_(revocations|cuts)\b`)

// TestEveryCutWriterEndsCLISessions is the census over every writer of a session cut: each function whose
// SQL writes oidc_session_revocations or oidc_session_cuts must, in the same function, either hand that
// SQL to store.ExecRevocationCut (which ends the CLI sessions the cut reaches in its transaction) or call
// endCLISessionsOf on its own transaction. A new writer that does neither would leave a revoked or
// signed-out person's CLI sessions renewing until their next refresh check, so it fails here.
//
// It reads the whole tree's non-test Go (internal/ and cmd/wardynd), not one package: the writers today
// are pgSessionRevocations' CutSessions and upsertCutoff (RevokeSub and RevokeAll), and store.PG's
// SuspendIdentity and UnbindIdentity, and none may be added without the CLI end. The cut's callers
// (sign-out, back-channel logout, the SCIM group cut) reach these through oidc.SessionRevocations.
func TestEveryCutWriterEndsCLISessions(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	writers := 0
	for _, dir := range []string{"internal", "cmd/wardynd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if dir == "cmd/wardynd" && filepath.Dir(path) != filepath.Join(root, dir) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				writes, ends := false, false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.BasicLit:
						if x.Kind == token.STRING {
							if v, err := strconv.Unquote(x.Value); err == nil && cutWriteRe.MatchString(v) {
								writes = true
							}
						}
					case *ast.CallExpr:
						name := ""
						switch f := x.Fun.(type) {
						case *ast.Ident:
							name = f.Name
						case *ast.SelectorExpr:
							name = f.Sel.Name
						}
						ends = ends || name == "ExecRevocationCut" || name == "endCLISessionsOf"
					}
					return true
				})
				if writes {
					writers++
					if !ends {
						t.Errorf("%s: %s writes a session cut without ending the CLI sessions it reaches: pass the SQL to store.ExecRevocationCut, or call endCLISessionsOf on the same transaction", rel, fn.Name.Name)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The census found the writers it names above; zero would mean the pattern stopped matching.
	if writers < 4 {
		t.Fatalf("found %d session-cut writers, want at least the 4 named in this test's comment: re-anchor cutWriteRe", writers)
	}
}

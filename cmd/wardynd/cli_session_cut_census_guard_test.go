// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	writers, violations, err := cutWriterCensus(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Errorf("%s writes a session cut without ending the CLI sessions it reaches: pass the SQL to store.ExecRevocationCut, or call endCLISessionsOf on the same transaction", v)
	}
	// The census found the writers it names above; zero would mean the pattern stopped matching.
	if writers < 4 {
		t.Fatalf("found %d session-cut writers, want at least the 4 named in this test's comment: re-anchor cutWriteRe", writers)
	}
}

// The census sees a cut written through a package-level const in another file of the package, and
// passes the same SQL handed to ExecRevocationCut.
func TestCutWriterCensusResolvesPackageConsts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "twin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"sql.go": "package twin\n\nconst cutSQL = `INSERT INTO oidc_session_cuts (sub, cut_at) VALUES ($1, now())`\n\nvar revokeSQL = \"UPDATE oidc_session_\" + \"revocations SET revoked_at = now()\"\n",
		"cut.go": "package twin\n\nfunc plantedWriter(q exec) { q.Exec(cutSQL) }\n\nfunc plantedVarWriter(q exec) { q.Exec(revokeSQL) }\n\n" +
			"func endedWriter(q exec) { store.ExecRevocationCut(nil, q, cutSQL, \"sub\", \"revoked\") }\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writers, violations, err := cutWriterCensus(root)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(violations)
	if want := []string{"internal/twin/cut.go: plantedVarWriter", "internal/twin/cut.go: plantedWriter"}; writers != 3 || !slices.Equal(violations, want) {
		t.Fatalf("census of the planted package = %d writers, violations %v; want 3 writers and %v", writers, violations, want)
	}
}

// cutWriterCensus walks root's internal/ and cmd/wardynd (that directory only) non-test Go and returns
// how many functions write a session cut, and "file: func" for each that neither hands SQL to
// ExecRevocationCut nor calls endCLISessionsOf. A cut's SQL counts as a string literal in the function
// or a package-level string const or var (a literal, or literals joined with +) the function names.
func cutWriterCensus(root string) (int, []string, error) {
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	names := map[*ast.File]string{}
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
			byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], f)
			names[f] = filepath.ToSlash(rel)
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, nil, err
		}
	}
	writers := 0
	var violations []string
	for _, files := range byDir {
		consts := censusPackageStrings(files)
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				writes, ends := false, false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.BasicLit:
						v, ok := censusStringLit(x)
						writes = writes || ok && cutWriteRe.MatchString(v)
					case *ast.Ident:
						v, ok := consts[x.Name]
						writes = writes || ok && cutWriteRe.MatchString(v)
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
						violations = append(violations, names[f]+": "+fn.Name.Name)
					}
				}
			}
		}
	}
	return writers, violations, nil
}

// censusPackageStrings maps each package-level const and var of files whose value is a string literal, or
// literals joined with +, to that value.
func censusPackageStrings(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if v, ok := censusStringExpr(vs.Values[i]); ok {
							out[name.Name] = v
						}
					}
				}
			}
		}
	}
	return out
}

func censusStringExpr(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		return censusStringLit(x)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		a, ok1 := censusStringExpr(x.X)
		b, ok2 := censusStringExpr(x.Y)
		return a + b, ok1 && ok2
	case *ast.ParenExpr:
		return censusStringExpr(x.X)
	}
	return "", false
}

func censusStringLit(x *ast.BasicLit) (string, bool) {
	if x.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(x.Value)
	return v, err == nil
}

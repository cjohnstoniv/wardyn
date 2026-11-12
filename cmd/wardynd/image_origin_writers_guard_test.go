/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestImageOriginWardynHasOneWriter pins the provenance a revive trusts. Revive skips the image check for a run
// whose recorded origin is "wardyn" (run_revive_image.go#Server.reviveImageAdmission), and nothing else on the row
// marks a run Wardyn launched itself, so that origin must be written only by newStepRun. A new reference to the
// constant, a "wardyn" literal set as an origin (in a composite literal or an assignment), a new caller of the
// scoped origin writers, or a new door to newStepRun fails here instead of relying on someone grepping for it. It is a census of symbols, call
// sites and literals, not a data-flow proof. Extend a list only with a writer whose origin no request can choose.
func TestImageOriginWardynHasOneWriter(t *testing.T) {
	root := repoRoot(t)
	want := map[string][]string{
		"ImageOriginWardyn": {
			"internal/api/run_revive_image.go:reviveImageAdmission",
			"internal/api/workspace_run_launch.go:newStepRun",
			"internal/types/image_admission_verdict.go:",
		},
		"recordImageOrigin": {
			"internal/api/run_image_pin.go:conventionImageAdmission",
			"internal/api/run_image_pin.go:recordBuiltImageOrigin",
		},
		"recordBuiltImageOrigin": {
			"internal/api/runs_create.go:admitResolvedImage",
		},
		"SetRunImageAdmission": {
			"internal/api/run_image_pin.go:conventionImageAdmission",
			"internal/api/run_image_pin.go:recordConventionAudit",
			"internal/api/run_image_pin.go:recordImageOrigin",
		},
		// Every run newStepRun makes carries the origin, so each door to it is a Wardyn-launched step whose image
		// no request chooses.
		"newStepRun": {
			"internal/api/harnesscred.go:launchHarnessLoginRun",
			"internal/api/site_config_probe_capacity.go:newProbeRun",
			"internal/api/source_scan.go:launchSourceScanRun",
			"internal/api/workspace_run_launch.go:newWorkspaceStepRun",
		},
		"newProbeRun": {
			"internal/api/site_config_probe.go:runSiteConfigProbe",
		},
		"newWorkspaceStepRun": {
			"internal/api/workspace_run_launch.go:launchRecordRun",
		},
	}
	got := map[string]map[string]bool{}
	var literalOrigins []string
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Errorf("parse %s: %v", rel, perr)
				return nil
			}
			record := func(name, fn string) {
				if _, ok := want[name]; !ok {
					return
				}
				if got[name] == nil {
					got[name] = map[string]bool{}
				}
				got[name][rel+":"+fn] = true
			}
			isWardyn := func(e ast.Expr) bool {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return false
				}
				v, _ := strconv.Unquote(lit.Value)
				return v == "wardyn"
			}
			walk := func(n ast.Node, fn string) {
				ast.Inspect(n, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						record(x.Sel.Name, fn)
					case *ast.Ident:
						if x.Name == "ImageOriginWardyn" {
							record(x.Name, fn)
						}
					case *ast.KeyValueExpr: // Verdict{Origin: "wardyn"}
						if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Origin" && isWardyn(x.Value) {
							literalOrigins = append(literalOrigins, rel+":"+fn)
						}
					case *ast.AssignStmt: // v.Origin = "wardyn", or one of several targets
						for i, lhs := range x.Lhs {
							if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Origin" && len(x.Rhs) == len(x.Lhs) && isWardyn(x.Rhs[i]) {
								literalOrigins = append(literalOrigins, rel+":"+fn)
							}
						}
					}
					return true
				})
			}
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok {
					walk(fd, fd.Name.Name)
				} else {
					walk(decl, "")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for name, allowed := range want {
		var sites []string
		for s := range got[name] {
			sites = append(sites, s)
		}
		sort.Strings(sites)
		if !slices.Equal(sites, allowed) {
			t.Errorf("%s is referenced from %v, want exactly %v: a new writer of a run's image origin must be one whose origin no request can choose", name, sites, allowed)
		}
	}
	if len(literalOrigins) > 0 {
		t.Errorf(`a "wardyn" string literal is used as an image origin at %v: only newStepRun writes it, through types.ImageOriginWardyn`, literalOrigins)
	}
	for _, site := range sqlWritingWardynOrigin(t, root) {
		t.Errorf("%s: an SQL statement naming image_admission carries a wardyn literal; only newStepRun writes that origin, as a bound value", site)
	}
}

// sqlComments matches SQL line and block comments, which prose about Wardyn fills.
var sqlComments = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)

// sqlWritingWardynOrigin is the SQL side the AST walk above cannot read: a Go string literal under internal/store,
// or a migration statement, that itself names image_admission and holds "wardyn" in any case (the store writes the
// column only from a bound verdict, so such a literal is a second writer). A literal split across strings, or
// built at run time, is outside this check.
func sqlWritingWardynOrigin(t *testing.T, root string) []string {
	t.Helper()
	var sites []string
	check := func(where, stmt string) {
		low := strings.ToLower(stmt)
		if strings.Contains(low, "image_admission") && strings.Contains(low, "wardyn") {
			sites = append(sites, where)
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "internal", "store"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
					rel, _ := filepath.Rel(root, fset.Position(lit.Pos()).Filename)
					check(filepath.ToSlash(rel)+":"+strconv.Itoa(fset.Position(lit.Pos()).Line), v)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/store: %v", err)
	}
	migrations, err := filepath.Glob(filepath.Join(root, "internal", "db", "migrations", "*.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no migrations under internal/db/migrations (%v)", err)
	}
	for _, path := range migrations {
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		for i, stmt := range strings.Split(sqlComments.ReplaceAllString(string(b), ""), ";") {
			check(filepath.Base(path)+" statement "+strconv.Itoa(i+1), stmt)
		}
	}
	return sites
}

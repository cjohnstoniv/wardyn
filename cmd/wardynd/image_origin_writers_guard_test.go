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
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestImageOriginWardynHasOneWriter pins the provenance a revive trusts. Revive skips the image check for a run
// whose recorded origin is "wardyn" (run_revive_image.go#Server.reviveImageAdmission), and nothing else on the row
// marks a run Wardyn launched itself, so that origin must be written only by newStepRun. A new reference to the
// constant, a "wardyn" literal as an origin, or a new caller of the scoped origin writers fails here instead of
// relying on someone grepping for it. Extend a list only with a writer whose origin no request can choose.
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
			if strings.HasPrefix(rel, "internal/store/") {
				return nil // the store implements SetRunImageAdmission; its callers are what is counted
			}
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
			walk := func(n ast.Node, fn string) {
				ast.Inspect(n, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						record(x.Sel.Name, fn)
					case *ast.Ident:
						if x.Name == "ImageOriginWardyn" {
							record(x.Name, fn)
						}
					case *ast.KeyValueExpr:
						if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Origin" {
							if lit, ok := x.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
								if v, _ := strconv.Unquote(lit.Value); v == "wardyn" {
									literalOrigins = append(literalOrigins, rel+":"+fn)
								}
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
}

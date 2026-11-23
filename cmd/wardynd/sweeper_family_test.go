// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// TestEveryLeaderSweepHasAFamily: every leaderGo call in this package names its sweep with a string literal that
// sweepFamilies maps to a family in db.SweeperFamilies, and every entry of the map is a sweep that is started.
// Catches: a new leader sweep with no family, which leaderGo refuses to run.
func TestEveryLeaderSweepHasAFamily(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	started := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "leaderGo" {
				return true
			}
			pos := fset.Position(call.Pos())
			var lit *ast.BasicLit
			if len(call.Args) == 4 {
				lit, _ = call.Args[2].(*ast.BasicLit)
			}
			if lit == nil || lit.Kind != token.STRING {
				t.Errorf("%s: leaderGo's sweep name is not a string literal", pos)
				return true
			}
			sweep, _ := strconv.Unquote(lit.Value)
			started[sweep] = true
			family, ok := sweepFamily(sweep)
			if !ok {
				t.Errorf("%s: leader sweep %q has no sweep family in sweepFamilies", pos, sweep)
			} else if !slices.Contains(db.SweeperFamilies, family) {
				t.Errorf("%s: leader sweep %q maps to %q, not a family in db.SweeperFamilies", pos, sweep, family)
			}
			return true
		})
	}
	if len(started) == 0 {
		t.Fatal("found no leaderGo call")
	}
	for sweep := range sweepFamilies {
		if !started[sweep] {
			t.Errorf("sweepFamilies names %q, which no leaderGo call starts", sweep)
		}
	}
}

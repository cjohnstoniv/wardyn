// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// blockingArmOwners are the ONLY functions allowed to set SetupCheck.Blocking.
//
// TestSetupCheckBlocking asserts the value of Blocking on the rows it calls.
// This asserts something it structurally cannot: that no FOURTH arm anywhere in
// the package sets the flag at all. That gap is real — a check constructor the
// table test never calls could mark itself blocking and every existing test
// would stay green, while the console would start confiscating itself over it.
//
// Parsing beats grepping here for the same reason slogOnlyPackages parses
// imports: it names the enclosing FUNCTION, so moving an arm into a new helper
// fails this test rather than sliding past a line-oriented scan.
var blockingArmOwners = []string{"runnerCheck", "confinementFloorCheck", "ssoRBACCheck"}

func TestOnlyThreeArmsSetBlocking(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var owners []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++

		var fn string
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				fn = node.Name.Name
			case *ast.KeyValueExpr:
				key, ok := node.Key.(*ast.Ident)
				if !ok || key.Name != "Blocking" {
					return true
				}
				// A `Blocking: false` is a no-op nobody writes; record every
				// occurrence rather than only the true ones, so a future
				// computed value (Blocking: someBool) is caught too.
				owners = append(owners, fn+" ("+filepath.Base(fset.Position(node.Pos()).Filename)+")")
			}
			return true
		})
	}

	// Guard the guard: a wrong directory would scan nothing and pass.
	if scanned == 0 {
		t.Fatal("scanned no non-test .go files in internal/api")
	}

	var got []string
	for _, o := range owners {
		got = append(got, strings.Split(o, " ")[0])
	}
	slices.Sort(got)
	want := slices.Clone(blockingArmOwners)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("functions setting SetupCheck.Blocking = %v, want exactly %v\n"+
			"sites: %v\n"+
			"Blocking decides whether the console refuses to open at all; a new one is a product "+
			"decision, not an implementation detail. Add it to blockingArmOwners deliberately, and "+
			"record its arm in setupCheckBlockingStatus (setup_checks_test.go) so its value is pinned too.",
			got, want, owners)
	}
}

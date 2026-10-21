// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validateAuthoredPolicySpec refuses fields Wardyn decides itself (owner_only_refs) at every door where a
// person writes a policy. The run path uses the plain validatePolicySpec, because it validates after launch-time
// narrowing has set those fields. Each write door is a one-line call that could be swapped back to the plain
// function with no behaviour test failing, so this guard reads the source: only these files may name
// validatePolicySpec, and each exactly as often as listed. A new caller is a new door: use
// validateAuthoredPolicySpec if a person writes the spec, and add the row here with the reason if the spec is
// Wardyn's own.
var validatePolicySpecCallers = map[string]struct {
	n   int
	why string
}{
	"policy.go":                   {1, "validateAuthoredPolicySpec itself: the plain check, then the authored-only refusals"},
	"run_policy_resolution.go":    {1, "the run path, after the inline policy is bounded"},
	"run_policy_overlay.go":       {1, "the run path, after the overlay is bounded"},
	"governance_profile_write.go": {1, "the profile base check: a base that already fails is not blamed on the overlay"},
}

func TestValidatePolicySpecCallerSet(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Every use of the name counts, a call or a function value, except the declaration's own.
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name == "validatePolicySpec" && n.Recv == nil {
					if n.Body != nil {
						ast.Inspect(n.Body, func(m ast.Node) bool {
							if id, ok := m.(*ast.Ident); ok && id.Name == "validatePolicySpec" {
								got[name]++
							}
							return true
						})
					}
					return false
				}
			case *ast.Ident:
				if n.Name == "validatePolicySpec" {
					got[name]++
				}
			}
			return true
		})
	}
	for name, n := range got {
		row, ok := validatePolicySpecCallers[name]
		switch {
		case !ok:
			t.Errorf("%s uses validatePolicySpec %d time(s) and is not on the list: a door where a person writes a policy must call validateAuthoredPolicySpec", name, n)
		case n != row.n:
			t.Errorf("%s uses validatePolicySpec %d time(s), the list says %d (%s): a swapped or added call needs a decision", name, n, row.n, row.why)
		}
	}
	for name, row := range validatePolicySpecCallers {
		if got[name] == 0 {
			t.Errorf("%s is on the list (%s) but no longer calls validatePolicySpec: drop the row", name, row.why)
		}
	}
}

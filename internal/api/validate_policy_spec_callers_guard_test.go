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
// function with no behaviour test failing, and the variants (Lenient, Unresolved, Mode) skip the same refusal.
// So this guard reads the source: of every function whose name starts with validatePolicySpec, only the listed
// files may name it, each exactly as often as listed. A new caller is a new door: use validateAuthoredPolicySpec
// if a person writes the spec, and add the row here with the reason if the spec is Wardyn's own.
type specCaller struct {
	n   int
	why string
}

var validatePolicySpecCallers = map[string]map[string]specCaller{
	"validatePolicySpec": {
		"policy.go":                {1, "validateAuthoredPolicySpec itself: the plain check, then the authored-only refusals"},
		"run_policy_resolution.go": {1, "the run path, after the inline policy is bounded"},
		"run_policy_overlay.go":    {1, "the run path, after the overlay is bounded"},
		"governance_unit_write.go": {1, "the profile base check: a base that already fails is not blamed on the overlay"},
	},
	"validatePolicySpecMode": {
		"policy.go": {3, "the plain, Lenient and Unresolved wrappers"},
	},
	"validatePolicySpecLenient": {
		"profile.go": {1, "a spec built from stored values, not authored"},
	},
	"validatePolicySpecUnresolved": {
		"policy_grade.go": {1, "grading reads no file of this host"},
	},
}

func TestValidatePolicySpecCallerSet(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]int{}
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
		// Every use counts, a call or a function value, except a declaration's own name.
		decl := map[token.Pos]bool{}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				decl[fd.Name.Pos()] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "validatePolicySpec") && !decl[id.Pos()] {
				if got[id.Name] == nil {
					got[id.Name] = map[string]int{}
				}
				got[id.Name][name]++
			}
			return true
		})
	}
	for fn, byFile := range got {
		for name, n := range byFile {
			row, ok := validatePolicySpecCallers[fn][name]
			switch {
			case !ok:
				t.Errorf("%s uses %s %d time(s) and is not on the list: a door where a person writes a policy must call validateAuthoredPolicySpec", name, fn, n)
			case n != row.n:
				t.Errorf("%s uses %s %d time(s), the list says %d (%s): a swapped or added call needs a decision", name, fn, n, row.n, row.why)
			}
		}
	}
	for fn, byFile := range validatePolicySpecCallers {
		for name, row := range byFile {
			if got[fn][name] == 0 {
				t.Errorf("%s is on the list for %s (%s) but no longer uses it: drop the row", name, fn, row.why)
			}
		}
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The census guard for deny-f2. A door written later that refuses from the
// ceiling must name the policy, and a list of today's doors cannot promise that:
// this scan can. It fails when
//
//   - authz.Deny is called with ReasonGovernanceProfile or ReasonRunQuota and the
//     decision never goes through .WithPolicy(...), or
//   - reasonRecordCeilingLimit reaches writeErrorReason instead of
//     writeErrorReasonPolicy (that door bypasses refuse, so WithPolicy cannot
//     reach it).
//
// It reads syntax only, so it cannot see a decision built into a variable and
// wrapped elsewhere; a door that wants that shape has to be added here by hand,
// which is the point of making it fail first.

// policyCensusViolations lists, as "file:line: what", every ceiling refusal in f
// that would ship without its policy.
func policyCensusViolations(fset *token.FileSet, name string, f *ast.File) []string {
	// Every call on the receiver chain of a .WithPolicy(...) is wrapped.
	wrapped := map[*ast.CallExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithPolicy" {
			for x := sel.X; ; {
				c, ok := x.(*ast.CallExpr)
				if !ok {
					break
				}
				wrapped[c] = true
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					break
				}
				x = s.X
			}
		}
		return true
	})

	var out []string
	at := func(n ast.Node, what string) {
		out = append(out, fmt.Sprintf("%s:%d: %s", name, fset.Position(n.Pos()).Line, what))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case isQualified(call.Fun, "authz", "Deny") && len(call.Args) > 0 && !wrapped[call] &&
			(isQualified(call.Args[0], "authz", "ReasonGovernanceProfile") || isQualified(call.Args[0], "authz", "ReasonRunQuota")):
			at(call, "a governance_profile or run_quota refusal that never reaches .WithPolicy(...)")
		case isBare(call.Fun, "writeErrorReason") && callMentions(call, "reasonRecordCeilingLimit"):
			at(call, "reasonRecordCeilingLimit written by writeErrorReason, not writeErrorReasonPolicy")
		}
		return true
	})
	return out
}

func isQualified(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}

func isBare(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func callMentions(call *ast.CallExpr, ident string) bool {
	found := false
	for _, a := range call.Args {
		ast.Inspect(a, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == ident {
				found = true
			}
			return !found
		})
	}
	return found
}

func TestEveryCeilingRefusalNamesThePolicy(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, v := range policyCensusViolations(fset, name, f) {
			t.Error(v)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files")
	}
}

// TestPolicyCensusGuardFiresOnAPlantedDoor shows the guard is not satisfied by
// being silent: each planted source is one a door could be written as, and each
// must be reported.
func TestPolicyCensusGuardFiresOnAPlantedDoor(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{"an unwrapped governance_profile refusal", `package api
func f(s *Server) { s.refuse(w, r, authz.Deny(authz.ReasonGovernanceProfile, "runs.x", "no")) }`, 1},
		{"an unwrapped run_quota refusal", `package api
func f(s *Server) { s.refuse(w, r, authz.Deny(authz.ReasonRunQuota, "runs.quota", "no").OnRun(id)) }`, 1},
		{"reasonRecordCeilingLimit through writeErrorReason", `package api
func f() { writeErrorReason(w, http.StatusForbidden, reasonRecordCeilingLimit, "no") }`, 1},
		{"a wrapped governance_profile refusal passes", `package api
func f(s *Server) { s.refuse(w, r, authz.Deny(authz.ReasonGovernanceProfile, "runs.x", "no").OnRun(id).WithPolicy(p)) }`, 0},
		{"reasonRecordCeilingLimit through writeErrorReasonPolicy passes", `package api
func f() { writeErrorReasonPolicy(w, http.StatusForbidden, reasonRecordCeilingLimit, "no", ref) }`, 0},
		{"an unrelated reason is not a ceiling refusal", `package api
func f(s *Server) { s.refuse(w, r, authz.Deny(authz.ReasonNotOwner, "run", "no")) }`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "planted.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := policyCensusViolations(fset, "planted.go", f); len(got) != tc.want {
				t.Errorf("violations = %v, want %d", got, tc.want)
			}
		})
	}
}

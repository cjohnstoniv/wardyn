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
//   - authz.Deny is called with ReasonGovernanceProfile, ReasonRunQuota or
//     ReasonComponentAutonomy — or with the autonomy ladder's reason, `<x>.reason`
//     for an x the function declares as an autonomySource, which is one of the
//     first and the third by construction — and the decision never goes
//     through .WithPolicy(...), or
//   - reasonRecordCeilingLimit reaches writeErrorReason instead of
//     writeErrorReasonPolicy (that door bypasses refuse, so WithPolicy cannot
//     reach it).
//
// It reads syntax only, so it cannot see a decision built into a variable and
// wrapped elsewhere; a door that wants that shape has to be added here by hand,
// which is the point of making it fail first.

// ladderReasons returns every `<x>.reason` expression in f whose x the enclosing
// function declares as an autonomySource (a parameter or a var): the reason the
// autonomy ladder refuses with. Syntax only, like the rest of this guard.
func ladderReasons(f *ast.File) map[ast.Expr]bool {
	out := map[ast.Expr]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		names := map[string]bool{}
		declare := func(typ ast.Expr, idents []*ast.Ident) {
			if isBare(typ, "autonomySource") {
				for _, id := range idents {
					names[id.Name] = true
				}
			}
		}
		for _, p := range fn.Type.Params.List {
			declare(p.Type, p.Names)
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if vs, ok := n.(*ast.ValueSpec); ok && vs.Type != nil {
				declare(vs.Type, vs.Names)
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "reason" {
				if id, ok := sel.X.(*ast.Ident); ok && names[id.Name] {
					out[sel] = true
				}
			}
			return true
		})
	}
	return out
}

// ceilingRefusals is every authz.Deny in f whose reason is a ceiling refusal's.
func ceilingRefusals(f *ast.File) []*ast.CallExpr {
	ladder := ladderReasons(f)
	var out []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isQualified(call.Fun, "authz", "Deny") || len(call.Args) == 0 {
			return true
		}
		reason := call.Args[0]
		if isQualified(reason, "authz", "ReasonGovernanceProfile") || isQualified(reason, "authz", "ReasonRunQuota") ||
			isQualified(reason, "authz", "ReasonComponentAutonomy") || ladder[reason] {
			out = append(out, call)
		}
		return true
	})
	return out
}

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
	for _, call := range ceilingRefusals(f) {
		if !wrapped[call] {
			at(call, "a governance_profile, run_quota or component_autonomy refusal that never reaches .WithPolicy(...)")
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isBare(call.Fun, "writeErrorReason") && callMentions(call, "reasonRecordCeilingLimit") {
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
	scanned, ladder := 0, 0
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
		if name == "runs_autonomy.go" {
			ladder = len(ceilingRefusals(f))
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files")
	}
	// The autonomy ladder refuses through autonomySource (governance_profile or
	// component_autonomy): five doors. Fewer seen means the guard went blind
	// to them, not that they went away.
	if ladder < 5 {
		t.Errorf("the census sees %d ceiling refusals in runs_autonomy.go, want the ladder's 5", ladder)
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
		{"an unwrapped component_autonomy refusal", `package api
func f(s *Server) { s.refuse(w, r, authz.Deny(authz.ReasonComponentAutonomy, "runs.x", "no")) }`, 1},
		{"an unwrapped ladder refusal through an autonomySource parameter", `package api
func f(s *Server, src autonomySource) { s.refuse(w, r, authz.Deny(src.reason, "runs.x", "no")) }`, 1},
		{"an unwrapped ladder refusal through an autonomySource var", `package api
func f(s *Server) { var src autonomySource; s.refuse(w, r, authz.Deny(src.reason, "runs.x", "no")) }`, 1},
		{"a wrapped ladder refusal passes", `package api
func f(s *Server, src autonomySource) { s.refuse(w, r, authz.Deny(src.reason, "runs.x", "no").WithPolicy(src.policy)) }`, 0},
		{"another type's reason field is not the ladder's", `package api
func f(s *Server, k capKind) { s.refuse(w, r, authz.Deny(k.reason, "runs.x", "no")) }`, 0},
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

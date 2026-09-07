// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestWorkspaceOwnershipPinsBothDirections is F315.
//
// The authz matrix DELEGATES the {id}-bearing workspace routes: TestAuthzMatrix's
// classMember probe fires a random uuid, which 404s at the store before
// authorization runs, so the matrix cannot see who is admitted and requirement
// 1's non-vacuity clause rests entirely on a compensating pin per route. Those
// pins live in workspace_owner_test.go as two hand-written case lists — the
// foreign row must 404 byte-identically, the OWN row must be reachable — and
// nothing held the two lists to each other.
//
// So one of them grew and the other did not. GET /workspaces/{id}/env-as-code is
// in the foreign-404 list and absent from TestWorkspaceOwnership_OwnerReachesOwn,
// which means the ADMIT direction was unpinned: the reviewer patched the handler
// to 403 every non-super-admin on their OWN workspace and all 221 test files in
// the package stayed green (executed, 44.9s).
//
// The one-line fix is to add the route to the second list. The DEFECT is that
// nothing would have noticed, and would not notice the next route either — the
// lists are function-local literals that can only drift silently. This reads
// both from source and holds them to each other, so a route with only half its
// delegation honoured fails instead of waiting for a reviewer.
//
// ownerAdmitNotInTheControl records every foreign-pinned route that is NOT in
// the positive control, with the reason. Some are pinned by another test; some
// are admitted debt. Either way the entry is a decision rather than an omission,
// and the anti-rot arm below deletes it for you when it stops being true.
var ownerAdmitNotInTheControl = map[string]string{
	// PINNED ELSEWHERE. R1 F287 moved this route off the member tier to
	// owner-or-super, so it is no longer a sibling of the three member reads in
	// OwnerReachesOwn. Its admit direction is pinned by
	// TestEnvAsCodeWithholdsTheArtifactRegistryFromNonFullReaders — "the
	// workspace's own member owner still gets it" — added under R1 F288, which
	// asserts the owning member reaches it AND receives the artifact-registry
	// content that decides the tier. Adding {GET, "/env-as-code"} to
	// OwnerReachesOwn as well is filed and would pass; when it lands, delete
	// this entry.
	"GET /env-as-code": "pinned by TestEnvAsCodeWithholdsTheArtifactRegistryFromNonFullReaders (the owning-member arm, R1 F288)",

	// ADMITTED DEBT, and it is the same class of hole F315 names — recorded
	// here because the completeness check surfaced it, not because it is fixed.
	// OwnerReachesOwn is a READ-ONLY positive control by construction: it walks
	// one fixture workspace, so a PUT would rename it and a DELETE would remove
	// it under the cases that follow. The {id} MUTATIONS therefore have their
	// foreign-404 direction pinned and their owner-admit direction pinned
	// nowhere — TestWorkspaceOwnership_MemberLocalDirGate and
	// _MemberWritableAllowlist exercise POST /workspaces (create), not these.
	// A handler that refused the owning member on PUT /workspaces/{id} would
	// leave the package green, exactly as F315's counterfactual did for
	// env-as-code. Closing it wants a per-case fixture in that file, which is
	// another lane's; filed.
	"PUT ":        "admitted debt (R1 F315): the positive control is read-only, so no test admits the owner on PUT /workspaces/{id}",
	"DELETE ":     "admitted debt (R1 F315): as above, for DELETE /workspaces/{id}",
	"POST /scan":  "admitted debt (R1 F315): as above, and a scan has side effects the read-only control avoids",
	"POST /build": "admitted debt (R1 F315): as above, for the build trigger",
}

func TestWorkspaceOwnershipPinsBothDirections(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "workspace_owner_test.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse workspace_owner_test.go: %v", err)
	}
	foreign := ownershipCaseRoutes(t, f, "TestWorkspaceOwnership_ForeignOwned404Parity")
	admitted := ownershipCaseRoutes(t, f, "TestWorkspaceOwnership_OwnerReachesOwn")

	for _, route := range slices.Sorted(maps.Keys(foreign)) {
		if admitted[route] {
			continue
		}
		if why := ownerAdmitNotInTheControl[route]; why != "" {
			t.Logf("%s: not in the control — %s", route, why)
			continue
		}
		t.Errorf("%s is pinned in the FOREIGN direction (byte-identical 404) and in neither admit list. The "+
			"matrix delegates this route — its classMember probe 404s at the store before authorization runs — "+
			"so a handler that refuses the OWNING member leaves the whole package green. Add it to "+
			"TestWorkspaceOwnership_OwnerReachesOwn, or record the reason in "+
			"ownerAdmitNotInTheControl", route)
	}

	// The reverse is a defect too, and a subtler one: a route admitted for its
	// owner with no foreign-404 pin is an existence oracle nobody is watching.
	for _, route := range slices.Sorted(maps.Keys(admitted)) {
		if !foreign[route] {
			t.Errorf("%s pins the admit direction and not the foreign one — a member can then probe another "+
				"member's workspace ids by the difference between 404 and anything else", route)
		}
	}

	// …and the exception list must not rot.
	for route, why := range ownerAdmitNotInTheControl {
		if !foreign[route] {
			t.Errorf("ownerAdmitNotInTheControl names %q, which the foreign-404 list no longer covers — drop "+
				"the entry (it said: %s)", route, why)
		}
		if admitted[route] {
			t.Errorf("ownerAdmitNotInTheControl names %q, which TestWorkspaceOwnership_OwnerReachesOwn now "+
				"covers directly — drop the entry", route)
		}
	}
}

// ownershipCaseRoutes reads the "METHOD suffix" pairs out of the table literal
// in the named test. Both tables are anonymous structs whose first two fields
// are the method and the URL suffix; the foreign one carries a third (body), and
// the SAME route appearing twice with different bodies is one route here.
func ownershipCaseRoutes(t *testing.T, f *ast.File, fn string) map[string]bool {
	t.Helper()
	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == fn {
			decl = d
			break
		}
	}
	if decl == nil {
		t.Fatalf("workspace_owner_test.go has no %s — this guard reads both case lists from source; "+
			"re-point it at the new shape rather than deleting it", fn)
	}
	out := map[string]bool{}
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || len(lit.Elts) < 2 {
			return true
		}
		method, ok := ownershipLitString(lit.Elts[0])
		if !ok || !strings.HasPrefix(method, "http.Method") {
			return true
		}
		suffix, ok := ownershipLitString(lit.Elts[1])
		if !ok {
			return true
		}
		out[strings.ToUpper(strings.TrimPrefix(method, "http.Method"))+" "+suffix] = true
		return true
	})
	if len(out) == 0 {
		t.Fatalf("%s: no {method, suffix} cases found — re-point this guard rather than deleting it", fn)
	}
	return out
}

// ownershipLitString renders a case field that is either a string literal (the
// suffix) or a selector such as http.MethodGet (the method).
func ownershipLitString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			return strings.Trim(v.Value, `"`), true
		}
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok {
			return pkg.Name + "." + v.Sel.Name, true
		}
	}
	return "", false
}

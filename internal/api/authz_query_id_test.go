// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The QUERY-PARAMETER id row class of TestAuthzMatrix.
//
// routeMatrix classifies a route by its PATH, and classOwner seeds an owned and
// a foreign id into the path's {id}. An id that arrives in the QUERY string
// never reaches that probe: `GET /approvals` is classMember, so the matrix
// admitted a member and stopped — and `?run_id=<someone else's run>` (the
// member IDOR the approvals handler closes with getRunAuthorized) ran under no
// test at all. queryIDMatrix is that missing row class, and
// TestQueryParamIDsAreClassified is what makes it exhaustive: every query
// parameter any handler in this package reads is either a named id row here or
// a named non-id in queryParamNotAnID, so the next `?run_id=`/`?owner=` reader
// fails the build until someone states what a member naming a FOREIGN id gets.

// Entities a query-param id can name beyond classOwner's run/approval/workspace.
const entityPrincipal routeEntity = "principal"

type queryIDRoute struct {
	// entity is what the parameter names: entityRun (seeded owned + foreign
	// runs, each carrying a PENDING approval) or entityPrincipal (the member's
	// own subject vs another member's).
	entity routeEntity
	// readers is every function in this package that reads the parameter on
	// this route's path — the (function, parameter) pairs
	// TestQueryParamIDsAreClassified finds in source.
	readers []string
	// foreign is the status a MEMBER naming a FOREIGN id must get. The body
	// must also carry none of the foreign entity's ids. 0 means the probe
	// cannot run against the matrix's doubles; pinnedBy then carries it.
	foreign int
	// ownAdmitted: a member naming their OWN id reaches the handler (neither
	// 401, 403 nor 404). False where naming the parameter at all is the
	// refusal (?owner= is admin-only, whatever it names).
	ownAdmitted bool
	// pinnedBy names the dedicated test that proves the scoping in depth. It
	// must exist; it is required when foreign is 0.
	pinnedBy string
}

// queryIDMatrix is keyed "METHOD /pattern?param"; METHOD /pattern must be a
// routeMatrix key unless the row is pinnedBy-only (the UI gateway enter lane is
// served beside the chi router, not on it).
var queryIDMatrix = map[string]queryIDRoute{
	// The F4 row: never run for a member before this class existed.
	"GET /api/v1/approvals?run_id": {
		entity: entityRun, readers: []string{"handleListApprovals"},
		foreign: 404, ownAdmitted: true, pinnedBy: "TestMemberApprovalListByRun",
	},
	// A collection endpoint: a foreign run collapses to the same empty 200 an
	// unknown one gets (auditScope's no-existence-oracle rule).
	"GET /api/v1/audit?run_id": {
		entity: entityRun, readers: []string{"auditScope"},
		foreign: 200, ownAdmitted: true, pinnedBy: "TestAuditMemberScope_QueryAndExportAgree",
	},
	// The export needs a paging store; the matrix double is not one (501 to
	// every caller before the scope runs), so the dedicated test carries it.
	"GET /api/v1/audit/export?run_id": {
		entity: entityRun, readers: []string{"auditScope"},
		pinnedBy: "TestAuditMemberScope_QueryAndExportAgree",
	},
	// ?owner= is admin-only: a member naming it at all gets a constant 403.
	"GET /api/v1/secrets?owner": {
		entity: entityPrincipal, readers: []string{"secretOwnerParam", "handleListSecrets"},
		foreign: 403, pinnedBy: "TestListSecrets_AdminOwnerParam_Member403",
	},
	// A PUT refuses ?owner= for an admin too (K7-A), so the probe's "an admin
	// is admitted" arm cannot hold; the dedicated test carries both refusals.
	"PUT /api/v1/secrets/{name}?owner": {
		entity: entityPrincipal, readers: []string{"handlePutSecret"},
		pinnedBy: "TestPutSecret_OwnerParamIsRefused",
	},
	"DELETE /api/v1/secrets/{name}?owner": {
		entity: entityPrincipal, readers: []string{"secretOwnerParam", "handleDeleteSecret"},
		foreign: 403, pinnedBy: "TestListSecrets_AdminOwnerParam_Member403",
	},
	// Internal (run token): the named approval is looked up among the CALLING
	// run's approvals only, so another run's id is a mismatch, never a spend.
	"GET /api/v1/internal/injection/{grantID}?approval": {
		entity: entityApproval, readers: []string{"answerADOCapability"},
		pinnedBy: "TestADOCapability_AnotherRunsApprovalIsRefused",
	},
	"GET /__wardyn/enter?run": {
		entity: entityRun, readers: []string{"handleUIEnter"},
		pinnedBy: "TestUIGateway_EnterRejectsNonOwnerTicket",
	},
}

// queryParamNotAnID names every query parameter this package reads that does
// NOT select another principal's entity, with why. Keyed by parameter name: a
// non-id stays a non-id wherever it is read. An id-shaped name read by a new
// function is NOT covered here — it needs its own queryIDMatrix row.
var queryParamNotAnID = map[string]string{
	"limit":                     "page window (parseListPage)",
	"offset":                    "page window (parseListPage)",
	"state":                     "approval state filter on an already-scoped listing; the ADO callback's signed OAuth state",
	"since":                     "audit time filter; narrows an already-scoped feed",
	"until":                     "audit time filter; narrows an already-scoped feed",
	"action":                    "audit action filter; narrows an already-scoped feed",
	"action_prefix":             "audit action filter; narrows an already-scoped feed",
	"actor":                     "audit principal filter; ANDed inside auditScope, so a member's own-run feed only narrows",
	"actor_type":                "audit filter; narrows an already-scoped feed",
	"outcome":                   "audit filter; narrows an already-scoped feed",
	"origin":                    "audit filter, enum device|organisation (parseAuditFilter 400s anything else); narrows an already-scoped feed",
	"force":                     "operator confirmation flag",
	"confirm":                   "operator confirmation flag",
	"acknowledge_access_change": "operator confirmation flag",
	"include":                   "response projection switch",
	"preset":                    "response projection switch",
	"rows":                      "terminal geometry",
	"cols":                      "terminal geometry",
	"type":                      "directory search kind (security tier route)",
	"q":                         "directory search text (security tier route)",
	"ticket":                    "single-use attach ticket: the credential itself, redeemed against the run it was minted for",
	"app":                       "UI gateway app name, checked against the run's effective policy",
	"code":                      "OAuth authorization code, bound to the caller's signed state",
	"error":                     "OAuth error echo",
	"error_description":         "OAuth error echo",
	"phase":                     "ADO sign-in phase marker",
	"scopes":                    "ADO sign-in: requested scopes, clamped to the admin's ceiling",
	"capabilities":              "ADO sign-in: requested capabilities, clamped to the admin's ceiling",
	"recheck":                   "setup status re-probe switch, honoured for operators only",
	"capability":                "internal route: the run token is the scope; the value is a capability name",
	"first_use":                 "internal route: the run token is the scope; the value is a mode",
	"method":                    "internal route: the proxy describing the agent's own request, under the run token",
	"path":                      "internal route: the proxy describing the agent's own request, under the run token",
	"repo":                      "internal route: the proxy describing the agent's own request, under the run token",
	"ref_class":                 "internal route: the proxy describing the agent's own request, under the run token",
}

// queryParamDynamicReads names the parameters a function reads through a
// non-constant expression (parseAuditFilter loops over {since, until}). The
// guard records these names for that function instead of failing on the read.
var queryParamDynamicReads = map[string][]string{
	"parseAuditFilter": {"since", "until"},
}

// queryParamReads returns every (function, parameter) pair in this package's
// non-test source where a handler reads a query parameter: X.Get/Has("p") or
// X["p"] where X is `<expr>.URL.Query()`, a local assigned from one, or a
// url.Values parameter. A parameter spelled by a constant resolves through the
// package's string constants; one that cannot be resolved is reported, so the
// guard fails closed rather than silently skipping a read.
func queryParamReads(t *testing.T) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	consts := map[string]string{}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files = append(files, f)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[id.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	isQueryCall := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Query" {
			return false
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		return ok && inner.Sel.Name == "URL"
	}
	out := map[string]map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			vals := map[string]bool{}
			for _, fl := range fd.Type.Params.List {
				if sel, ok := fl.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Values" {
					for _, n := range fl.Names {
						vals[n.Name] = true
					}
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
					for i, rhs := range as.Rhs {
						if id, ok := as.Lhs[i].(*ast.Ident); ok && isQueryCall(rhs) {
							vals[id.Name] = true
						}
					}
				}
				return true
			})
			isValues := func(e ast.Expr) bool {
				if id, ok := e.(*ast.Ident); ok {
					return vals[id.Name]
				}
				return isQueryCall(e)
			}
			record := func(arg ast.Expr, pos token.Pos) {
				var p string
				switch a := arg.(type) {
				case *ast.BasicLit:
					p, _ = strconv.Unquote(a.Value)
				case *ast.Ident:
					p = consts[a.Name]
				}
				if p == "" && queryParamDynamicReads[fd.Name.Name] != nil {
					for _, d := range queryParamDynamicReads[fd.Name.Name] {
						if out[d] == nil {
							out[d] = map[string]bool{}
						}
						out[d][fd.Name.Name] = true
					}
					return
				}
				if p == "" {
					t.Errorf("%s: %s reads a query parameter this guard cannot resolve to a name — "+
						"spell it as a literal or a package string constant", fset.Position(pos), fd.Name.Name)
					return
				}
				if out[p] == nil {
					out[p] = map[string]bool{}
				}
				out[p][fd.Name.Name] = true
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if ok && (sel.Sel.Name == "Get" || sel.Sel.Name == "Has") && len(x.Args) == 1 && isValues(sel.X) {
						record(x.Args[0], x.Pos())
					}
				case *ast.IndexExpr:
					if isValues(x.X) {
						record(x.Index, x.Pos())
					}
				}
				return true
			})
		}
	}
	return out
}

func TestQueryParamIDsAreClassified(t *testing.T) {
	reads := queryParamReads(t)
	if len(reads["run_id"]) == 0 || len(reads["owner"]) == 0 {
		t.Fatalf("the source walk found no run_id/owner reads (%v) — the guard is blind, not the package clean", reads)
	}

	idReaders := map[string]map[string]bool{} // param -> readers stated by queryIDMatrix
	for key, row := range queryIDMatrix {
		_, param, ok := strings.Cut(key, "?")
		if !ok || len(row.readers) == 0 {
			t.Errorf("queryIDMatrix %q: key must be \"METHOD /pattern?param\" and readers non-empty", key)
			continue
		}
		if queryParamNotAnID[param] != "" {
			t.Errorf("%q is both an id row and in queryParamNotAnID — pick one", param)
		}
		if idReaders[param] == nil {
			idReaders[param] = map[string]bool{}
		}
		for _, fn := range row.readers {
			idReaders[param][fn] = true
			if !reads[param][fn] {
				t.Errorf("STALE queryIDMatrix %q: %s no longer reads ?%s= — remove it from readers", key, fn, param)
			}
		}
	}

	for _, param := range slices.Sorted(maps.Keys(reads)) {
		if queryParamNotAnID[param] != "" {
			continue
		}
		for _, fn := range slices.Sorted(maps.Keys(reads[param])) {
			if !idReaders[param][fn] {
				t.Errorf("UNCLASSIFIED query parameter: %s reads ?%s=. If it names another principal's run, "+
					"approval, workspace or namespace, add a queryIDMatrix row stating what a member naming a "+
					"FOREIGN one gets; otherwise add it to queryParamNotAnID with the reason", fn, param)
			}
		}
	}
	for _, param := range slices.Sorted(maps.Keys(queryParamNotAnID)) {
		if len(reads[param]) == 0 {
			t.Errorf("STALE queryParamNotAnID %q: nothing in the package reads it any more — remove it", param)
		}
	}

	// Every pinnedBy names a real test, and a delegated row cannot be empty.
	testFuncs := map[string]bool{}
	fset := token.NewFileSet()
	tests, _ := filepath.Glob("*_test.go")
	for _, n := range tests {
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, n, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				testFuncs[fd.Name.Name] = true
			}
		}
	}
	for key, row := range queryIDMatrix {
		if row.pinnedBy != "" && !testFuncs[row.pinnedBy] {
			t.Errorf("queryIDMatrix %q: pinnedBy %s does not exist in this package", key, row.pinnedBy)
		}
		if row.foreign == 0 && row.pinnedBy == "" {
			t.Errorf("queryIDMatrix %q is probed nowhere: set foreign, or name the test in pinnedBy", key)
		}
		route, _, _ := strings.Cut(key, "?")
		if _, ok := routeMatrix[route]; row.foreign != 0 && !ok {
			t.Errorf("queryIDMatrix %q is probed but %s is not a routeMatrix route", key, route)
		}
	}
}

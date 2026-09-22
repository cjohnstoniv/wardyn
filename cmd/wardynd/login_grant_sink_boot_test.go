// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// run() wires TWO things that, together, make Azure DevOps access-token
// capture happen at login time: the api.Config passed to api.New must set
// ADOEntra from adoEntraSourceFromFlags(...), and attachLoginGrantSink(...)
// must be called on the resulting server AFTER srv exists and BEFORE
// startBackgroundWorkers(...) — the doc comment at the call site says why the
// ordering matters ("the login-grant edge, joined after both sides exist and
// before anything is served, because they form a cycle").
//
// run() itself is un-unit-testable (it parses flags, dials Postgres, opens a
// listener); nothing exercises it in CI. So this pins the WIRING mechanically,
// the way outbound_dial_guard_test.go pins the corp-proxy dial sites: parse
// main.go with go/ast, not a regexp, and fail if either boot-time seam is
// missing, moved to the wrong side of api.New/startBackgroundWorkers, or if
// the ADOEntra field is dropped from api.Config.
//
// The BEHAVIOUR this wiring produces — a console login widening its
// authorization request and storing an Azure DevOps access token — is pinned
// separately by TestConsoleLoginCapturesAzureDevOpsAccess
// (internal/api/ado_entra_login_test.go). That test cannot see whether
// wardynd's own boot actually calls the functions it exercises; this one
// cannot see whether the functions behave correctly. Both are required.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// findRunFunc parses cmd/wardynd/main.go and returns the "run" FuncDecl.
func findRunFunc(t *testing.T, root string) (*ast.File, *ast.FuncDecl, *token.FileSet) {
	t.Helper()
	path := filepath.Join(root, "cmd", "wardynd", "main.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "run" || fd.Recv != nil {
			continue
		}
		return f, fd, fset
	}
	t.Fatal("cmd/wardynd/main.go: func run() not found — the guard's target moved or was renamed")
	return nil, nil, nil
}

// callName reports the dotted-or-bare name of a call's callee, e.g. "api.New"
// or "attachLoginGrantSink", or "" for anything else.
func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok {
			return id.Name + "." + fn.Sel.Name
		}
	}
	return ""
}

// adoEntraFieldWiredFromFlags reports whether call (expected to be api.New's
// single api.Config{...} argument) sets the ADOEntra field to a call of
// adoEntraSourceFromFlags(...).
func adoEntraFieldWiredFromFlags(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	lit, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "ADOEntra" {
			continue
		}
		valCall, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return false
		}
		return callName(valCall) == "adoEntraSourceFromFlags"
	}
	return false
}

// TestBootWiresADOEntraSource pins that api.Config.ADOEntra is set from
// adoEntraSourceFromFlags(...) inside run() — the flag/store-backed Azure
// DevOps Entra row is reachable by the login-grant sink at all. Without this,
// attachLoginGrantSink is wired to an always-nil source and every login is
// silently the no-Azure-DevOps-row case (see
// TestLoginScopesAreEmptyWithoutAConfiguredRow in
// internal/api/ado_entra_login_test.go for what that no-op looks like).
func TestBootWiresADOEntraSource(t *testing.T) {
	root := repoRoot(t)
	_, run, _ := findRunFunc(t, root)

	found := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || callName(call) != "api.New" {
			return true
		}
		found = true
		if !adoEntraFieldWiredFromFlags(call) {
			t.Error("cmd/wardynd/main.go: the api.Config{} passed to api.New(...) does not set " +
				"ADOEntra: adoEntraSourceFromFlags(...) — the login-grant sink would be attached to " +
				"an always-nil source and no login would ever capture an Azure DevOps access token")
		}
		return true
	})
	if !found {
		t.Fatal(`cmd/wardynd/main.go: run() does not call api.New(...) — the guard's anchor moved`)
	}
}

// TestBootAttachesLoginGrantSink pins that attachLoginGrantSink(...) is
// called inside run(), after srv exists (api.New(...)) and before
// startBackgroundWorkers(...) starts periodic goroutines. Deleting the call,
// or moving it to the wrong side of either anchor, would silently turn off
// login-time Azure DevOps access-token capture for every deployment with a
// configured Entra row — with no error, because attachLoginGrantSink is a
// no-op-safe call and nothing else depends on its having run.
func TestBootAttachesLoginGrantSink(t *testing.T) {
	root := repoRoot(t)
	_, run, fset := findRunFunc(t, root)

	var apiNewLine, attachLine, startWorkersLine int
	ast.Inspect(run.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		line := fset.Position(call.Pos()).Line
		switch callName(call) {
		case "api.New":
			if apiNewLine == 0 {
				apiNewLine = line
			}
		case "attachLoginGrantSink":
			if attachLine == 0 {
				attachLine = line
			}
		case "startBackgroundWorkers":
			if startWorkersLine == 0 {
				startWorkersLine = line
			}
		}
		return true
	})

	if apiNewLine == 0 {
		t.Fatal("cmd/wardynd/main.go: run() does not call api.New(...) — the guard's anchor moved")
	}
	if startWorkersLine == 0 {
		t.Fatal("cmd/wardynd/main.go: run() does not call startBackgroundWorkers(...) — the guard's anchor moved")
	}
	if attachLine == 0 {
		t.Fatal("cmd/wardynd/main.go: run() no longer calls attachLoginGrantSink(...) — login-time " +
			"Azure DevOps access-token capture is wired only through this call " +
			"(behaviour pinned separately by TestConsoleLoginCapturesAzureDevOpsAccess in " +
			"internal/api/ado_entra_login_test.go, which cannot see whether wardynd's own boot " +
			"actually calls it)")
	}
	if attachLine < apiNewLine {
		t.Errorf("cmd/wardynd/main.go:%d attachLoginGrantSink(...) is called before api.New(...) "+
			"(line %d) — it needs the *api.Server api.New returns", attachLine, apiNewLine)
	}
	if attachLine > startWorkersLine {
		t.Errorf("cmd/wardynd/main.go:%d attachLoginGrantSink(...) is called after "+
			"startBackgroundWorkers(...) (line %d) — background goroutines (and the server) would "+
			"start serving before the login-grant sink is attached", attachLine, startWorkersLine)
	}
}

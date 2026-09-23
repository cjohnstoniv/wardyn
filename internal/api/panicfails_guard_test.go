// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestHandlerDrivesGoThroughPanicFails is #511/F3's ratchet on panicFails' own
// claim ("no test path in the package can read a recovered panic back as a
// passing test", api_test.go). Two call sites — demo_video_base_url_test.go's
// demoVideoServedCSP and devices_test.go's doPeer — called srv.Handler().
// ServeHTTP directly, bypassing panicFails, and nothing caught it because
// nothing asserted the "every call site wraps" claim mechanically. This does.
//
// AST-based, not a text grep: panicFails' own doc comment QUOTES the banned
// shapes in prose ("Every srv.Handler().ServeHTTP / httptest.NewServer(...)
// call site in the package wraps its handler with this"), which a raw byte
// scan would flag as a violation of itself. Walking CallExpr nodes only sees
// code, never comments or string literals — the same reasoning
// TestCommentsCiteSymbolsNotLineNumbers (cmd/wardynd/citation_guard_test.go)
// uses for the opposite direction.
func TestHandlerDrivesGoThroughPanicFails(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	filesScanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		filesScanned++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if bareHandlerServeHTTP(call) || bareNewServerHandler(call) {
				pos := fset.Position(call.Pos())
				t.Errorf("%s:%d: a bare ServeHTTP drive against a Server's handler bypasses panicFails — a "+
					"recovered panic here answers with an ordinary 500 that every assertion still matches "+
					"(#338/#511). Wrap it: panicFails(t, srv.Handler()).ServeHTTP(w, r), or "+
					"httptest.NewServer(panicFails(t, srv.Handler()))", name, pos.Line)
			}
			return true
		})
	}
	if filesScanned == 0 {
		t.Fatal("scanned 0 _test.go files — this guard would pass vacuously")
	}
}

// bareHandlerServeHTTP matches "<x>.Handler().ServeHTTP(...)" and
// "<x>.router.ServeHTTP(...)" — the two unwrapped shapes panicFails' comment
// names.
func bareHandlerServeHTTP(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ServeHTTP" {
		return false
	}
	switch recv := sel.X.(type) {
	case *ast.CallExpr:
		inner, ok := recv.Fun.(*ast.SelectorExpr)
		return ok && inner.Sel.Name == "Handler"
	case *ast.SelectorExpr:
		return recv.Sel.Name == "router"
	}
	return false
}

// bareNewServerHandler matches "httptest.NewServer(<x>.Handler())" — a
// handler served for the life of the test with no panic catcher on any
// request it answers.
func bareNewServerHandler(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewServer" {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "httptest" {
		return false
	}
	if len(call.Args) != 1 {
		return false
	}
	arg, ok := call.Args[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	inner, ok := arg.Fun.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "Handler"
}

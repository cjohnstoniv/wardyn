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
// scan would flag as a violation of itself. Walking CallExpr/CompositeLit
// nodes only sees code, never comments or string literals — the same
// reasoning TestCommentsCiteSymbolsNotLineNumbers
// (cmd/wardynd/citation_guard_test.go) uses for the opposite direction.
//
// The first round only matched the two exact bugs found (Handler().ServeHTTP,
// httptest.NewServer(x.Handler())) — a narrow enough net that a THIRD shape
// (a local var holding the raw handler, or the constructor spelled with
// NewUnstartedServer/NewTLSServer, or an &http.Server{Handler: ...} literal)
// would have bypassed the ratchet exactly as the original two bypassed
// panicFails. So the guard now tracks, PER FUNCTION, every identifier assigned
// from a raw-handler expression (see taintedIdents) and treats that identifier
// as equivalent to writing the raw expression again at every unsafe sink
// (isHandlerIdent) — not just the direct syntactic forms.
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
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			checkFuncForBareHandlerDrives(t, fset, name, fn)
			return true
		})
	}
	if filesScanned == 0 {
		t.Fatal("scanned 0 _test.go files — this guard would pass vacuously")
	}
}

// checkFuncForBareHandlerDrives reports every unsafe use of a raw Server
// handler inside one function body: a direct expression (srv.Handler(),
// srv.routes(), srv.router — see isHandlerExpr) or a local identifier that
// expression was assigned to earlier in the SAME function (taintedIdents),
// used at any of the sinks a request can be driven, or a listener started,
// through without panicFails: .ServeHTTP(...), httptest.NewServer /
// NewUnstartedServer / NewTLSServer(...), or the Handler field of an
// http.Server{} composite literal.
//
// Taint tracking is whole-function and flow-INSENSITIVE (an identifier once
// assigned from a raw handler anywhere in the function is tainted throughout
// it, regardless of order or of which branch), which is deliberately the
// generous direction for a ratchet: it can only flag a real assignment that
// exists in the function, never miss an unsafe use because of an if/else or a
// loop the walk did not model.
func checkFuncForBareHandlerDrives(t *testing.T, fset *token.FileSet, file string, fn *ast.FuncDecl) {
	t.Helper()
	tainted := taintedIdents(fn.Body)
	report := func(pos token.Pos) {
		p := fset.Position(pos)
		t.Errorf("%s:%d: a bare handler drive against a Server bypasses panicFails — a recovered panic here "+
			"answers with an ordinary 500 that every assertion still matches (#338/#511). Wrap it: "+
			"panicFails(t, srv.Handler()).ServeHTTP(w, r), or httptest.NewServer(panicFails(t, srv.Handler()))",
			file, p.Line)
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if isBareServeHTTP(v, tainted) || isBareHTTPTestServerCtor(v, tainted) {
				report(v.Pos())
			}
		case *ast.CompositeLit:
			if isBareHTTPServerLiteral(v, tainted) {
				report(v.Pos())
			}
		}
		return true
	})
}

// unparen strips any number of enclosing parentheses — "(srv.Handler())" and
// "srv.Handler()" name the same expression.
func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// isHandlerExpr reports whether e is a raw, un-wrapped http.Handler straight
// off a Server: "<x>.Handler()", "<x>.routes()" (both methods — routes() is
// api's internal chi router accessor, Handler() the public one), or "<x>.
// router" (a field, no call). The receiver <x> is unchecked — a test's local
// is `srv`, `h.srv`, `srv1`, `srv2`, ... — so this matches on method/field
// NAME alone, the same syntactic-not-typed heuristic the wildcard
// audit-actions guard uses for the same cost reason.
func isHandlerExpr(e ast.Expr) bool {
	switch v := unparen(e).(type) {
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		return ok && (sel.Sel.Name == "Handler" || sel.Sel.Name == "routes")
	case *ast.SelectorExpr:
		return v.Sel.Name == "router"
	}
	return false
}

// isHandlerIdent reports whether e is itself a raw handler expression, or a
// bare identifier this function's taint set says WAS one.
func isHandlerIdent(e ast.Expr, tainted map[string]bool) bool {
	e = unparen(e)
	if isHandlerExpr(e) {
		return true
	}
	id, ok := e.(*ast.Ident)
	return ok && tainted[id.Name]
}

// taintedIdents returns the names, within body, assigned (":=", "=", or a var
// spec's initializer) from a raw handler expression — "h := srv.Handler()"
// taints "h" for the rest of the function, the same as writing srv.Handler()
// again at every place "h" is then used as a handler.
func taintedIdents(body *ast.BlockStmt) map[string]bool {
	tainted := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range v.Rhs {
				if i >= len(v.Lhs) || !isHandlerExpr(rhs) {
					continue
				}
				if id, ok := v.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
					tainted[id.Name] = true
				}
			}
		case *ast.ValueSpec:
			for i, val := range v.Values {
				if i >= len(v.Names) || !isHandlerExpr(val) {
					continue
				}
				tainted[v.Names[i].Name] = true
			}
		}
		return true
	})
	return tainted
}

// isBareServeHTTP matches "<handler>.ServeHTTP(...)" where <handler> is a raw
// handler expression or a tainted identifier — "srv.Handler().ServeHTTP(",
// "srv.router.ServeHTTP(", "h.ServeHTTP(" (h tainted), and the parenthesized
// form "(srv.Handler()).ServeHTTP(" all match.
func isBareServeHTTP(call *ast.CallExpr, tainted map[string]bool) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ServeHTTP" {
		return false
	}
	return isHandlerIdent(sel.X, tainted)
}

// isBareHTTPTestServerCtor matches
// "httptest.NewServer/NewUnstartedServer/NewTLSServer(<handler>)" where
// <handler> is a raw handler expression or a tainted identifier — a listener
// serving every request it gets for the life of the test with no panic
// catcher on any of them.
func isBareHTTPTestServerCtor(call *ast.CallExpr, tainted map[string]bool) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != "httptest" {
		return false
	}
	switch sel.Sel.Name {
	case "NewServer", "NewUnstartedServer", "NewTLSServer":
	default:
		return false
	}
	return len(call.Args) == 1 && isHandlerIdent(call.Args[0], tainted)
}

// isBareHTTPServerLiteral matches an http.Server{} composite literal (bare or
// behind "&") whose Handler field is a raw handler expression or a tainted
// identifier — "&http.Server{Handler: srv.Handler()}" starts a real
// net/http.Server on that handler with nothing recovering a panic for it.
func isBareHTTPServerLiteral(cl *ast.CompositeLit, tainted map[string]bool) bool {
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Server" {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "http" {
		return false
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if ok && key.Name == "Handler" && isHandlerIdent(kv.Value, tainted) {
			return true
		}
	}
	return false
}

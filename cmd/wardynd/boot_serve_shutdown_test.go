// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestServeShutdownOrder pins the shutdown sequence the platform grace period
// is sized for (internal/api TestShutdownGraceCoversTheBudget): stop accepting
// requests, then wait for the detached work handlers left behind, then flush.
// Without the WaitBackground call a SIGTERM drops a superseded sign-in's
// teardown and a run launch in flight; a return on the path between the
// Shutdown call and WaitBackground, or WaitBackground moved back inside a
// branch that only runs on Shutdown's error, drops them just the same. Walks
// serveAndShutdown's AST rather than matching source text, so a reformat
// can't silently defeat the pin.
func TestServeShutdownOrder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "boot_serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	fn := findFuncDecl(file, "serveAndShutdown")
	if fn == nil {
		t.Fatal("boot_serve.go no longer defines serveAndShutdown")
	}

	const (
		callShutdown = "httpSrv.Shutdown"
		callWait     = "srv.WaitBackground"
		callFlush    = "srv.FlushAuthFailedStreak"
	)

	// firstPos records each pinned call's first-seen source position.
	firstPos := map[string]token.Pos{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := selectorName(call)
		if !ok {
			return true
		}
		switch name {
		case callShutdown, callWait, callFlush:
			if _, seen := firstPos[name]; !seen {
				firstPos[name] = call.Pos()
			}
		}
		return true
	})

	prev := token.Pos(-1)
	for _, want := range []string{callShutdown, callWait, callFlush} {
		pos, ok := firstPos[want]
		if !ok {
			t.Fatalf("serveAndShutdown no longer calls %s", want)
		}
		if pos < prev {
			t.Fatalf("serveAndShutdown calls %s before the step that must precede it", want)
		}
		prev = pos
	}
	shutdownPos, waitPos := firstPos[callShutdown], firstPos[callWait]

	// N1: a return anywhere between the Shutdown call and WaitBackground drops
	// WaitBackground on that path exactly like the pre-fix early return did —
	// regardless of nesting depth or which variable holds the error.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if ret.Pos() > shutdownPos && ret.Pos() < waitPos {
			t.Fatalf("serveAndShutdown returns (at %s) between %s and %s — a timed-out HTTP drain must still wait for background work (see internal/api TestShutdownGraceCoversTheBudget)",
				fset.Position(ret.Pos()), callShutdown, callWait)
		}
		return true
	})

	// shutdownVars collects every identifier assigned directly from the
	// httpSrv.Shutdown call (shutErr := httpSrv.Shutdown(shutCtx), including
	// inside an if's Init), so the nesting check below recognizes a guard on
	// it by variable reference, not only by re-calling Shutdown inline.
	shutdownVars := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			if name, ok := selectorName(call); !ok || name != callShutdown {
				continue
			}
			if i < len(assign.Lhs) {
				if id, ok := assign.Lhs[i].(*ast.Ident); ok {
					shutdownVars[id.Name] = true
				}
			}
		}
		return true
	})

	// N2: WaitBackground must never run only on a branch gated by Shutdown's
	// error — an if whose Init or Cond calls httpSrv.Shutdown directly, or
	// tests the variable Shutdown's result was assigned to, must not contain
	// the WaitBackground call in its body.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		guards := refsShutdown(ifs.Init, callShutdown, shutdownVars) || refsShutdown(ifs.Cond, callShutdown, shutdownVars)
		if guards && ifs.Body != nil && waitPos >= ifs.Body.Pos() && waitPos <= ifs.Body.End() {
			t.Fatalf("serveAndShutdown calls %s nested inside an if that guards on %s's error — it must run unconditionally", callWait, callShutdown)
		}
		return true
	})
}

func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// selectorName renders a call's "recv.Method" text for comparison against the
// pinned call sites, ignoring calls it can't render this way (nothing of
// interest here has a non-identifier receiver).
func selectorName(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return recv.Name + "." + sel.Sel.Name, true
}

// refsShutdown reports whether n (an *ast.IfStmt's Init or Cond, either of
// which may be nil) calls calleeName directly, or references one of vars —
// the identifiers calleeName's result was assigned to elsewhere in the
// function.
func refsShutdown(n ast.Node, calleeName string, vars map[string]bool) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if name, ok := selectorName(v); ok && name == calleeName {
				found = true
			}
		case *ast.Ident:
			if vars[v.Name] {
				found = true
			}
		}
		return true
	})
	return found
}

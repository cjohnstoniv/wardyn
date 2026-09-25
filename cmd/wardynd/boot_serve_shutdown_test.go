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
// teardown and a run launch in flight; an early return on a timed-out HTTP
// drain (or WaitBackground moved back inside the Shutdown-error branch) drops
// them just the same. Walks serveAndShutdown's AST rather than matching
// source text, so a reformat can't silently defeat the pin.
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
	wantOrder := []string{callShutdown, callWait, callFlush}

	var (
		gotOrder      []string
		waitSeen      bool
		guardStack    []bool // true while inside an if whose Init/Cond calls Shutdown
		sawEarlyGuard bool   // such an if, with a return in its body, seen before WaitBackground
	)

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			if len(guardStack) > 0 {
				guardStack = guardStack[:len(guardStack)-1]
			}
			return true
		}

		if ifs, ok := n.(*ast.IfStmt); ok {
			guardsShutdown := callsSelector(ifs.Init, "Shutdown") || callsSelector(ifs.Cond, "Shutdown")
			guardStack = append(guardStack, guardsShutdown)
			if guardsShutdown && !waitSeen && containsReturn(ifs.Body) {
				sawEarlyGuard = true
			}
			return true
		}

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
			gotOrder = append(gotOrder, name)
			if name == callWait {
				waitSeen = true
				for _, guarded := range guardStack {
					if guarded {
						t.Fatalf("serveAndShutdown calls %s nested inside an if that tests httpSrv.Shutdown's error — it must run unconditionally", callWait)
					}
				}
			}
		}
		return true
	})

	if sawEarlyGuard {
		t.Fatalf("serveAndShutdown returns from an httpSrv.Shutdown error before %s runs — a timed-out HTTP drain must still wait for background work (see internal/api TestShutdownGraceCoversTheBudget)", callWait)
	}

	firstIndex := map[string]int{}
	for i, name := range gotOrder {
		if _, ok := firstIndex[name]; !ok {
			firstIndex[name] = i
		}
	}
	prev := -1
	for _, want := range wantOrder {
		idx, ok := firstIndex[want]
		if !ok {
			t.Fatalf("serveAndShutdown no longer calls %s", want)
		}
		if idx < prev {
			t.Fatalf("serveAndShutdown calls %s before the step that must precede it", want)
		}
		prev = idx
	}
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

// callsSelector reports whether n (an *ast.IfStmt's Init or Cond, either of
// which may be nil) contains a call to a method named methodName.
func callsSelector(n ast.Node, methodName string) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == methodName {
				found = true
			}
		}
		return true
	})
	return found
}

// containsReturn reports whether body has a return statement reachable
// without crossing into a nested closure.
func containsReturn(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.ReturnStmt:
			found = true
		case *ast.FuncLit:
			return false
		}
		return true
	})
	return found
}

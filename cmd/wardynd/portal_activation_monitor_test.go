// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

type monitorSpy struct{ ctxs chan context.Context }

func (m monitorSpy) RunPortalActivationMonitor(ctx context.Context) {
	m.ctxs <- ctx
	<-ctx.Done()
}

// The helper starts the monitor once, on the context it was given, and the monitor ends with that context.
func TestStartPortalActivationMonitor_RunsOnTheServerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spy := monitorSpy{ctxs: make(chan context.Context, 2)}
	startPortalActivationMonitor(ctx, spy)
	select {
	case got := <-spy.ctxs:
		if got != ctx {
			t.Fatal("the monitor was started on another context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor was never started")
	}
	select {
	case <-spy.ctxs:
		t.Fatal("the monitor was started twice")
	case <-time.After(100 * time.Millisecond):
	}
}

// The boot path starts it: startBackgroundWorkers, which every serving replica runs after its store is ready,
// calls startPortalActivationMonitor exactly once, not behind the sweeper leader's gate (a call under leaderGo or
// a leader check would leave a follower's activation answering sample_warming for good).
func TestStartBackgroundWorkers_StartsPortalActivationMonitor(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "boot_serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "startBackgroundWorkers" {
			continue
		}
		for _, stmt := range fd.Body.List { // top-level statements only: a call nested under an if is gated
			if es, ok := stmt.(*ast.ExprStmt); ok {
				if ce, ok := es.X.(*ast.CallExpr); ok {
					if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "startPortalActivationMonitor" {
						calls++
					}
				}
			}
		}
	}
	if calls != 1 {
		t.Fatalf("startBackgroundWorkers calls startPortalActivationMonitor %d times at its top level, want 1", calls)
	}
}

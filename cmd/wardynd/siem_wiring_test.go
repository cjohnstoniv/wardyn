// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The rows the database writes itself reach the SIEM sinks only if run() hands them the sink. These tests pin
// that hand-over: the retention boot path sends its row with the stored hashes, the drain's recorder sends
// nothing for a write the store refused, and run() wires the one store and the key manager to the sink.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type captureSink struct {
	mu  sync.Mutex
	got []types.AuditEvent
}

func (*captureSink) Name() string { return "capture" }

func (c *captureSink) Emit(_ context.Context, ev types.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, ev)
	return nil
}

func (c *captureSink) events() []types.AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]types.AuditEvent(nil), c.got...)
}

func TestPG_StartAuditRetentionSendsTheSetRowToTheSIEM(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	sink := &captureSink{}
	st := store.NewPG(pool)
	st.SIEM = sink
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	days, autodrop := 45, false

	startAuditRetention(ctx, &bootFlags{auditRetentionDays: &days, auditRetentionAutodrop: &autodrop}, st, nil)

	got := sink.events()
	if len(got) != 1 || got[0].Action != "audit.retention.set" {
		t.Fatalf("the sink received %+v, want exactly one audit.retention.set", got)
	}
	var prev, hash string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE id = $1`, got[0].ID).Scan(&prev, &hash); err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	if hash == "" || got[0].RowHash != hash || got[0].PrevHash != prev {
		t.Errorf("the sink's hashes %q/%q differ from the stored %q/%q", got[0].PrevHash, got[0].RowHash, prev, hash)
	}
}

func TestLandedRecorderSendsNothingForARefusedStoreWrite(t *testing.T) {
	pool, _ := miscCovClosedPool(t)
	sink := &captureSink{}
	rec := landedRecorder{primary: store.Recorder{Pool: pool}, fanout: sinks.NewFanout(sink)}

	if err := rec.Record(t.Context(), types.AuditEvent{Action: "approval.decide", ActorType: types.ActorSystem, Actor: "wardynd", Outcome: "success"}); err == nil {
		t.Fatal("a write to a closed pool reported success")
	}
	if got := sink.events(); len(got) != 0 {
		t.Fatalf("the sink received %+v for a row the store never took", got)
	}
}

// siemWiringViolations parses main.go's source and returns what run() leaves unwired: the sink every
// database-written row goes to, the one store that carries it (to the API and to the background workers,
// where the retention sweeper runs), and the key manager's destroy row.
func siemWiringViolations(t *testing.T, src string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "run" && fn.Recv == nil {
			run = fn
		}
	}
	if run == nil {
		return []string{"main.go has no func run()"}
	}
	var siemDefined, storeSet, keysArmed, workersGetStore, apiGetsStore, storeDefined bool
	// store.PG is a value, so every copy of st taken before st.SIEM is set carries a nil sink.
	var setPos, firstUse token.Pos
	noteUse := func(p token.Pos) {
		if firstUse == token.NoPos || p < firstUse {
			firstUse = p
		}
	}
	ast.Inspect(run, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
				return true
			}
			switch lhs := x.Lhs[0].(type) {
			case *ast.Ident:
				if call, ok := x.Rhs[0].(*ast.CallExpr); ok && lhs.Name == "siem" && identName(call.Fun) == "siemSink" {
					siemDefined = true
				}
				if call, ok := x.Rhs[0].(*ast.CallExpr); ok && lhs.Name == "st" {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && identName(sel.X) == "store" && sel.Sel.Name == "NewPG" {
						storeDefined = true
					}
				}
			case *ast.SelectorExpr:
				if identName(lhs.X) == "st" && lhs.Sel.Name == "SIEM" && identName(x.Rhs[0]) == "siem" {
					storeSet, setPos = true, x.Pos()
				}
			}
		case *ast.CallExpr:
			switch identName(x.Fun) {
			case "armKeyDestroySIEM":
				keysArmed = len(x.Args) == 2 && identName(x.Args[1]) == "siem"
			case "startBackgroundWorkers":
				for _, a := range x.Args {
					if identName(a) == "st" {
						workersGetStore = true
					}
				}
			}
			for _, a := range x.Args {
				if identName(a) == "st" {
					noteUse(a.Pos())
				}
			}
		case *ast.KeyValueExpr:
			if identName(x.Key) == "Store" && identName(x.Value) == "st" {
				apiGetsStore = true
				noteUse(x.Value.Pos())
			}
		}
		return true
	})
	var out []string
	if storeSet && firstUse != token.NoPos && setPos > firstUse {
		out = append(out, "st.SIEM = siem comes after the first use of st (a copy of st would carry no sink)")
	}
	for _, c := range []struct {
		ok   bool
		what string
	}{
		{siemDefined, "siem := siemSink(...)"},
		{storeDefined, "st := store.NewPG(...)"},
		{storeSet, "st.SIEM = siem"},
		{keysArmed, "armKeyDestroySIEM(secrets, siem)"},
		{workersGetStore, "st passed to startBackgroundWorkers"},
		{apiGetsStore, "Store: st in the API config"},
	} {
		if !c.ok {
			out = append(out, "run() does not have "+c.what)
		}
	}
	return out
}

func TestRunWiresTheSIEMToTheDatabaseWrittenRows(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if v := siemWiringViolations(t, string(b)); len(v) != 0 {
		t.Fatalf("SIEM wiring violations:\n%s", strings.Join(v, "\n"))
	}

	t.Run("st.SIEM set after a copy of st is caught", func(t *testing.T) {
		const late = `package main
func run() error {
	siem := siemSink(fan)
	st := store.NewPG(pool)
	_ = api.Config{Store: st}
	st.SIEM = siem
	startBackgroundWorkers(a, b, st)
	armKeyDestroySIEM(secrets, siem)
	return nil
}
`
		if v := strings.Join(siemWiringViolations(t, late), "\n"); !strings.Contains(v, "after the first use of st") {
			t.Errorf("violations %q do not name the out-of-order assignment", v)
		}
	})

	t.Run("an unwired run() is caught", func(t *testing.T) {
		const bad = `package main
func run() error {
	siem := siemSink(fan)
	st := store.NewPG(pool)
	startBackgroundWorkers(a, b, st)
	_ = api.Config{Store: st}
	armKeyDestroySIEM(secrets, nil)
	return nil
}
`
		v := strings.Join(siemWiringViolations(t, bad), "\n")
		for _, want := range []string{"st.SIEM = siem", "armKeyDestroySIEM(secrets, siem)"} {
			if !strings.Contains(v, want) {
				t.Errorf("violations %q do not name %q", v, want)
			}
		}
	})
}

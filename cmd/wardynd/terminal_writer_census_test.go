// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// terminalRunStates is the set whose arrival can strand a PENDING approval. A
// run that reaches any of them has an agent that is gone, so every question it
// asked a human is one nobody can answer any more.
var terminalRunStates = map[string]bool{
	"RunKilled": true, "RunCompleted": true, "RunFailed": true, "RunStopped": true,
}

// terminalWriterCensus is the FROZEN list of functions that move a run into a
// terminal state, and what each one does about the approvals it strands.
//
// It is frozen because the first version of B4's cascade was written against a
// census taken by hand and got it wrong: it claimed "the terminal transitions
// that CAN strand an approval are finalizeRunTail's and handleKillRun's", and
// missed lifecycleStopper.StopRun — the idle reaper, in a different package,
// the ONLY writer of STOPPED, and the writer MOST likely to have an approval to
// strand (an idle-stopped run is typically idle because its agent is parked on
// a wait_for_review hold). The consequence was a live hole, not a stale comment:
// terminalCancelReason's RunStopped arm was unreachable, AUDIT-ACTIONS.md
// documented a reason nothing emitted, and the stranded approval stayed
// decidable for 24h — long enough for an `always` approve to be replayed into
// the workspace allowlist for a sandbox that no longer existed.
//
// A new writer added anywhere in internal/api or cmd/wardynd reds this test,
// which is the point: the author must say which treatment it gets before the
// transition can ship.
var terminalWriterCensus = map[string]string{
	// (1) Everything that routes through the shared terminal tail
	// (finalizeRunTail, which calls cancelRunApprovals). Each wins its own CAS
	// and hands off, so they are one treatment.
	"startCompletionWatcher":   "CASes, then finalizeRunTail",
	"reclaimProbeRun":          "CASes, then finalizeRunTail",
	"finalizeUndispatchedRuns": "reconcileFinalize -> finalizeRunTail",
	"sweepRunWatchers":         "reconcileFinalize -> finalizeRunTail",
	"reconcileWatch":           "reconcileFinalize -> finalizeRunTail",
	// (2) The kill switch, which deliberately does NOT route through the tail.
	// handleKillRun is the HTTP half only; the cascade is killRunCascade
	// (claimKillTransition then killTeardownTail — #122 split the CAS out of the
	// slow teardown so the login supersede could claim it synchronously and hand
	// the teardown to a detached goroutine). claimKillTransition is the writer:
	// it owns the CAS and calls cancelRunApprovals directly, right after. Both
	// killRunCascade's caller (handleKillRun) and supersedeOneLoginRun call it.
	"claimKillTransition": "CASes, then calls cancelRunApprovals directly",
	// (3) The idle reaper, in this package. Reaches the same helper through
	// api.Server.CancelTerminalRunApprovals, threaded in at boot.
	"StopRun": "calls cancelApprovals (api.Server.CancelTerminalRunApprovals)",
	// (4) The create/dispatch compensator, which is BOTH: it cascades when it is
	// handed from=RunRunning (runs_dispatch.go fails a run three times after the
	// STARTING->RUNNING CAS — the exec-less BYOI refusal, a failed `agent-run
	// --selftest`, a failed task Exec — with the sandbox and proxy sidecar up and
	// an egress approval already raisable), and skips it below RUNNING, where no
	// approval can exist yet. Both arms are pinned in internal/api. The frozen
	// claim here USED to be "exempt: fails a run that never reached RUNNING" —
	// which was false at three call sites and is what let a PENDING approval sit
	// in the queue for 24h and expire as "nobody answered".
	"failAndRevoke": "calls cancelRunApprovals when from==RunRunning; exempt below it",
	// (5) The lease (#568) and lost runs (#574): a run whose end passed, or
	// whose sandbox was lost, and could not be kept, or whose grace ran out.
	// CASes RUNNING->STOPPED or FAILED, then finalizeRunTail.
	"stopKeptRun": "CASes, then finalizeRunTail",
	// A run whose token lapsed and that cannot be kept (#574).
	"sweepLapsedRunTokens": "reconcileFinalize -> finalizeRunTail",
}

// TestTerminalRunStateWriterCensus scans every non-test .go file in internal/api
// and cmd/wardynd for a write of a terminal run state and reports the enclosing
// function, so the census above cannot silently go stale the way its prose
// ancestor did.
func TestTerminalRunStateWriterCensus(t *testing.T) {
	root := repoRoot(t)
	found := map[string]string{} // func -> file
	for _, pkg := range []string{filepath.Join("internal", "api"), filepath.Join("cmd", "wardynd")} {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || !writesTerminalState(call) {
						return true
					}
					found[fn.Name.Name] = filepath.Join(pkg, name)
					return true
				})
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no terminal run-state writer found at all — this guard has lost its anchor, re-derive it")
	}
	for fn, file := range found {
		if _, ok := terminalWriterCensus[fn]; !ok {
			t.Errorf("%s (%s) writes a terminal run state and is NOT in terminalWriterCensus.\n"+
				"Every terminal transition strands the run's PENDING approvals: either call the shared "+
				"cascade (finalizeRunTail / cancelRunApprovals / api.Server.CancelTerminalRunApprovals), "+
				"or add an entry here saying why it is exempt.", fn, file)
		}
	}
	for fn := range terminalWriterCensus {
		if _, ok := found[fn]; !ok {
			t.Errorf("terminalWriterCensus lists %q, which no longer writes a terminal run state — "+
				"drop the entry so the census stays a census", fn)
		}
	}
}

// writesTerminalState reports whether call is a run-state write whose TARGET is
// one of the terminal states — the two primitives every transition goes through
// (internal/api's casRunState, and the reaper's guarded UpdateRunStateIfIdle) —
// or a finalize helper handed a terminal state.
func writesTerminalState(call *ast.CallExpr) bool {
	name := ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	switch name {
	case "casRunState", "UpdateRunStateIfIdle", "reconcileFinalize":
	default:
		return false
	}
	for _, arg := range call.Args {
		if namesTerminalState(arg) {
			return true
		}
	}
	return false
}

// namesTerminalState reports whether expr is a types.RunX terminal constant, or
// a plain identifier bound to one (reconcile.go's `final`, the watcher's
// `terminal` — both start at types.RunFailed/RunCompleted).
func namesTerminalState(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return terminalRunStates[e.Sel.Name]
	case *ast.Ident:
		return e.Name == "terminal" || e.Name == "final"
	}
	return false
}

// TestIdleReaperIsWiredToTheApprovalCascade pins the WIRING the census above can
// only see the shape of: the reaper's stopper really is handed
// api.Server.CancelTerminalRunApprovals at boot. Without it the third writer
// compiles, runs, and cancels nothing — which is exactly the state this lane
// found it in.
func TestIdleReaperIsWiredToTheApprovalCascade(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "wardynd", "boot_serve.go"))
	if err != nil {
		t.Fatalf("read boot_serve.go: %v", err)
	}
	if !strings.Contains(string(b), "cancelApprovals: srv.CancelTerminalRunApprovals") {
		t.Error("startBackgroundWorkers no longer hands lifecycleStopper the approval cascade — " +
			"an idle-stopped run's PENDING approvals go back to sitting in the queue until the 24h sweeper")
	}
}

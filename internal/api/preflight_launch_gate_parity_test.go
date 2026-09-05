// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// TestPreflightMirrorsLaunchGates is the STRUCTURAL half of preflight parity.
//
// Every other preflight parity test (TestPreflight_WorkspaceIDSeeded,
// TestPreflightAnswersTheSameDriveRefusalAsCreate,
// TestWorkspaceRequirements_PreflightLaunchAgreement, …) names one specific,
// already-known gate — so none of them can fail when an UNKNOWN new gate is
// added to the launch path only, which is precisely the drift that has already
// happened once (the G3/PF-34 post-seed capability re-check). handlePreflightRun's
// own doc comment carries a hand-maintained "Not reproduced" inventory; this
// test makes that inventory executable.
//
// The rule: every gate helper handleCreateRun calls on the Server BEFORE it
// mints a run identity — the dry-run prefix, i.e. everything preflight claims to
// reproduce — must also be called by handlePreflightRun, unless it is named in
// preflightGateExceptions below with the reason. Adding a gate to launch
// therefore forces a deliberate decision here instead of silently previewing a
// rosier checklist than the launch it previews.
//
// MintRunIdentity is the boundary marker: "it mints nothing, persists nothing,
// dispatches nothing" is preflight's contract, so everything after the mint is
// launch-only by construction and out of scope. A missing marker fails loudly
// rather than silently widening the scan to the whole handler.
var preflightGateExceptions = map[string]string{
	// Preflight decodes the same createRunRequest but reproduces this wrapper's
	// gates individually (decodeStrict + denyMemberRequest + the eager
	// integration_id check); the rest of it — the agent-required 400, the BYOI
	// image/devcontainer 400s — is unreachable via the wizard body this endpoint
	// serves, per handlePreflightRun's doc comment.
	"decodeAndValidateCreateRun": "preflight reproduces its reachable gates inline (decodeStrict + denyMemberRequest + the integration_id 400)",
	// Preflight calls the shared enforcedConfinement math directly and
	// deliberately skips this wrapper's TAIL gates (runner capability, the
	// cloud_sts grantChecker): deriveSetupItems' backend row reports an
	// unenforceable class as a fixable checklist row instead of a fatal error
	// that blanks the Review panel.
	"resolveEnforcedConfinement": "preflight calls enforcedConfinement directly; the runner-capability + cloud_sts tail gates are reported by the checklist instead (doc comment)",
}

func TestPreflightMirrorsLaunchGates(t *testing.T) {
	fset := token.NewFileSet()
	create := parseHandler(t, fset, "runs.go", "handleCreateRun")
	preflight := parseHandler(t, fset, "preflight.go", "handlePreflightRun")

	mint := serverCallPos(create, "MintRunIdentity")
	if mint == token.NoPos {
		t.Fatal("handleCreateRun no longer calls MintRunIdentity — this test uses the mint as the " +
			"dry-run/launch-only boundary; re-point it at the new boundary rather than deleting it")
	}

	launchGates := serverCalls(create, mint)
	previewed := serverCalls(preflight, token.NoPos)

	var missing []string
	for name := range launchGates {
		if previewed[name] {
			continue
		}
		if _, ok := preflightGateExceptions[name]; ok {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("handleCreateRun gates handlePreflightRun does not reproduce: %v\n"+
			"Either call them from handlePreflightRun (preferred — a shared helper both call is better still), "+
			"or add each to preflightGateExceptions with the reason Review is allowed to skip it.", missing)
	}

	// The exception list must not rot: an entry naming a helper launch no longer
	// calls is a stale licence to diverge.
	for name := range preflightGateExceptions {
		if !launchGates[name] {
			t.Errorf("preflightGateExceptions names %q, which handleCreateRun no longer calls before the mint — drop the entry", name)
		}
	}
}

// parseHandler returns the named top-level method's body from an internal/api
// source file.
func parseHandler(t *testing.T, fset *token.FileSet, file, name string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("%s: no method %s", file, name)
	return nil
}

// serverCalls collects the names of every `s.<Name>(…)` call in fn's body,
// stopping at `before` when it is a real position (token.NoPos scans all of it).
// `s.cfg.X.Y()` and package-level calls are deliberately not collected: this
// guard is about the Server's own gate helpers.
func serverCalls(fn *ast.FuncDecl, before token.Pos) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if before != token.NoPos && call.Pos() >= before {
			return true
		}
		if name := serverMethodName(call); name != "" {
			out[name] = true
		}
		return true
	})
	return out
}

// serverCallPos returns the position of the first call whose selector ends in
// name (receiver-agnostic: the mint rides on s.cfg.Identity).
func serverCallPos(fn *ast.FuncDecl, name string) token.Pos {
	found := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found != token.NoPos {
			return found == token.NoPos
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = call.Pos()
			return false
		}
		return true
	})
	return found
}

func serverMethodName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != "s" {
		return ""
	}
	return sel.Sel.Name
}

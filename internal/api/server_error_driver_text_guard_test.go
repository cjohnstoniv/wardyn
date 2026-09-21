// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This guard is #173's half of the fix: converting the 93 sites that built a
// 5xx body by concatenating err.Error() onto an action string
// (writeServerError/loggedMsg in writeservererror.go are the shipped
// answer) is a one-time edit. Nothing stopped the NEXT handler from being
// written the old way — the raw pattern still compiles, still passes review
// at a glance, and still ships pgx/driver text (the DB host, port, SQLSTATE,
// constraint name) to a caller as unprivileged as a member reading their own
// run. This test is what stops that: it reads every non-test source file in
// this package and fails on any writeError call whose status is a KNOWN 5xx
// and whose message calls err.Error() anywhere in its expression tree.
//
// It reads the AST rather than grepping: a byte scanner would trip on this
// comment's own examples and on every fixture in the _test.go files a raw
// scan would have to remember to skip by hand. go/parser already knows the
// difference between code and a string literal or a comment.
//
// Scope, deliberately narrower than "every writeError call reachable from a
// 5xx": the status argument must be one of the http.Status5xx identifiers
// (or a raw 5xx int literal) written AT THE CALL SITE. A few call sites pass
// a dynamic `code int` that is 4xx on every branch today (validateWorkspaceSources
// and its siblings, all in workspace_refs.go/inline_policy.go/runs_create.go,
// verified by hand when this guard was written); this guard does not attempt
// the dataflow analysis that would be needed to prove that stays true, so it
// does not watch them. What it does watch — the literal/named-constant
// surface — is exactly the shape every site this issue fixed had, and the
// shape a re-added leak is overwhelmingly likely to take: nobody accidentally
// routes a hardcoded 500 through three layers of indirection.
var serverErrorDriverTextAllowlist = map[string]string{
	// injection_awssso.go:356 — the credential-reauth-raise failure is a
	// DELIBERATELY MODELLED body (docs/design/0.8/PLAN.md's AWS SSO lane):
	// credentialReauthRaiseFailedBody is a frozen operator-facing sentence and
	// aerr here is s.cfg.Approvals.Request's own error, never driver/substrate
	// text. #173's DO NOT TOUCH names this site explicitly.
	"injection_awssso.go:356": "modelled AWS SSO reauth-raise body; #173 DO NOT TOUCH",
	// run_resources.go:201 — reached only after errors.Is(err,
	// runner.ErrExecStreamUnsupported) just matched, so err.Error() here is
	// always that sentinel's own fixed text ("runner: ExecStream not
	// supported"), never driver/substrate text. Converting it would also
	// break TestRunResources_ExecStreamUnsupported_Returns501, which pins the
	// sentinel staying in the 501 body so the console/operator can tell this
	// case apart from the no-runner-configured guard beside it.
	"run_resources.go:201": "fixed ErrExecStreamUnsupported sentinel, not driver text; pinned by TestRunResources_ExecStreamUnsupported_Returns501",
	// Every other site #173 found was FIXED, not allowlisted — a new entry
	// here needs the same kind of justification (a named fixed sentinel or a
	// design record, plus a pinning test) as these two, not just a passing
	// build.
}

// server5xxStatusIdents are the http.Status identifiers this guard treats as
// a 5xx for the purpose of flagging a writeError call. StatusFailedDependency
// (424) and the 4xx family are deliberately absent — #173 leaves 4xx bodies
// alone, and a 4xx is the caller's own mistake restated, not the deployment's
// internals.
var server5xxStatusIdents = map[string]bool{
	"StatusInternalServerError":     true, // 500
	"StatusNotImplemented":          true, // 501
	"StatusBadGateway":              true, // 502
	"StatusServiceUnavailable":      true, // 503
	"StatusGatewayTimeout":          true, // 504
	"StatusHTTPVersionNotSupported": true, // 505
	"StatusInsufficientStorage":     true, // 507
	"StatusLoopDetected":            true, // 508
	"StatusNotExtended":             true, // 510
}

// TestNoDriverTextInServerErrorBody walks internal/api's non-test sources and
// fails on a writeError(w, <5xx>, ...) call whose message argument calls
// err.Error() anywhere inside it — the pattern writeServerError/loggedMsg
// exist to replace. See the package doc comment above for scope and the
// allowlist's one deliberate exception.
func TestNoDriverTextInServerErrorBody(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("read %s: %v", wd, err)
	}

	found := map[string]string{} // "file.go:line" -> source snippet
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(wd, name)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		file, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "writeError" || len(call.Args) < 3 {
				return true
			}
			if !is5xxStatusArg(call.Args[1]) {
				return true
			}
			if !callsErrorMethod(call.Args[2]) {
				return true
			}
			pos := fset.Position(call.Pos())
			key := fmt.Sprintf("%s:%d", name, pos.Line)
			found[key] = strings.TrimSpace(exprSourceLine(src, pos.Line))
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned 0 files — the guard's directory listing is wrong")
	}

	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := serverErrorDriverTextAllowlist[k]; ok {
			continue
		}
		t.Errorf("%s builds a 5xx body from err.Error(): %s\n"+
			"use writeServerError(w, r, \"<action>\", err) for a 500, or "+
			"writeError(w, code, loggedMsg(ctx, \"<action>\", err)) for another 5xx — "+
			"see internal/api/writeservererror.go. If this is deliberately modelled "+
			"(like injection_awssso.go's AWS reauth body), add it to "+
			"serverErrorDriverTextAllowlist with the same kind of justification "+
			"that entry carries.", k, found[k])
	}
	for k, why := range serverErrorDriverTextAllowlist {
		if _, ok := found[k]; !ok {
			t.Errorf("serverErrorDriverTextAllowlist has a stale entry %q (%s) — "+
				"the site it names no longer leaks err.Error() into a 5xx body; "+
				"shrink the allowlist by removing it", k, why)
		}
	}
	t.Logf("scanned %d files, %d allowlisted site(s), %d violation(s)", scanned, len(serverErrorDriverTextAllowlist), len(found)-len(serverErrorDriverTextAllowlist))
}

// is5xxStatusArg reports whether arg is a status this guard recognizes as a
// 5xx: an http.StatusXxx selector naming one of server5xxStatusIdents, or a
// raw integer literal in [500,599]. Anything else (a variable, a function
// call) is NOT a 5xx this guard can prove — see the package doc comment.
func is5xxStatusArg(arg ast.Expr) bool {
	switch e := arg.(type) {
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		return ok && pkg.Name == "http" && server5xxStatusIdents[e.Sel.Name]
	case *ast.BasicLit:
		if e.Kind != token.INT {
			return false
		}
		var n int
		if _, err := fmt.Sscanf(e.Value, "%d", &n); err != nil {
			return false
		}
		return n >= 500 && n <= 599
	default:
		return false
	}
}

// callsErrorMethod reports whether expr's tree contains a zero-argument
// call to a method named Error — the err.Error() shape, whatever the
// receiver's variable name (err, gerr, lerr, cerr, aerr... every one of
// #173's sites used a different one, which is exactly why this checks the
// METHOD, not a variable name).
func callsErrorMethod(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "Error" {
			found = true
			return false
		}
		return true
	})
	return found
}

// exprSourceLine returns src's line n (1-indexed), or "" if out of range —
// used only to make a failure readable, never to decide anything.
func exprSourceLine(src []byte, n int) string {
	lines := strings.Split(string(src), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

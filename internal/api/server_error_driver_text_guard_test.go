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
//
// #295's ONE-HOP EXTENSION. A direct writeError(w, 5xx, "..."+err.Error())
// call site is not the only shape #173 left standing: a handler can build the
// (status, message) pair and hand it to a HELPER that calls writeError
// itself, and the helper's own call site is all this guard used to read —
// where the message has already collapsed into one opaque string argument.
// serverErrorForwarders below names the helpers this guard now follows one
// hop into: refuseCapture and uiFail take (status, msg) as ordinary
// arguments, so the same is5xxStatusArg/callsErrorMethod checks run against
// the ARGUMENT EXPRESSIONS at their call sites instead of writeError's.
// driveBindFailureHere (and driveShareBindFailure, its tail call) builds its
// answer as a *driveBindFailure composite literal rather than a function
// call, so that shape is matched separately, by its status/member fields,
// wherever such a literal is constructed. callsErrorMethod also treats a call
// to sshExecStreamErrorMessage as carrying error text: its own fallback arm
// is "exec failed: "+err.Error(), so a message built by calling it is exactly
// as unsafe as calling err.Error() inline, one level up. This is deliberately
// ONE hop, matching the issue's scope — a helper that forwards through a
// SECOND helper is not chased; every known forwarder found by hand when this
// extension was written reaches writeError (or, for the two SSH-exec-only
// sshExecStreamErrorMessage callers in sshgateway_channels.go, a channel
// stderr write that is not an HTTP 5xx body at all and this guard does not
// watch) in one step.
var serverErrorDriverTextAllowlist = map[string]string{
	// handleInternalCredentialReauth's raise-failure arm is a
	// DELIBERATELY MODELLED body (docs/design/0.8/PLAN.md's AWS SSO lane):
	// credentialReauthRaiseFailedBody is a frozen operator-facing sentence and
	// aerr here is s.cfg.Approvals.Request's own error, never driver/substrate
	// text. #173's DO NOT TOUCH names this site explicitly.
	"injection_awssso.go:379": "modelled AWS SSO reauth-raise body; #173 DO NOT TOUCH",
	// handleRunResourcesExecStream's unsupported arm is reached only after errors.Is(err,
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

// serverErrorForwarder names one hop this guard follows: a function or
// method whose ordinary arguments already carry a (status, message) pair
// bound for writeError, at fixed positions written at the call site (the
// same "no dataflow analysis" limit is5xxStatusArg's doc describes — a
// forwarder that received its status or message through a variable built
// elsewhere is not something this guard can prove either way).
type serverErrorForwarder struct {
	statusArg int
	msgArg    int
}

// serverErrorForwarders is the known-forwarder table #295 asks this guard to
// follow. Matched by the called function/method's NAME alone (not by
// package-qualifying it), which is safe here because both names are unique
// in this package: refuseCapture is awssso_pin.go's audited sso-token
// refusal writer (ssotoken.go's four call sites are exactly what #295 fixed),
// uiFail is uigateway.go's UI-relay dial-error constructor.
var serverErrorForwarders = map[string]serverErrorForwarder{
	"refuseCapture": {statusArg: 3, msgArg: 5}, // s.refuseCapture(w, r, claims, status, reason, msg, scope)
	"uiFail":        {statusArg: 1, msgArg: 2}, // uiFail(ctx, status, msg)
}

// calleeName returns a CallExpr's called function or method name, or "" for
// a call through anything else (a func value, an indexed/parenthesized
// expression) — none of which this guard's fixed forwarder table matches.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}

// driveBindFailureLitStatusMember reports the status and member field
// expressions of a `driveBindFailure{...}` composite literal, or nil, nil if
// lit is not one or carries neither field. driveBindFailureHere and its tail
// call driveShareBindFailure (user_drives_run.go) answer with this literal
// rather than a call into writeError, so the (status, message) pair this
// guard needs lives in the literal's fields, not in call arguments.
func driveBindFailureLitStatusMember(lit *ast.CompositeLit) (status, member ast.Expr) {
	id, ok := lit.Type.(*ast.Ident)
	if !ok || id.Name != "driveBindFailure" {
		return nil, nil
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "status":
			status = kv.Value
		case "member":
			member = kv.Value
		}
	}
	return status, member
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
			switch node := n.(type) {
			case *ast.CallExpr:
				// Direct shape: writeError(w, <5xx>, <msg with err.Error()>).
				if fn, ok := node.Fun.(*ast.Ident); ok && fn.Name == "writeError" && len(node.Args) >= 3 {
					if is5xxStatusArg(node.Args[1]) && callsErrorMethod(node.Args[2]) {
						pos := fset.Position(node.Pos())
						key := fmt.Sprintf("%s:%d", name, pos.Line)
						found[key] = strings.TrimSpace(exprSourceLine(src, pos.Line))
					}
					return true
				}
				// #295's one-hop shape: a call into a known forwarder that
				// itself carries writeError's (status, msg) pair as ordinary
				// arguments at fixed positions.
				fwd, ok := serverErrorForwarders[calleeName(node)]
				if !ok {
					return true
				}
				maxArg := fwd.statusArg
				if fwd.msgArg > maxArg {
					maxArg = fwd.msgArg
				}
				if len(node.Args) > maxArg && is5xxStatusArg(node.Args[fwd.statusArg]) && callsErrorMethod(node.Args[fwd.msgArg]) {
					pos := fset.Position(node.Pos())
					key := fmt.Sprintf("%s:%d", name, pos.Line)
					found[key] = strings.TrimSpace(exprSourceLine(src, pos.Line))
				}
			case *ast.CompositeLit:
				// #295's other one-hop shape: driveBindFailureHere /
				// driveShareBindFailure answer with a *driveBindFailure
				// literal instead of a writeError call, so the (status,
				// message) pair lives in its fields, not call arguments.
				status, member := driveBindFailureLitStatusMember(node)
				if status != nil && member != nil && is5xxStatusArg(status) && callsErrorMethod(member) {
					pos := fset.Position(node.Pos())
					key := fmt.Sprintf("%s:%d", name, pos.Line)
					found[key] = strings.TrimSpace(exprSourceLine(src, pos.Line))
				}
			}
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
// sshExecStreamErrorMessage's own fallback arm is "exec failed: "+err.Error()
// (sshgateway_channels.go), so a message built by calling it carries driver
// text exactly as directly as calling err.Error() inline one level up — see
// the package doc comment's one-hop discussion.
const errMethodForwarder = "sshExecStreamErrorMessage"

func callsErrorMethod(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name == "Error" && len(call.Args) == 0 {
				found = true
				return false
			}
		case *ast.Ident:
			if fn.Name == errMethodForwarder {
				found = true
				return false
			}
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

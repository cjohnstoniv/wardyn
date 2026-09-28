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
	"strconv"
	"strings"
	"testing"
)

// #656's own DONE WHEN: "guard green on the whole route table" — every
// non-2xx body this package writes carries a machine-readable reason, or is
// one of a small, reviewed, named exception. writeError(w, status, msg) and
// the standard library's http.Error(w, msg, status) are the two bare writers
// #656 has spent three slices (and one final review round) converting call
// sites away from; this guard is what stops a new one from being added by
// accident once the sweep is done. It is this package's own idiom for
// "refused at compile time" (server_error_driver_text_guard_test.go is the
// model): a plain Go type cannot forbid a 5xx literal at compile time without
// a struct-wrapped status type touching every remaining caller for no
// behavioral gain, so the enforcement point is a test that fails the build
// the moment an unreviewed bare site appears — exactly as every other guard
// in this package already works (TestReasonDocsMatchReasonsGo,
// TestNoAdHocAuthz, TestNoDriverTextInServerErrorBody).
//
// Scope: the bare writeError(w, status, msg) and http.Error(w, msg, status)
// call shapes. writeErrorReason, writeJSON and writeServerError all already
// carry (or, for writeServerError, always attach reasonInternalError at
// minimum) a reason and are not this guard's concern. driveBindFailure's own
// bare path (user_drives_run.go) resolves through writeError too — see
// driveBindFailure.write, whose *one* writeError(w, f.status, f.body()) call
// is what this guard finds; it does not re-derive which of the several
// driveBindFailure composite literals could reach it; the write() method's
// own doc comment already states which of its two arms are deliberately
// bare.
//
// Keyed by "file:enclosing-symbol", not by line — same reason
// server_error_driver_text_guard_test.go's own allowlist is: a line number
// shifts on any unrelated edit landing above the site, and that must never
// make a still-correct entry look stale. Each entry PINS the exact number of
// sites it covers (#656 final review round FIX-2): `if _, ok := ...; ok {
// continue }` used to skip every bare call inside an allowlisted function,
// so a NEW bare site landing inside one went undetected — the reviewer
// proved this by adding a 4th bare call to enforceRunModelProvider and
// watching the guard still pass. Comparing len(found[key]) against the
// entry's own count closes that: an allowlisted function may have EXACTLY
// its stated number of bare sites, never more, never fewer.
type bareWriteErrorEntry struct {
	count int
	why   string
}

var bareWriteErrorAllowlist = map[string]bareWriteErrorEntry{
	// run_model_provider.go's own two sites (both inside
	// enforceRunModelProvider, hence the same key): "a transient store
	// failure is no door" (multi-provider §5.8) — the site config read and
	// the credential read both answer with the sentence alone, deliberately.
	// TestProviderJoin_DoorsEveryKind pins BOTH "provider-unreadable" and
	// "block-unreadable" reason-less; TestRunModelProviderDoors' "a
	// credential that cannot be read refuses with the sentence alone" case
	// pins the second.
	"run_model_provider.go:Server.enforceRunModelProvider": {2, "both 5xx (StatusServiceUnavailable): a transient store failure is no door (multi-provider §5.8); TestProviderJoin_DoorsEveryKind and TestRunModelProviderDoors pin both reason-less"},
	// enforceCreateLLMMechanism's one outage arm: an AWS Bedrock SSO renewal
	// that got no answer at all (not a refusal, an absence of one) must not
	// open the console's sign-in door the way a real credential refusal
	// does — TestCreateRun_AnUnansweredRenewalIsRefusedWithoutTheClass pins
	// this 422 carrying no class.
	"runs_dispatch_llm_mechanism.go:Server.enforceCreateLLMMechanism": {1, "4xx (StatusUnprocessableEntity): an unanswered renewal is not a refusal class; TestCreateRun_AnUnansweredRenewalIsRefusedWithoutTheClass pins it reason-less so the sign-in door does not open over \"try again\""},
	// driveBindFailure.write's own bare arm: f.reason=="" is the
	// runner-capabilities-unreadable 503 (driveBindFailureHere); f.silent is
	// either of driveShareBindFailure's two ctx.Err()!=nil arms, which DO
	// set a reason (422) but must not be counted, named or now WIRED as a
	// real refusal — a caller who gave up is not a share that stopped
	// answering, and doing so would make the drive-refusal metric (and, from
	// #656 on, the reason on the wire) mean something different from what it
	// says. See driveBindFailure's own doc comment.
	"user_drives_run.go:driveBindFailure.write": {1, "dynamic status (503 no-reason, or 422 silent): see driveBindFailure's own doc comment for both deliberately-bare arms"},
	// ui.go's serveIndex is NOT the JSON API: it serves the console's own
	// index.html (or 404s via http.NotFound, which carries no body at all,
	// hence no reason to give it). A read failure here has no errorBody to
	// carry a reason in — the response IS the HTML page, or in this one
	// failure arm, plain text — so #656's reason contract, which is about
	// errorBody{error,reason}, does not reach this handler at all.
	"ui.go:Server.serveIndex": {1, "not the JSON API: serves index.html; a read failure here has no errorBody to carry a reason in"},
}

// isHTTPDotError reports whether call is the standard library's
// http.Error(w, msg, status) — the same bare-body shape as writeError, just
// spelled as a SelectorExpr (http.Error) rather than a bare Ident.
func isHTTPDotError(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 3 {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Error"
}

// findBareWriteErrorCalls walks file's top-level function declarations for a
// direct call to the bare, three-argument writeError(w, status, msg) or the
// standard library's http.Error(w, msg, status) — never writeErrorReason,
// writeJSON or a forwarder, all of which already carry (or, for the
// writeServerError family, always attach) a reason — and returns every match
// keyed "file:enclosing-symbol" -> each site's trimmed source line,
// mirroring scanFileForLeaks' shape and stability rationale
// (server_error_driver_text_guard_test.go).
func findBareWriteErrorCalls(fset *token.FileSet, name string, file *ast.File, src []byte) map[string][]string {
	found := map[string][]string{}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		symbol := enclosingSymbolName(fd) // "Type.Method" for any receiver, e.g. driveBindFailure.write

		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			isBareWriteError := false
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "writeError" && len(call.Args) == 3 {
				isBareWriteError = true
			} else if isHTTPDotError(call) {
				isBareWriteError = true
			}
			if !isBareWriteError {
				return true
			}
			pos := fset.Position(call.Pos())
			key := fmt.Sprintf("%s:%s", name, symbol)
			snippet := strings.TrimSpace(exprSourceLine(src, pos.Line))
			// http.Error's status is its THIRD argument, writeError's SECOND —
			// check whichever this call actually is.
			statusArg := call.Args[1]
			if isHTTPDotError(call) {
				statusArg = call.Args[2]
			}
			if is5xxStatusArg(statusArg) {
				snippet += "  // 5xx"
			}
			found[key] = append(found[key], snippet)
			return true
		})
	}
	return found
}

// TestEveryWriteErrorCallCarriesAReasonOrIsReviewed is #656's repo-wide
// guard: it walks every non-test source file in this package and fails on
// any bare writeError(w, status, msg) or http.Error(w, msg, status) call that
// is not in bareWriteErrorAllowlist AT EXACTLY its pinned count. A new bare
// site — 4xx or 5xx, new function or one more inside an already-allowlisted
// one — must either become writeErrorReason (the answer for nearly every one
// of the ~415+ sites this issue found) or earn its own allowlist entry with
// the same kind of evidence (a pinning test showing the reason is
// DELIBERATELY absent) the entries above carry.
func TestEveryWriteErrorCallCarriesAReasonOrIsReviewed(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("read %s: %v", wd, err)
	}

	found := map[string][]string{}
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
		for k, v := range findBareWriteErrorCalls(fset, name, file, src) {
			found[k] = append(found[k], v...)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned 0 files — the guard's directory listing is wrong")
	}

	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	violations := 0
	for _, k := range keys {
		entry, ok := bareWriteErrorAllowlist[k]
		if !ok {
			for _, snippet := range found[k] {
				violations++
				t.Errorf("%s calls a bare error writer (no reason): %s\n"+
					"use writeErrorReason(w, status, reason, msg) instead, or — only for a "+
					"call site DELIBERATELY answering with no class, proven by its own "+
					"pinning test — add %q to bareWriteErrorAllowlist with that evidence.",
					k, snippet, k)
			}
			continue
		}
		// #656 final review round FIX-2: pinned per site, not skipped whole-hog.
		// An allowlisted function may have EXACTLY its stated number of bare
		// calls — a new one landing beside a reviewed site is still a new,
		// unreviewed site, and must fail exactly like one anywhere else.
		if got := len(found[k]); got != entry.count {
			violations++
			t.Errorf("%s has %d bare error-writer call(s), bareWriteErrorAllowlist pins %d (%s): %v\n"+
				"a count above the pin means a NEW bare site was added beside the reviewed one(s) — "+
				"give it a reason, or review it and update the pinned count with the same evidence.",
				k, got, entry.count, entry.why, found[k])
		}
	}
	for k, entry := range bareWriteErrorAllowlist {
		if _, ok := found[k]; !ok {
			t.Errorf("bareWriteErrorAllowlist has a stale entry %q (%s) — "+
				"the site it names no longer calls a bare error writer; shrink the allowlist by removing it", k, entry.why)
		}
	}
	t.Logf("scanned %d files, %d allowlisted site(s), %d violation(s)", scanned, len(bareWriteErrorAllowlist), violations)
}

// bareErrorBodyAllowlist is TestNoAdHocErrorBodyOrReasonLiteral's own small
// exception list (#656 final review round 2, N2/G4): an errorBody{} composite
// literal built with no Reason: key, because the enclosing function sets
// body.Reason afterward, on every path, before its writeJSON call — a
// construct-then-assign shape the AST check below cannot see is safe without
// tracing data flow, so it is reviewed and pinned here instead. Keyed
// "file:enclosing-symbol", same reason bareWriteErrorAllowlist is.
var bareErrorBodyAllowlist = map[string]string{
	"run_model_provider.go:Server.writeProviderRefusal": "constructs body without Reason, then sets body.Reason on both branches below (credential vs generic bucket) before its one writeJSON call",
}

// findBareErrorBodyLiterals walks file for an errorBody{...} composite
// literal with no Reason: key, keyed "file:enclosing-symbol" -> each site's
// trimmed source line. A literal built with Error/Provider/Kind but no Reason
// reaches the wire with an empty reason exactly like a bare writeError would,
// but escapes that guard entirely since it never calls writeError by name.
func findBareErrorBodyLiterals(fset *token.FileSet, name string, file *ast.File, src []byte) map[string][]string {
	found := map[string][]string{}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		symbol := enclosingSymbolName(fd)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			id, ok := lit.Type.(*ast.Ident)
			if !ok || id.Name != "errorBody" {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Reason" {
					return true // has one, named or not, static or not — not this check's business
				}
			}
			pos := fset.Position(lit.Pos())
			key := fmt.Sprintf("%s:%s", name, symbol)
			found[key] = append(found[key], strings.TrimSpace(exprSourceLine(src, pos.Line)))
			return true
		})
	}
	return found
}

// findAdHocAndDirectReasonLiterals walks file for a writeErrorReason(w,
// status, reason, msg) call whose reason argument is a non-empty string
// literal written directly at the call site, rather than a named constant —
// the shape #656's own convention has always forbidden by review, never by a
// compiler or a test until now. "" is exempt: that is writeError's OWN
// internal forwarding call (http.go), the bare-reason case the OTHER guard,
// TestEveryWriteErrorCallCarriesAReasonOrIsReviewed, already owns.
func findAdHocAndDirectReasonLiterals(fset *token.FileSet, name string, file *ast.File, src []byte) map[string][]string {
	found := map[string][]string{}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		symbol := enclosingSymbolName(fd)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "writeErrorReason" || len(call.Args) != 4 {
				return true
			}
			lit, ok := call.Args[2].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true // an identifier/selector/call — traced back to a const is FIX-1's job, not this check's
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && v == "" {
				return true // writeError's own bare-reason forwarder; the other guard's concern
			}
			pos := fset.Position(call.Pos())
			key := fmt.Sprintf("%s:%s", name, symbol)
			found[key] = append(found[key], strings.TrimSpace(exprSourceLine(src, pos.Line)))
			return true
		})
	}
	return found
}

// TestNoAdHocErrorBodyOrReasonLiteral is #656's second repo-wide reason guard
// (final review round 2, N2): TestEveryWriteErrorCallCarriesAReasonOrIsReviewed
// only catches a BARE writer (no reason argument at all); it cannot see a
// writer that DOES carry something, but the wrong kind of something — a raw
// string typed at the call site (G5) instead of a name from the closed set,
// or an errorBody{} construction that skips the field a JSON encoder cannot
// tell apart from "reviewed and empty" (G4). Neither shape exists in this
// package today outside bareErrorBodyAllowlist's one reviewed exception;
// this guard is what keeps it that way.
func TestNoAdHocErrorBodyOrReasonLiteral(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("read %s: %v", wd, err)
	}

	bareBodies := map[string][]string{}
	adHocLiterals := map[string][]string{}
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
		for k, v := range findBareErrorBodyLiterals(fset, name, file, src) {
			bareBodies[k] = append(bareBodies[k], v...)
		}
		for k, v := range findAdHocAndDirectReasonLiterals(fset, name, file, src) {
			adHocLiterals[k] = append(adHocLiterals[k], v...)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned 0 files — the guard's directory listing is wrong")
	}

	var keys []string
	for k := range bareBodies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := bareErrorBodyAllowlist[k]; ok {
			continue
		}
		for _, snippet := range bareBodies[k] {
			t.Errorf("%s builds an errorBody{} with no Reason: key: %s\n"+
				"set Reason (directly, or by assignment before every writeJSON call), or — only for a "+
				"reviewed construct-then-assign shape — add %q to bareErrorBodyAllowlist with that evidence.",
				k, snippet, k)
		}
	}
	for k, why := range bareErrorBodyAllowlist {
		if _, ok := bareBodies[k]; !ok {
			t.Errorf("bareErrorBodyAllowlist has a stale entry %q (%s) — "+
				"the site it names no longer builds a Reason-less errorBody{}; shrink the allowlist by removing it", k, why)
		}
	}

	var litKeys []string
	for k := range adHocLiterals {
		litKeys = append(litKeys, k)
	}
	sort.Strings(litKeys)
	for _, k := range litKeys {
		for _, snippet := range adHocLiterals[k] {
			t.Errorf("%s passes writeErrorReason a string literal directly: %s\n"+
				"name a constant from reasons.go/reasons_routes.go (or an authz.Reason) instead — "+
				"a hand-typed reason is not part of the documented closed set and TestReasonDocsMatchReasonsGo cannot see it.",
				k, snippet)
		}
	}
	t.Logf("scanned %d files, %d bare-errorBody allowlisted site(s), 0 ad-hoc-literal exceptions", scanned, len(bareErrorBodyAllowlist))
}

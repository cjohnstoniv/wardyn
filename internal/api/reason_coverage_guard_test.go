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
	"slices"
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
// sites it covers: an earlier version of this map keyed by name alone, so
// `if _, ok := ...; ok { continue }` skipped every bare call inside an
// allowlisted function — a NEW bare site landing inside one went undetected,
// demonstrated by adding a 4th bare call to enforceRunModelProvider and
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
	// handleRecordWorkspace's two: the record door's answers for the same unreadable
	// provider block and unreadable credential as enforceRunModelProvider's two
	// sites above — the bare 503 sentences, byte-equal to create's.
	// TestRecordDoorAnswersTheProviderRefusalAsCreateDoes pins both against the
	// create door's body.
	"record.go:Server.handleRecordWorkspace": {2, "5xx (StatusServiceUnavailable): the create door's reason-less unreadable-provider-block sentence (mpRunUnreadable); TestRecordDoorAnswersTheProviderRefusalAsCreateDoes pins the two byte-equal"},
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
		// Pinned per site, not skipped whole-hog: an allowlisted function may
		// have EXACTLY its stated number of bare calls — a new one landing
		// beside a reviewed site is still a new, unreviewed site, and must
		// fail exactly like one anywhere else.
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
// exception list: an errorBody{} composite literal built with no Reason: key,
// because the enclosing function sets body.Reason afterward, on every path,
// before its writeJSON call — a construct-then-assign shape the AST check
// below cannot see is safe without tracing data flow, so it is reviewed and
// pinned here instead. Keyed "file:enclosing-symbol", same reason
// bareWriteErrorAllowlist is, and pinned to an exact count for the same
// reason: a second Reason-less literal beside the reviewed one is a new,
// unreviewed site.
var bareErrorBodyAllowlist = map[string]bareWriteErrorEntry{
	"run_model_provider.go:Server.writeProviderRefusalAs": {1, "constructs body without Reason, then sets body.Reason on both branches below (credential vs generic bucket) before its one writeJSON call"},
}

// reasonConstOutsideReasonFilesAllowlist is the string consts declared outside
// reasonsGoFiles that are reviewed as NOT wire reasons, although they are
// reason-named or reach a reason position (writeErrorReason's argument,
// errorBody.Reason). Keyed "file:const". A wire reason declared beside its lane
// instead of in the closed set is invisible to TestReasonDocsMatchReasonsGo,
// which reads only reasonsGoFiles.
var reasonConstOutsideReasonFilesAllowlist = map[string]string{
	"runs_create_requirements.go:reasonRequirementModelHost":        "the audit data.reason of a run.requirement.skip entry, never an errorBody reason",
	"runs_create_requirements.go:reasonRequirementModelHostUnknown": "the audit data.reason of a run.requirement.skip entry, never an errorBody reason",
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

// topLevelStringConsts returns the name of every top-level const in file
// whose value is a string literal.
func topLevelStringConsts(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						names = append(names, id.Name)
					}
				}
			}
		}
	}
	return names
}

// findReasonConstsOutsideReasonFiles walks file's top-level consts for a
// string const named reason* whose value has a wire reason's own shape
// (reasonWireValueShape), keyed "file:const" -> its trimmed source line. It
// catches a declaration the use-site check cannot: one reached through a local
// variable, or not used yet. Prose consts that merely start with "reason"
// (harness.go's explanations) fail the shape test and are not reasons.
func findReasonConstsOutsideReasonFiles(fset *token.FileSet, name string, file *ast.File, src []byte) map[string]string {
	found := map[string]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if i >= len(vs.Values) || !strings.HasPrefix(id.Name, "reason") {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if v, err := strconv.Unquote(lit.Value); err == nil && reasonWireValueShape.MatchString(v) {
					found[name+":"+id.Name] = strings.TrimSpace(exprSourceLine(src, fset.Position(id.Pos()).Line))
				}
			}
		}
	}
	return found
}

// isNonEmptyStringLit reports whether e is a string literal other than "".
func isNonEmptyStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err != nil || v != ""
}

// reasonIdentUse is a bare identifier (or string(ident)) sitting in a reason
// position; the guard resolves it to its const's declaring file afterwards,
// once every file has been read.
type reasonIdentUse struct {
	ident, symbolKey, snippet string
}

// isErrorBodyValue reports whether e is errorBody{...} or &errorBody{...}.
func isErrorBodyValue(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	id, ok := lit.Type.(*ast.Ident)
	return ok && id.Name == "errorBody"
}

// isErrorBodyType reports whether e is the type errorBody or *errorBody.
func isErrorBodyType(e ast.Expr) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "errorBody"
}

// errorBodyVars returns the names fd binds to an errorBody: parameters,
// `var x errorBody`, and `x := errorBody{...}` / `x := &errorBody{...}`. The
// assignment check below is limited to these receivers, so another struct's
// Reason field (a Capability's prose, an Approval's) is not this guard's.
func errorBodyVars(fd *ast.FuncDecl) map[string]bool {
	vars := map[string]bool{}
	if fd.Type.Params != nil {
		for _, f := range fd.Type.Params.List {
			if isErrorBodyType(f.Type) {
				for _, n := range f.Names {
					vars[n.Name] = true
				}
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(n.Rhs) && isErrorBodyValue(n.Rhs[i]) {
					vars[id.Name] = true
				}
			}
		case *ast.ValueSpec:
			if n.Type != nil && isErrorBodyType(n.Type) {
				for _, id := range n.Names {
					vars[id.Name] = true
				}
			}
		}
		return true
	})
	return vars
}

// reasonConstIdent returns the identifier e names as a reason: X, or string(X).
func reasonConstIdent(e ast.Expr) string {
	if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 {
		if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "string" {
			e = call.Args[0]
		}
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// findReasonUses walks file for the three reason positions — the third
// argument of writeErrorReason(w, status, reason, msg), an errorBody{Reason: X}
// composite, and a `body.Reason = X` assignment on an errorBody — the shape
// #656's own convention has always forbidden by review, never by a compiler or
// a test until now. A non-empty string literal there is returned in literals,
// keyed "file:enclosing-symbol"; a bare identifier is returned in idents to be
// resolved against the package's consts. "" is exempt: that is writeError's OWN
// internal forwarding call (http.go), the bare-reason case the OTHER guard,
// TestEveryWriteErrorCallCarriesAReasonOrIsReviewed, already owns.
func findReasonUses(fset *token.FileSet, name string, file *ast.File, src []byte) (literals map[string][]string, idents []reasonIdentUse) {
	literals = map[string][]string{}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		key := fmt.Sprintf("%s:%s", name, enclosingSymbolName(fd))
		bodies := errorBodyVars(fd)
		use := func(e ast.Expr, site ast.Node) {
			snippet := strings.TrimSpace(exprSourceLine(src, fset.Position(site.Pos()).Line))
			if isNonEmptyStringLit(e) {
				literals[key] = append(literals[key], snippet)
			} else if id := reasonConstIdent(e); id != "" {
				idents = append(idents, reasonIdentUse{ident: id, symbolKey: key, snippet: snippet})
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "writeErrorReason" && len(n.Args) == 4 {
					use(n.Args[2], n)
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "errorBody" {
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Reason" {
								use(kv.Value, kv)
							}
						}
					}
				}
			case *ast.AssignStmt:
				if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
					break
				}
				if sel, ok := n.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Reason" {
					if x, ok := sel.X.(*ast.Ident); ok && bodies[x.Name] {
						use(n.Rhs[0], n)
					}
				}
			}
			return true
		})
	}
	return literals, idents
}

// TestNoAdHocErrorBodyOrReasonLiteral is #656's second repo-wide reason
// guard: TestEveryWriteErrorCallCarriesAReasonOrIsReviewed only catches a
// BARE writer (no reason argument at all); it cannot see a writer that DOES
// carry something, but the wrong kind of something — a raw string typed at
// the call site instead of a name from the closed set (as a writeErrorReason
// argument, an errorBody{Reason: "..."} or a Reason assignment), an errorBody{}
// construction that skips the field a JSON encoder cannot tell apart from
// "reviewed and empty", or a reason const declared outside the closed set's
// files where TestReasonDocsMatchReasonsGo cannot see it. None of these shapes
// exists in this package today outside the allowlists' reviewed exceptions;
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
	var identUses []reasonIdentUse
	constFile := map[string]string{}  // top-level string const -> its declaring file
	strayDecls := map[string]string{} // reason-named wire-shaped consts declared outside reasonsGoFiles
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
		lits, idents := findReasonUses(fset, name, file, src)
		for k, v := range lits {
			adHocLiterals[k] = append(adHocLiterals[k], v...)
		}
		identUses = append(identUses, idents...)
		for _, c := range topLevelStringConsts(file) {
			constFile[c] = name
		}
		if !slices.Contains(reasonsGoFiles, name) {
			for k, v := range findReasonConstsOutsideReasonFiles(fset, name, file, src) {
				strayDecls[k] = v
			}
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
		entry, ok := bareErrorBodyAllowlist[k]
		if !ok {
			for _, snippet := range bareBodies[k] {
				t.Errorf("%s builds an errorBody{} with no Reason: key: %s\n"+
					"set Reason (directly, or by assignment before every writeJSON call), or — only for a "+
					"reviewed construct-then-assign shape — add %q to bareErrorBodyAllowlist with that evidence.",
					k, snippet, k)
			}
			continue
		}
		if got := len(bareBodies[k]); got != entry.count {
			t.Errorf("%s builds %d Reason-less errorBody{} literal(s), bareErrorBodyAllowlist pins %d (%s): %v\n"+
				"a count above the pin is a NEW literal beside the reviewed one — give it a Reason, "+
				"or review it and update the pinned count with the same evidence.",
				k, got, entry.count, entry.why, bareBodies[k])
		}
	}
	for k, entry := range bareErrorBodyAllowlist {
		if _, ok := bareBodies[k]; !ok {
			t.Errorf("bareErrorBodyAllowlist has a stale entry %q (%s) — "+
				"the site it names no longer builds a Reason-less errorBody{}; shrink the allowlist by removing it", k, entry.why)
		}
	}

	var litKeys []string
	for k := range adHocLiterals {
		litKeys = append(litKeys, k)
	}
	sort.Strings(litKeys)
	for _, k := range litKeys {
		for _, snippet := range adHocLiterals[k] {
			t.Errorf("%s writes a wire reason as a string literal: %s\n"+
				"name a constant from reasons.go/reasons_routes.go (or an authz.Reason) instead — "+
				"a hand-typed reason is not part of the documented closed set and TestReasonDocsMatchReasonsGo cannot see it.",
				k, snippet)
		}
	}

	// A const in a reason position but declared outside the closed set is a
	// wire reason TestReasonDocsMatchReasonsGo cannot see, whatever it is named.
	flagged := map[string]bool{}
	for _, u := range identUses {
		file, ok := constFile[u.ident]
		if !ok || slices.Contains(reasonsGoFiles, file) {
			continue
		}
		key := file + ":" + u.ident
		flagged[key] = true
		if _, ok := reasonConstOutsideReasonFilesAllowlist[key]; ok {
			continue
		}
		t.Errorf("%s puts %s in a reason position (%s), but %s is declared outside %v\n"+
			"move it into the closed set (and docs/sdk.md) — TestReasonDocsMatchReasonsGo reads only those files — "+
			"or, for a value that is not a wire reason, add %q to reasonConstOutsideReasonFilesAllowlist with why.",
			u.symbolKey, u.ident, u.snippet, u.ident, reasonsGoFiles, key)
		strayDecls[key] = "" // reported here; the declaration check below need not repeat it
	}
	declKeys := make([]string, 0, len(strayDecls))
	for k := range strayDecls {
		declKeys = append(declKeys, k)
	}
	sort.Strings(declKeys)
	for _, k := range declKeys {
		flagged[k] = true
		if _, ok := reasonConstOutsideReasonFilesAllowlist[k]; ok || strayDecls[k] == "" {
			continue
		}
		t.Errorf("%s declares a reason const outside %v: %s\n"+
			"move it into the closed set (and docs/sdk.md) — TestReasonDocsMatchReasonsGo reads only those files — "+
			"or, for a value that is not a wire reason, add %q to reasonConstOutsideReasonFilesAllowlist with why.",
			k, reasonsGoFiles, strayDecls[k], k)
	}
	for k, why := range reasonConstOutsideReasonFilesAllowlist {
		if !flagged[k] {
			t.Errorf("reasonConstOutsideReasonFilesAllowlist has a stale entry %q (%s) — "+
				"the const is gone, moved, or no longer reason-named or used as a reason; shrink the allowlist by removing it", k, why)
		}
	}
	t.Logf("scanned %d files, %d bare-errorBody and %d stray-const allowlisted site(s), 0 ad-hoc-literal exceptions",
		scanned, len(bareErrorBodyAllowlist), len(reasonConstOutsideReasonFilesAllowlist))
}

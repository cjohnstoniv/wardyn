// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
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
// The three doors share one fold (runFoldSteps, run_fold.go), so most of that
// prefix is the fold's. The scan follows each door's foldRunRequest call into
// the steps its mode runs, and inside a step evaluates every `if f.mode == …`
// for that mode, so a gate a step skips at one door is missing from that door
// exactly as if its handler had dropped it. The fold itself is pinned: its
// steps, their order and their doors (wantRunFoldSteps), the loop that runs
// them, and every place a mode is read.
//
// MintRunIdentity is the boundary marker: "it mints nothing, persists nothing,
// dispatches nothing" is preflight's contract, so everything after the mint is
// launch-only by construction and out of scope. A missing marker fails loudly
// rather than silently widening the scan to the whole handler.

// preflightInlinedWrappers are the launch-side helpers preflight deliberately
// does not call AS A UNIT because it reproduces their gates individually. The
// guard scans THROUGH them: every Server gate they call counts as a launch gate
// in its own right, so each one is either previewed by preflight or named in
// preflightGateExceptions with its own reason.
//
// This is deliberately narrow: one exception per real gap, never one per
// wrapper. A blanket entry for `decodeAndValidateCreateRun` would license the
// whole wrapper — including requestRepoProviderRefusals, which lives inside it —
// and leave this guard structurally unable to say that preflight never calls it.
//
// The value is the file the wrapper is declared in, so a move reds here naming
// the file rather than as a bare parse failure.
var preflightInlinedWrappers = map[string]string{
	"decodeAndValidateCreateRun":  "runs_create_validate.go",
	"denyUserRequest":             "runs_create_validate.go",
	"denyUserGovernance":          "runs_create_validate.go",
	"denyUserRunQuota":            "runs_create_validate.go",
	"denyUserCapability":          "runs_create_validate.go",
	"resolveRunPolicy":            "inline_policy.go",
	"boundUserSpec":               "inline_policy.go",
	"validateRunTextFields":       "runs_create_fields.go",
	"requestRepoProviderRefusals": "workspace_admission.go",
	"admitRepoSources":            "workspace_admission.go",
	"denyUserWorkspaceProviders":  "workspace_providers.go",
	"seedAndAdmitWorkspace":       "runs.go",
	"seedRequestDrive":            "user_drives_run.go",
	"enforceRunModelProvider":     "run_model_provider.go",
	"authorizePreviewRequest":     "policy_preview.go",
	"runRequestGovernance":        "run_request_governance.go",
	"resolveRunPolicyFacts":       "run_policy_resolution.go",
	"boundRunUserSpec":            "run_policy_resolution.go",
	"seedAuthorizedWorkspace":     "run_workspace_authorization.go",
	"authorizeRequestDrive":       "run_drive_authorization.go",
	"authorizeRunModelProvider":   "run_provider_authorization.go",
}

var preflightGateExceptions = map[string]string{
	// Inside decodeAndValidateCreateRun. Preflight reaches the identical check
	// through seedAndAdmitWorkspace, which re-runs it on the POST-SEED request —
	// a workspace's base_image can set req.Image after the wrapper ran, so that
	// is the stricter of the two calls and the one Review must make.
	"validateImageBuildRequest": "preflight runs the same check post-seed inside seedAndAdmitWorkspace (runs.go), which is the stricter call",
	// Preflight runs the shared enforcedConfinement math directly in
	// stepConfinement and deliberately skips this wrapper's TAIL gates (runner
	// capability, the cloud_sts grantChecker): deriveSetupItems' backend row
	// reports an unenforceable class as a fixable checklist row instead of a
	// fatal error that blanks the Review panel.
	"resolveEnforcedConfinement": "preflight calls enforcedConfinement directly; the runner-capability + cloud_sts tail gates are reported by the checklist instead (doc comment)",
}

// preflightGateExceptionsMax caps preflightGateExceptions, which may only
// shrink or stay (authorization-kernel design G3). Lower it when an entry
// goes; raising it needs a reviewed reason.
const preflightGateExceptionsMax = 2

var policyPreviewGateExceptions = map[string]string{
	"runQuotaRefusal":            "preview does not count this member's runs",
	"gitCredentialRefusal":       "preview never reads repository credential values; credential_liveness is pending",
	"driveMountFor":              "preview checks read-only narrowing separately; runner and share readiness stay pending",
	"resolveEnforcedConfinement": "preview runs the same static floor math; runner resolution stays pending",
	"providerLiveness":           "preview authorizes the selection; credential values and renewal are forbidden",
	"writeProviderChoiceRefusal": "credential liveness refusals belong only to create and preflight",
	"resolveRunAutonomy":         "credential-dependent autonomy and tool approvals stay pending",
	"admitHostCapacity":          "preview does not probe host capacity",
	"refuseRunCapFull":           "preview does not count runs or enforce quota",
	"refuseRunFit":               "preview does not read Kubernetes quota",
	"runFitSpec":                 "preview does not resolve a dispatch fit request",
}

const policyPreviewGateExceptionsMax = 11

// wantRunFoldSteps pins runFoldSteps: every step, in order, with the doors it
// runs at. A lane adding a gate adds its step here in the same commit; a step
// deleted, reordered or moved off a door reds here.
var wantRunFoldSteps = []pinnedStep{
	{"stepRunContract", foldCreate | foldPreflight | foldPreview},
	{"stepPolicy", foldCreate | foldPreflight | foldPreview},
	{"stepSeedWorkspace", foldCreate | foldPreflight | foldPreview},
	{"stepDrive", foldCreate | foldPreflight | foldPreview},
	{"stepWorkspaces", foldCreate | foldPreflight | foldPreview},
	{"stepPreviewEgress", foldPreflight | foldPreview},
	{"stepRequirements", foldCreate | foldPreflight | foldPreview},
	{"stepGitHubEgress", foldCreate | foldPreflight | foldPreview},
	{"stepComponents", foldCreate | foldPreflight | foldPreview},
	{"stepConfinement", foldCreate | foldPreflight | foldPreview},
	{"stepModelProvider", foldCreate | foldPreflight | foldPreview},
	{"stepAutonomy", foldCreate | foldPreflight | foldPreview},
	{"stepADOStanding", foldCreate | foldPreflight | foldPreview},
	{"stepPATNarrowing", foldPreflight | foldPreview},
	{"stepHostCapacity", foldCreate | foldPreflight},
	{"stepRunCap", foldCreate | foldPreflight},
	{"stepRunFit", foldCreate | foldPreflight},
}

type pinnedStep struct {
	step  string
	modes foldMode
}

var foldModeNames = map[string]foldMode{"foldCreate": foldCreate, "foldPreflight": foldPreflight, "foldPreview": foldPreview}

func TestPreflightMirrorsLaunchGates(t *testing.T) {
	fset := token.NewFileSet()
	g := &gateScan{t: t, fset: fset, steps: runFoldStepDecls(t, fset), consumed: map[*ast.SelectorExpr]bool{}, nested: map[token.Pos]bool{}}
	t.Run("fold", func(t *testing.T) { checkFoldShape(t, fset, g.steps) })

	create := parseHandler(t, fset, "runs.go", "handleCreateRun")
	mint := serverCallPos(create, "MintRunIdentity")
	if mint == token.NoPos {
		t.Fatal("handleCreateRun lost MintRunIdentity: re-point the dry-run boundary")
	}
	if fold := g.foldCall(create, foldCreate); fold != nil && fold.Pos() >= mint {
		t.Error("handleCreateRun folds after MintRunIdentity: every gate must refuse before the mint")
	}
	launch := g.calls(create, mint)
	for _, door := range []struct {
		name, file string
		mode       foldMode
		exceptions map[string]string
		cap        int
	}{
		{"handlePreflightRun", "preflight.go", foldPreflight, preflightGateExceptions, preflightGateExceptionsMax},
		{"handlePolicyPreview", "policy_preview.go", foldPreview, policyPreviewGateExceptions, policyPreviewGateExceptionsMax},
	} {
		t.Run(door.name, func(t *testing.T) {
			dry := parseHandler(t, fset, door.file, door.name)
			g.foldCall(dry, door.mode)
			preview := g.calls(dry, token.NoPos)
			for _, gate := range launch {
				if !slices.Contains(preview, gate) && door.exceptions[gate] == "" {
					t.Errorf("%s does not reproduce launch gate %s", door.name, gate)
				}
			}
			if len(door.exceptions) > door.cap {
				t.Errorf("%s has %d exceptions, cap %d", door.name, len(door.exceptions), door.cap)
			}
			for gate := range door.exceptions {
				if !slices.Contains(launch, gate) {
					t.Errorf("%s exception %s no longer names a launch gate", door.name, gate)
				}
			}
			shared := func(a, b []string) []string {
				return slices.DeleteFunc(slices.Clone(a), func(gate string) bool { return !slices.Contains(b, gate) || door.exceptions[gate] != "" })
			}
			if a, b := shared(launch, preview), shared(preview, launch); !slices.Equal(a, b) {
				t.Errorf("%s shared gate order differs: launch %v; dry run %v", door.name, a, b)
			}
		})
	}
	// Every door has now walked every step in each of its modes.
	for _, st := range g.steps {
		ast.Inspect(st.decl.Body, func(n ast.Node) bool {
			if sel := modeSelector(n); sel != nil && !g.consumed[sel] {
				t.Errorf("%s reads f.mode at %s in a form this guard cannot evaluate: use `if f.mode == foldX` / `!=`, or a flag argument", st.name, fset.Position(sel.Pos()))
			}
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "mode" && modeSelector(sel) == nil {
				t.Errorf("%s reads a mode at %s through something other than f", st.name, fset.Position(sel.Pos()))
			}
			return true
		})
	}
}

type foldStepDecl struct {
	name  string
	modes foldMode
	decl  *ast.FuncDecl
}

// runFoldStepDecls reads runFoldSteps itself, not its source: the names and
// doors the loop will actually run, each with the declaration to scan.
func runFoldStepDecls(t *testing.T, fset *token.FileSet) []foldStepDecl {
	t.Helper()
	var out []foldStepDecl
	for _, st := range runFoldSteps {
		full := runtime.FuncForPC(reflect.ValueOf(st.run).Pointer()).Name()
		name := full[strings.LastIndex(full, ".")+1:]
		if !strings.Contains(full, "(*Server)."+name) {
			t.Fatalf("runFoldSteps entry %s is not a (*Server) method expression", full)
		}
		out = append(out, foldStepDecl{name, st.modes, parseHandler(t, fset, "run_fold_steps.go", name)})
	}
	return out
}

// checkFoldShape pins the table, the three doors' distinct bits, the loop that
// runs the table, and that no helper outside it can branch on a mode.
func checkFoldShape(t *testing.T, fset *token.FileSet, steps []foldStepDecl) {
	if foldCreate&foldPreflight != 0 || foldCreate&foldPreview != 0 || foldPreflight&foldPreview != 0 || foldCreate == 0 || foldPreflight == 0 || foldPreview == 0 {
		t.Fatalf("fold modes must be distinct bits: %b %b %b", foldCreate, foldPreflight, foldPreview)
	}
	got := make([]string, 0, len(steps))
	for _, st := range steps {
		got = append(got, st.name)
		if i := slices.IndexFunc(wantRunFoldSteps, func(w pinnedStep) bool { return w.step == st.name }); i >= 0 && wantRunFoldSteps[i].modes != st.modes {
			t.Errorf("%s runs at doors %03b, pinned %03b", st.name, st.modes, wantRunFoldSteps[i].modes)
		}
	}
	want := make([]string, 0, len(wantRunFoldSteps))
	for _, w := range wantRunFoldSteps {
		want = append(want, w.step)
	}
	if !slices.Equal(got, want) {
		t.Errorf("runFoldSteps = %v, pinned %v", got, want)
	}

	fold := parseHandler(t, fset, "run_fold.go", "foldRunRequest")
	modeUses, ranges, returns, tableLoop := 0, 0, 0, false
	ast.Inspect(fold.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			ast.Inspect(n.Value, func(v ast.Node) bool {
				if id, ok := v.(*ast.Ident); ok && id.Name == "mode" {
					modeUses++
				}
				return true
			})
			return false
		case *ast.Ident:
			if n.Name == "mode" {
				modeUses++
			}
		case *ast.BranchStmt:
			t.Errorf("foldRunRequest has a %s at %s: every step whose doors include the mode must run", n.Tok, fset.Position(n.Pos()))
		case *ast.ReturnStmt:
			returns++
		case *ast.RangeStmt:
			ranges++
			id, ok := n.X.(*ast.Ident)
			tableLoop = ok && id.Name == "runFoldSteps"
		}
		return true
	})
	// Two: the runFold field and `step.modes&mode`.
	if modeUses != 2 || ranges != 1 || !tableLoop {
		t.Errorf("foldRunRequest must range over runFoldSteps once and read mode only for the field and the door test (mode uses %d, ranges %d)", modeUses, ranges)
	}
	// The step's refusal and the success: a data-conditional return in the loop
	// would skip every later gate at every door.
	if returns != 2 {
		t.Errorf("foldRunRequest has %d returns, want exactly 2 (a step answered; every step ran)", returns)
	}

	names := []string{"foldRunRequest"}
	for _, st := range steps {
		names = append(names, st.name)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && takesFold(fn) && !slices.Contains(names, fn.Name.Name) {
				t.Errorf("%s: %s takes a runFold or a foldMode but is not a runFoldSteps step: a mode branch there is invisible to this guard", file, fn.Name.Name)
			}
		}
	}
}

func takesFold(fn *ast.FuncDecl) bool {
	var fields []*ast.Field
	if fn.Recv != nil {
		fields = append(fields, fn.Recv.List...)
	}
	fields = append(fields, fn.Type.Params.List...)
	for _, field := range fields {
		typ := field.Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		if id, ok := typ.(*ast.Ident); ok && (id.Name == "runFold" || id.Name == "foldMode") {
			return true
		}
	}
	return false
}

// gateScan collects the Server methods a door calls, in first-call order.
type gateScan struct {
	t        *testing.T
	fset     *token.FileSet
	steps    []foldStepDecl
	consumed map[*ast.SelectorExpr]bool // every f.mode the mode walk has evaluated
	step     string                     // the step being walked, for messages
	nested   map[token.Pos]bool         // mode tests already reported as nested
}

// foldCall returns the door's one foldRunRequest call and fails unless it is
// exactly one, for this door's mode.
func (g *gateScan) foldCall(fn *ast.FuncDecl, mode foldMode) *ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && serverMethodName(call) == "foldRunRequest" {
			calls = append(calls, call)
		}
		return true
	})
	if len(calls) != 1 || foldCallMode(calls[0]) != mode {
		g.t.Errorf("%s must call foldRunRequest exactly once with its own mode %03b (calls: %d)", fn.Name.Name, mode, len(calls))
		return nil
	}
	return calls[0]
}

func foldCallMode(call *ast.CallExpr) foldMode {
	var mode foldMode
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok {
			mode |= foldModeNames[id.Name]
		}
	}
	return mode
}

// calls returns the Server methods fn calls before `before`. Registered
// wrappers are inlined so extracting a gate cannot hide it, and a
// foldRunRequest call expands to the steps its mode runs.
func (g *gateScan) calls(fn *ast.FuncDecl, before token.Pos) []string {
	var out []string
	add := func(name string) {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && (before == token.NoPos || call.Pos() < before) {
			g.addCall(call, 0, add)
		}
		return true
	})
	return out
}

func (g *gateScan) addCall(call *ast.CallExpr, mode foldMode, add func(string)) {
	name := serverMethodName(call)
	if mode != 0 {
		for _, arg := range call.Args {
			g.modeTest(arg, mode) // a flag argument: as visible as a literal true/false
		}
	}
	switch file := preflightInlinedWrappers[name]; {
	case name == "foldRunRequest":
		m := foldCallMode(call)
		for _, st := range g.steps {
			if st.modes&m != 0 {
				g.step = st.name
				g.walkBlock(st.decl.Body.List, m, true, add)
			}
		}
	case file != "":
		for _, inner := range g.calls(parseHandler(g.t, g.fset, file, name), token.NoPos) {
			add(inner)
		}
	case name != "":
		add(name)
	}
}

// walkBlock walks a step body as mode would run it and reports whether the
// block always returns. top is true while every statement walked so far runs
// unconditionally apart from mode tests: a mode test anywhere else is under a
// data condition, whose return the walk cannot see.
func (g *gateScan) walkBlock(list []ast.Stmt, mode foldMode, top bool, add func(string)) bool {
	for _, st := range list {
		if g.walkStmt(st, mode, top, add) {
			return true
		}
	}
	return false
}

func (g *gateScan) walkStmt(st ast.Stmt, mode foldMode, top bool, add func(string)) bool {
	switch st := st.(type) {
	case *ast.IfStmt:
		g.inspect(st.Init, mode, add)
		if taken, ok := g.modeTest(st.Cond, mode); ok {
			if !top && !g.nested[st.Cond.Pos()] {
				g.nested[st.Cond.Pos()] = true
				g.t.Errorf("%s: mode test nested under a data condition at %s; hoist it to the step's top level", g.step, g.fset.Position(st.Cond.Pos()))
			}
			if taken {
				return g.walkBlock(st.Body.List, mode, top, add)
			}
			if blk, isBlock := st.Else.(*ast.BlockStmt); isBlock {
				return g.walkBlock(blk.List, mode, top, add)
			}
			return st.Else != nil && g.walkStmt(st.Else, mode, top, add)
		}
		g.inspect(st.Cond, mode, add)
		g.walkBlock(st.Body.List, mode, false, add)
		if st.Else != nil {
			g.walkStmt(st.Else, mode, false, add)
		}
		return false
	case *ast.BlockStmt:
		return g.walkBlock(st.List, mode, false, add)
	case *ast.ReturnStmt:
		g.inspect(st, mode, add)
		return true
	}
	g.inspect(st, mode, add)
	return false
}

func (g *gateScan) inspect(n ast.Node, mode foldMode, add func(string)) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			g.addCall(call, mode, add)
		}
		return true
	})
}

// modeTest evaluates `f.mode == foldX` or `!=` for mode; ok is false for
// any other expression.
func (g *gateScan) modeTest(e ast.Expr, mode foldMode) (taken, ok bool) {
	be, ok := e.(*ast.BinaryExpr)
	if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
		return false, false
	}
	sel := modeSelector(be.X)
	id, ok := be.Y.(*ast.Ident)
	if sel == nil || !ok || foldModeNames[id.Name] == 0 {
		return false, false
	}
	g.consumed[sel] = true
	return (mode == foldModeNames[id.Name]) == (be.Op == token.EQL), true
}

func modeSelector(n ast.Node) *ast.SelectorExpr {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "mode" {
		return nil
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "f" {
		return sel
	}
	return nil
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

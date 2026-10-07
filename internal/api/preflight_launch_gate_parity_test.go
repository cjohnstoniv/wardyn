// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
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
	"denyUserSeededImage":         "runs_create_validate.go",
	"resolveRunPolicy":            "inline_policy.go",
	"boundUserSpec":               "inline_policy.go",
	"validateRunTextFields":       "runs_create_fields.go",
	"requestRepoProviderRefusals": "workspace_admission.go",
	"admitRepoSources":            "workspace_admission.go",
	"denyUserWorkspaceProviders":  "workspace_providers.go",
	"seedAndAdmitWorkspace":       "runs.go",
	"getWorkspaceLaunchable":      "helpers.go",
	"seedRequestDrive":            "user_drives_run.go",
	"denyUserDrive":               "user_drives_run.go",
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
	// Preflight calls the shared enforcedConfinement math directly and
	// deliberately skips this wrapper's TAIL gates (runner capability, the
	// cloud_sts grantChecker): deriveSetupItems' backend row reports an
	// unenforceable class as a fixable checklist row instead of a fatal error
	// that blanks the Review panel.
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

func TestPreflightMirrorsLaunchGates(t *testing.T) {
	for _, door := range []struct {
		name, file string
		exceptions map[string]string
		cap        int
	}{
		{"handlePreflightRun", "preflight.go", preflightGateExceptions, preflightGateExceptionsMax},
		{"handlePolicyPreview", "policy_preview.go", policyPreviewGateExceptions, policyPreviewGateExceptionsMax},
	} {
		t.Run(door.name, func(t *testing.T) {
			fset := token.NewFileSet()
			create := parseHandler(t, fset, "runs.go", "handleCreateRun")
			dry := parseHandler(t, fset, door.file, door.name)
			mint := serverCallPos(create, "MintRunIdentity")
			if mint == token.NoPos {
				t.Fatal("handleCreateRun lost MintRunIdentity: re-point the dry-run boundary")
			}
			launch := orderedServerCalls(t, fset, create, mint)
			preview := orderedServerCalls(t, fset, dry, token.NoPos)
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
}

// orderedServerCalls is serverCalls in source order, each name at its FIRST
// call, with each preflightInlinedWrappers call replaced in place by the
// wrapper's own calls (the order launch actually meets them in).
func orderedServerCalls(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl, before token.Pos) []string {
	t.Helper()
	var out []string
	add := func(name string) {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || (before != token.NoPos && call.Pos() >= before) {
			return true
		}
		name := serverMethodName(call)
		if file, inlined := preflightInlinedWrappers[name]; inlined {
			for _, inner := range orderedServerCalls(t, fset, parseHandler(t, fset, file, name), token.NoPos) {
				add(inner)
			}
		} else if name != "" {
			add(name)
		}
		return true
	})
	return out
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

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This guard answers one question: which RunPolicySpec fields does anything
// actually bound?
//
// Reading two files side by side is the only other way to find out, and that
// is how a field slips through: max_holds and first_use_hold_seconds, left
// unbounded by both doors, would let a member author a million held
// goroutines; workspace_repos passes the composer clamp untouched while its
// sibling workspace_mounts is dropped entirely, which has to read as a
// decision rather than an omission; git_push_any_branch is clamped as a
// privilege and graded nowhere.
//
// So the table below is the answer, in code: every field of RunPolicySpec
// carries a row saying whether validatePolicySpec bounds it, whether
// composer.Clamp bounds it, and why. A new field with no row reds this test,
// and so does a row whose claim has drifted from the source — including the
// pass-through claims, which is the point: a field that is deliberately not
// clamped must go on saying so out loud.
//
// It reads the SOURCE rather than exercising behaviour deliberately. Behaviour
// for each door is pinned by that door's own tests; what has no other home is
// the census itself, and a census is a claim about which code exists.
type policyFieldCoverage struct {
	// validated: internal/api/policy.go's validatePolicySpec lane names it.
	validated bool
	// clamped: internal/composer/clamp.go's Clamp names it.
	clamped bool
	why     string
}

var runPolicySpecCoverage = map[string]policyFieldCoverage{
	"AllowedDomains": {validated: true, clamped: true,
		why: "per-entry shape (proxy.ValidDomainEntry) plus a count cap; intersected down to the operator ceiling"},
	"DeniedDomains": {validated: true, clamped: true,
		why: "per-entry shape; the ceiling's denies are unioned in (a deny only ever widens)"},
	"AllowAllEgress": {validated: false, clamped: true,
		why: "a bool: nothing bounds it at write (validateLLMInspection only READS it to decide whether a sidecar URL's host is allowed). Documented inert under allow-all; forced false when the ceiling is not allow-all"},
	"FirstUseApproval": {validated: true, clamped: true,
		why: "closed enum; raised to the ceiling's stricter mode by firstUseApprovalRank"},
	"FirstUseHoldSeconds": {validated: true, clamped: true,
		why: "bounded non-negative and <= maxFirstUseHoldSeconds here, and clamped AGAIN in the proxy's configureHold for a stored policy that never re-crosses this door. composer.Clamp also lowers it to the ceiling's own value (normFirstUseHold), as meet and Leq read it"},
	"MaxHolds": {validated: true, clamped: true,
		why: "same doors, same reason, and lowered to the ceiling's own value by composer.Clamp (normMaxHolds). max_holds is a channel capacity in the proxy, so the cap is a resource bound as well as a ceiling"},
	"AllowedMethods": {validated: true, clamped: true,
		why: "a blank entry is refused; intersected down to the ceiling's method list. An unrecognised method matches no request and so denies rather than widens — it can only ever narrow this run"},
	"MinConfinementClass": {validated: true, clamped: true,
		why: "required and must be a known class; raised to the ceiling's floor"},
	"EligibleGrants": {validated: true, clamped: true,
		why: "per-grant scope shape + lane exclusivity; kinds the ceiling omits are dropped, github permissions intersected, TTL capped, approval forced"},
	"AutoStopAfterSec": {validated: false, clamped: true,
		why: "capped at the ceiling's maximum (0 and negative both rank as never-reap and are capped down too). No absolute bound to validate: never-reap is a legitimate posture for an interactive run, and composer.Grade is what tells the human it was chosen"},
	"WorkspaceMounts": {validated: true, clamped: true,
		why: "runner.ValidateMount plus the unique-target invariant; DROPPED entirely by the clamp — a host bind mount is operator-authored and must never arrive from a composer fed untrusted input"},
	"WorkspaceRepos": {validated: true, clamped: false,
		why: "answered deliberately: runner.ValidateTarget plus the same unique-target invariant, and NOT dropped like its WorkspaceMounts sibling. A repo is cloned into the sandbox rather than bound to a host path, so it carries no host-filesystem authority to drop; the authority it does carry is the workspace ONBOARDING check, which narrowUserInlinePolicy applies on the member lane"},
	"LLMInspection": {validated: true, clamped: true,
		why: "mode/marker/sidecar-URL shape; replaced wholesale by the operator's configured mode, or cleared when the operator configures none"},
	"UIApps": {validated: true, clamped: true,
		why: "name/port/path shape plus a count cap; intersected down to the ceiling's (name, port) allowlist"},
	"Resources": {validated: true, clamped: true,
		why: "each set field capped at the ceiling's, with the deployment's default size standing in for an unset ceiling; a negative size is refused (validateResources)"},
	"ToolRules": {validated: true, clamped: true,
		why: "closed effect enum, name charset/length, count cap; a proposal may narrow the ceiling's rules and never widen them"},
	"GitPushAnyBranch": {validated: false, clamped: true,
		why: "forced false unless the ceiling sets it — a boolean with no shape to validate. Grade adds the item so the human is told when it is on"},
	"AzureDevOpsCapabilities": {validated: true, clamped: false,
		why: "each entry must be adoscope-grantable (a 400 with its own reason). Bounded at DISPATCH, not by the clamp, because the bound needs the run's Azure DevOps row: an operator's list must sit inside the row's capability_ceiling; a member's stands only where it meets the row's default_profile or their governance ADO list, from any policy source, and a list meeting nothing refuses the launch (adoStandingBound). The clamp passes it through so an explicit choice never reaches dispatch narrowed to nothing, which would read as the row's default"},
	"GitHubCapabilities": {validated: false, clamped: false,
		why: "AzureDevOpsCapabilities' twin, inert until the GitHub provider row block and the per-person lanes read it. NOT YET validated: the catalogue check (each entry ghscope-grantable) lands in internal/api/policy.go in a later wave, after that file's in-flight edit; until then an unknown entry is stored and matches no capability. Passed through by the clamp for the ADO reason — the bound needs the run's GitHub row, so it is dispatch's"},
	"PushRules": {validated: true, clamped: true,
		why: "#176: deny_paths per-entry byte/charset shape (no count cap — see maxPushRulesPathBytes' own doc for why, same stance as denied_domains), max_inspect_pack_mib range-checked. It only NARROWS what a push may touch, so an operator-silent ceiling leaves a proposal's own push_rules untouched; a ceiling that sets one (PushRulesSpec.IsSet — an all-zero push_rules:{} does not count) is a FLOOR — an unset proposal inherits it wholesale, a set one gets the ceiling's deny_paths unioned in EXACT-STRING (unionPaths, not union — a git path is case-sensitive) and max_inspect_pack_mib capped. composer.Grade adds a medium-risk item when it is set (same PushRulesSpec.IsSet guard) and the run's only git grant is ssh_key (unenforceable, not unsafe). #180: require_review_paths takes deny_paths' per-entry checks and unions the same exact-string way; hold_seconds is range-checked 0..600 (the proxy's maxHoldTimeout, which also clamps it sidecar-side) and clamps to the shorter of two authored holds"},
}

func TestRunPolicySpec_EveryFieldIsBoundedOrDeclaredPassThrough(t *testing.T) {
	// The fields the write-time validators bound, read off the syntax tree: a
	// substring match over the whole file is satisfied by a field mentioned only
	// in a comment, or only READ by a validator for another field's sake.
	validated := writeBoundedFields(t, readRepoFile(t, "internal", "api", "policy.go"))
	// cloneProposal is the clamp's OWNERSHIP copy (it reallocates the caller's
	// slices and pointees so the clamped spec aliases nothing). It names EVERY
	// reference-semantics field by construction and bounds none of them, so
	// counting it would force clamped=true on every slice and pointer field and
	// cost this census the distinction it exists to draw — the workspace_repos
	// pass-through row below would become indistinguishable from its dropped
	// workspace_mounts sibling.
	clampSrc := stripGoComments(t, withoutFunc(t, readRepoFile(t, "internal", "composer", "clamp.go"), "cloneProposal"))

	seen := map[string]bool{}
	for _, f := range reflect.VisibleFields(reflect.TypeOf(types.RunPolicySpec{})) {
		row, ok := runPolicySpecCoverage[f.Name]
		if !ok {
			t.Errorf("RunPolicySpec.%s has no coverage row: say whether validatePolicySpec bounds it, "+
				"whether composer.Clamp bounds it, and why — a field nothing visits is how a gap like this happened", f.Name)
			continue
		}
		seen[f.Name] = true
		if row.why == "" {
			t.Errorf("RunPolicySpec.%s: the row needs a reason, not just two booleans", f.Name)
		}
		if got := validated[f.Name]; got != row.validated {
			t.Errorf("RunPolicySpec.%s: validated=%v in the table, but internal/api/policy.go %s reference it",
				f.Name, row.validated, saysOrNot(got))
		}
		if got := strings.Contains(clampSrc, "."+f.Name); got != row.clamped {
			t.Errorf("RunPolicySpec.%s: clamped=%v in the table, but internal/composer/clamp.go %s reference it",
				f.Name, row.clamped, saysOrNot(got))
		}
	}
	for name := range runPolicySpecCoverage {
		if !seen[name] {
			t.Errorf("coverage row %q names no RunPolicySpec field — the field was renamed or removed", name)
		}
	}
}

// validatorReads are the spec.<Field> selectors a validator reads as inputs to
// another field's check without bounding them: validateLLMInspection tests
// AllowAllEgress and AllowedDomains to decide whether the sidecar URL's host is
// allowed, and bounds neither.
var validatorReads = map[string]map[string]bool{
	"validateLLMInspection": {"AllowAllEgress": true, "AllowedDomains": true},
}

// writeBoundedFields returns the RunPolicySpec fields named by a spec.<Field>
// selector inside one of policy.go's validate* functions, minus validatorReads.
func writeBoundedFields(t *testing.T, src string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "policy.go", src, 0)
	if err != nil {
		t.Fatalf("parse policy.go: %v", err)
	}
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "validate") {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "spec" && !validatorReads[fn.Name.Name][sel.Sel.Name] {
				out[sel.Sel.Name] = true
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatal("found no spec.<Field> selector in policy.go's validate* functions — re-anchor this guard")
	}
	return out
}

// withoutFunc returns src with one top-level func's declaration cut out, so a
// census of "which fields does this file bound" can exclude a helper that
// names a field without bounding it. A missing name is FATAL rather than a
// silent no-op: a renamed helper must red this guard, not quietly restore the
// reference it was excluded for.
func withoutFunc(t *testing.T, src, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name {
			continue
		}
		return src[:fset.Position(fd.Pos()).Offset] + src[fset.Position(fd.End()).Offset:]
	}
	t.Fatalf("func %q is not in the source — this guard's exclusion has gone stale", name)
	return ""
}

func saysOrNot(has bool) string {
	if has {
		return "DOES"
	}
	return "does NOT"
}

// stripGoComments removes every line and block comment from src using the
// real Go tokenizer (go/scanner), so a string literal containing "//" or
// "/*" is never mistaken for one. Comments are replaced with a single space
// (not deleted outright) so a token on either side of a removed comment
// never accidentally merges with its neighbor.
func stripGoComments(t *testing.T, src string) string {
	t.Helper()
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, []byte(src), nil, scanner.ScanComments)

	var out strings.Builder
	prevEnd := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT {
			start := file.Offset(pos)
			out.WriteString(src[prevEnd:start])
			out.WriteByte(' ')
			prevEnd = start + len(lit)
		}
	}
	out.WriteString(src[prevEnd:])
	return out.String()
}

// Negative control: a field named only inside a comment must not count
// as bounded once stripGoComments runs — proving the strip actually closes
// the loophole the guard's own doc comment declares ("It reads the SOURCE
// rather than exercising behaviour deliberately").
func TestStripGoComments_CommentOnlyMentionIsNotCounted(t *testing.T) {
	raw := "package composer\n\n// Ghost is unbounded today; see .GhostField for the plan.\nfunc Clamp() {}\n"
	if !strings.Contains(raw, ".GhostField") {
		t.Fatal("test setup: raw source must contain the substring before stripping")
	}
	if strings.Contains(stripGoComments(t, raw), ".GhostField") {
		t.Fatal("stripGoComments left a comment-only mention in the source — a field named only in a " +
			"comment would still read as bounded")
	}
}

// readRepoFile reads a repo-relative source file for the guards above. The
// package under test sits two levels down from the repo root.
func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Dir(filepath.Dir(wd)) // internal/api -> repo root
	b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(b)
}

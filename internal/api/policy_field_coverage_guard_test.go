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

// This guard closes the question B11b-F2/F10/F11 all landed on from different
// directions: which RunPolicySpec fields does anything actually BOUND?
//
// The answer used to be discoverable only by reading two files side by side,
// and the three findings above are what that costs — max_holds and
// first_use_hold_seconds were visited by neither door until a member could
// author a million held goroutines with them; workspace_repos passes the
// composer clamp untouched while its sibling workspace_mounts is dropped
// entirely, with nothing saying whether that is a decision or an omission;
// git_push_any_branch is clamped as a privilege and graded nowhere.
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
	"AllowAllEgress": {validated: true, clamped: true,
		why: "documented inert under allow-all; forced false when the ceiling is not allow-all"},
	"FirstUseApproval": {validated: true, clamped: true,
		why: "closed enum; raised to the ceiling's stricter mode by firstUseApprovalRank"},
	"FirstUseHoldSeconds": {validated: true, clamped: false,
		why: "B11b-F2: bounded non-negative and <= maxFirstUseHoldSeconds here, and clamped AGAIN in the proxy's configureHold for a stored policy that never re-crosses this door. Not a composer.Clamp field: there is no ceiling to intersect against — the bound is absolute, not per-operator"},
	"MaxHolds": {validated: true, clamped: false,
		why: "B11b-F2: same pair of doors, same reason. max_holds is a channel capacity in the proxy, so the cap is a resource bound rather than a privilege ceiling"},
	"AllowedMethods": {validated: false, clamped: true,
		why: "intersected down to the ceiling's method list. Unvalidated by design: an unrecognised method matches no request and so denies rather than widens — it can only ever narrow this run"},
	"MinConfinementClass": {validated: true, clamped: true,
		why: "required and must be a known class; raised to the ceiling's floor"},
	"EligibleGrants": {validated: true, clamped: true,
		why: "per-grant scope shape + lane exclusivity; kinds the ceiling omits are dropped, github permissions intersected, TTL capped, approval forced"},
	"AutoStopAfterSec": {validated: false, clamped: true,
		why: "capped at the ceiling's maximum (0 and negative both rank as never-reap and are capped down too). No absolute bound to validate: never-reap is a legitimate posture for an interactive run, and composer.Grade is what tells the human it was chosen"},
	"WorkspaceMounts": {validated: true, clamped: true,
		why: "runner.ValidateMount plus the unique-target invariant; DROPPED entirely by the clamp — a host bind mount is operator-authored and must never arrive from a composer fed untrusted input"},
	"WorkspaceRepos": {validated: true, clamped: false,
		why: "B11b-F10, answered deliberately: runner.ValidateTarget plus the same unique-target invariant, and NOT dropped like its WorkspaceMounts sibling. A repo is cloned into the sandbox rather than bound to a host path, so it carries no host-filesystem authority to drop; the authority it does carry is the workspace ONBOARDING check, which narrowMemberInlinePolicy applies on the member lane"},
	"LLMInspection": {validated: true, clamped: true,
		why: "mode/marker/sidecar-URL shape; replaced wholesale by the operator's configured mode, or cleared when the operator configures none"},
	"UIApps": {validated: true, clamped: true,
		why: "name/port/path shape plus a count cap; intersected down to the ceiling's (name, port) allowlist"},
	"Resources": {validated: false, clamped: true,
		why: "each set field capped at the ceiling's, with the platform defaults standing in for an unset ceiling. Unvalidated here: every field is a size the driver defaults and the clamp caps, so there is no shape to reject that the cap does not already bound"},
	"ToolRules": {validated: true, clamped: true,
		why: "closed effect enum, name charset/length, count cap; a proposal may narrow the ceiling's rules and never widen them"},
	"GitPushAnyBranch": {validated: false, clamped: true,
		why: "forced false unless the ceiling sets it — a boolean with no shape to validate. B11b-F11 added the Grade item so the human is told when it is on"},
}

func TestRunPolicySpec_EveryFieldIsBoundedOrDeclaredPassThrough(t *testing.T) {
	// W6-05: strip comments first — a bare substring match is satisfied by a
	// field mentioned only in a comment, which would let a row read "bounded"
	// for a field the source only talks about. See stripGoComments below.
	policySrc := stripGoComments(t, readRepoFile(t, "internal", "api", "policy.go"))
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
				"whether composer.Clamp bounds it, and why — a field nothing visits is how B11b-F2 happened", f.Name)
			continue
		}
		seen[f.Name] = true
		if row.why == "" {
			t.Errorf("RunPolicySpec.%s: the row needs a reason, not just two booleans", f.Name)
		}
		if got := strings.Contains(policySrc, "spec."+f.Name); got != row.validated {
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

// W6-05 negative control: a field named only inside a comment must NOT count
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

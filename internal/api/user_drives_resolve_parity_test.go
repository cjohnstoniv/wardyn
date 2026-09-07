// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDriveDoorsAnswerOverTheSameSentinels is F139's anti-recurrence guard.
//
// F139 was the state collapse itself: three server states the launch path
// refuses with 403, 422 and 500 all reached GET /me as the one
// `{"user_drive":null}` a genuinely unallocated member gets, so the console
// rendered no drive affordance and the member never met the sentence naming
// their remedy. The wire half of that is fixed — `user_drive_unavailable`
// carries a closed token per state, and TestMeUserDrive pins all three arms.
//
// What was NOT fixed is the thing that let it happen: the two switches are kept
// in agreement by a COMMENT. user_drives_resolve.go says of
// driveUnavailableReason, in the imperative, that it "IS writeDriveError'S
// SWITCH, in the same order and over the same sentinels … so a new arm in one is
// a missing arm in the other rather than a silent divergence" — and nothing
// executes that claim. Add a sentinel to writeDriveError alone and every state
// it names collapses back into driveUnavailableUnknown at /me: F139's exact
// defect, one sentinel at a time, with the whole package green.
//
// So the invariant is asserted the way the file states it: mechanically, over
// the source, in order. This is the guard genre cmd/wardynd already uses for the
// same class of hand-maintained parity (envdoc_guard_test.go,
// audit_actions_forward_guard_test.go) and that internal/api uses for the
// launch/preflight gate inventory (TestPreflightMirrorsLaunchGates).
//
// A DELIBERATE divergence is still expressible — it just has to be written down
// here, in driveSentinelDivergence, with the reason. That is the point: the
// asymmetry becomes a decision instead of an omission.
var driveSentinelDivergence = map[string]string{
	// (empty: the two switches are identical today, which is the state the
	// file's own comment describes. An entry here is a licence to diverge and
	// must carry the reason /me may answer for a sentinel the door does not, or
	// the other way round.)
}

func TestDriveDoorsAnswerOverTheSameSentinels(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "user_drives_resolve.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse user_drives_resolve.go: %v", err)
	}

	door := driveSwitchSentinels(t, f, "writeDriveError")
	me := driveSwitchSentinels(t, f, "driveUnavailableReason")

	// SAME SENTINELS, SAME ORDER — the comment's own words. Order matters
	// because both switches are first-match over wrapped errors: an error that
	// wraps two sentinels must be classified identically at both doors, and only
	// the order decides which one wins.
	if len(door) != len(me) {
		t.Errorf("writeDriveError matches %d sentinels %v, driveUnavailableReason matches %d %v",
			len(door), door, len(me), me)
	}
	for i := range door {
		if i >= len(me) {
			break
		}
		if door[i] == me[i] {
			continue
		}
		if reason, ok := driveSentinelDivergence[door[i]]; ok {
			t.Logf("declared divergence at arm %d: %s (%s)", i, door[i], reason)
			continue
		}
		t.Errorf("arm %d: writeDriveError matches %s, driveUnavailableReason matches %s — "+
			"the two answer ONE question at two doors (the launch refusal and what /me says before "+
			"the member tries), and a sentinel named at only one of them is F139 again: the state "+
			"collapses back into %q on the wire and the console renders nothing where the server "+
			"composed a remedy. Add the arm to both, or record the reason in driveSentinelDivergence.",
			i, door[i], me[i], driveUnavailableUnknown)
	}

	// A sentinel the DOOR grew and /me did not is the F139 direction exactly,
	// so it is reported as its own failure rather than only as a length
	// mismatch — the message has to name the arm an author has to go add.
	for _, name := range door {
		if driveSentinelListed(me, name) || driveSentinelDivergence[name] != "" {
			continue
		}
		t.Errorf("writeDriveError refuses %s with its own status and sentence, but driveUnavailableReason "+
			"has no arm for it, so GET /me reports %q for that state — indistinguishable from any other "+
			"unreadable allocation. Give it a token in the closed set beside the others.",
			name, driveUnavailableUnknown)
	}
	// …and the reverse: a token /me can emit for a state the launch path does
	// not refuse in its own shape is a console affordance promising a remedy no
	// door delivers.
	for _, name := range me {
		if driveSentinelListed(door, name) || driveSentinelDivergence[name] != "" {
			continue
		}
		t.Errorf("driveUnavailableReason names %s on the wire, but writeDriveError has no arm for it: /me "+
			"tells the member a remedy the launch door will not corroborate.", name)
	}

	// BOTH KEEP A DEFAULT. The whole four-state argument rests on an
	// unrecognised error still being answerable ("unavailable" / 500) rather
	// than falling through to the zero value, which reads as "you have no
	// drive" for a member who has one.
	for _, fn := range []string{"writeDriveError", "driveUnavailableReason"} {
		if !driveSwitchHasDefault(t, f, fn) {
			t.Errorf("%s's switch has no default arm — an unrecognised error must still be ANSWERED "+
				"(500 at the door, %q on the wire), never silently treated as no drive", fn, driveUnavailableUnknown)
		}
	}
}

// driveSwitchSentinels returns, in source order, the identifier named as the
// second argument of every `errors.Is(err, X)` in the named top-level function's
// switch — i.e. the sentinels that function classifies over.
func driveSwitchSentinels(t *testing.T, f *ast.File, name string) []string {
	t.Helper()
	fn := driveTopLevelFunc(t, f, name)
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Is" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "errors" {
			return true
		}
		if id, ok := call.Args[1].(*ast.Ident); ok {
			out = append(out, id.Name)
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("%s: no errors.Is arms found — this guard reads the two switches from source; "+
			"re-point it at the new shape rather than deleting it", name)
	}
	return out
}

func driveSwitchHasDefault(t *testing.T, f *ast.File, name string) bool {
	t.Helper()
	fn := driveTopLevelFunc(t, f, name)
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if cc, ok := n.(*ast.CaseClause); ok && cc.List == nil {
			found = true
		}
		return !found
	})
	return found
}

func driveTopLevelFunc(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("user_drives_resolve.go: no function %s", name)
	return nil
}

func driveSentinelListed(in []string, name string) bool {
	for _, s := range in {
		if s == name {
			return true
		}
	}
	return false
}

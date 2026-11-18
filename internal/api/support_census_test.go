// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	gotypes "go/types"
	"slices"
	"strings"
	"testing"
)

// The support arm's census. supportMayEnter is what lets a helper through a door of someone else's run, so
// WHICH functions call it is a security decision, not an implementation detail: the first list below is every
// door that may, the second every function that must never. A new door, or a door moved, needs a design change
// (DESIGN-support-sessions.md section 2), not an edit of these lists.

// supportCallList is every function that may call supportMayEnter, by name. Value true is a pending mark: the
// door's lane (support-ssh for sshAuth and sshCurrentKey, support-web for the other three) has not landed, so the
// function may or may not call the arm yet, and the lane deletes its own mark when it lands.
var supportCallList = map[string]bool{
	"handleAttachTicket":       false, // the ticket mint: purpose "" is the terminal, "ui" the UI doors
	"refuseAttachEntry":        false, // the terminal's WebSocket entry, its ticket branch only
	"attachAuthority":          false, // the open terminal's re-check, its ticket branch only
	"handleRunExec":            false, // POST /exec
	"uiTicketMayEnter":         true,  // the UI gateway's ticket entry, inside ta.caller == ""
	"uiSessionStillAuthorized": true,  // the UI session's re-check
	"desktopRun":               true,  // the Files panel
	"sshAuth":                  false, // the SSH gateway's authentication, inside the operator-owned rule's branch
	"sshCurrentKey":            false, // the SSH gateway's re-check of an open connection
}

// supportNeverList is every function that must never call supportMayEnter, nor any function of the call list,
// with why. The cookie lane of refuseAttachEntry is on this list too: it is no function of its own, so the
// block check below holds it.
var supportNeverList = map[string]string{
	"loadTakeover":                `"Needs you" take-over ends the owner's terminal`,
	"handleAttachTakeover":        "terminal take-over",
	"signinRun":                   "a desktop sign-in binds the owner's cloud identity",
	"handleBeginDesktopSignin":    "a desktop sign-in binds the owner's cloud identity",
	"handleShareDesktopSignin":    "a desktop sign-in binds the owner's cloud identity",
	"handleRunSignIn":             "a run sign-in binds the owner's cloud identity",
	"macWindowViewFor":            "needs the owner at their own Mac",
	"handleMacWindow":             "needs the owner at their own Mac",
	"backgroundWatchViewFor":      "background runs get no grant",
	"handleBackgroundWatchTicket": "background runs get no grant",
	"uiBackgroundDoorRefused":     "background runs get no grant",
	"uiBackgroundWatchDenies":     "background runs get no grant",
	"mayPauseDesktopAgent":        "pauses the agent for the owner's sign-in",
	"inboundCaller":               "an inbound app's callers are fixed at create",
	"localAttachCheck":            "local attach is the owner's alone",
}

const supportCensusEdit = "A new door into a run needs a design change (DESIGN-support-sessions.md section 2), not an edit of this list."

// callSites is every call in body to a function or method named one of names (a bare mention is no call).
func callSites(body ast.Node, names ...string) []ast.Node {
	var out []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if slices.Contains(names, name) {
			out = append(out, call)
		}
		return true
	})
	return out
}

func callsAny(body ast.Node, names ...string) bool {
	return len(callSites(body, names...)) > 0
}

// TestSupportCensus_OnlyTheListedDoorsCallTheArm: the functions that call supportMayEnter are inside
// supportCallList, and every name on it that is not pending calls it.
func TestSupportCensus_OnlyTheListedDoorsCallTheArm(t *testing.T) {
	bodies := funcBodies(t, false)
	for _, name := range sortedKeys(bodies) {
		if _, listed := supportCallList[name]; !listed && callsAny(bodies[name], "supportMayEnter") {
			t.Errorf("%s calls supportMayEnter and is not on supportCallList. %s", name, supportCensusEdit)
		}
	}
	for _, name := range sortedKeys(supportCallList) {
		body, ok := bodies[name]
		if !ok {
			t.Errorf("supportCallList names %s, which no longer exists: a rename or a move is a design change too. %s", name, supportCensusEdit)
			continue
		}
		if !supportCallList[name] && !callsAny(body, "supportMayEnter") {
			t.Errorf("%s is on supportCallList without a pending mark and does not call supportMayEnter: a door that stopped asking the arm lets a helper in or out unchecked. %s", name, supportCensusEdit)
		}
	}
}

// TestSupportCensus_NeverListedFunctionsStayOut: every function on supportNeverList exists (a rename fails here
// rather than silently leaving the door unwatched) and calls neither supportMayEnter nor any call-list function.
func TestSupportCensus_NeverListedFunctionsStayOut(t *testing.T) {
	bodies := funcBodies(t, false)
	callList := sortedKeys(supportCallList)
	for _, name := range sortedKeys(supportNeverList) {
		body, ok := bodies[name]
		if !ok {
			t.Errorf("supportNeverList names %s, which no longer exists: a rename moves the guard off the door. %s", name, supportCensusEdit)
			continue
		}
		if callsAny(body, "supportMayEnter") {
			t.Errorf("%s calls supportMayEnter but must never (%s). %s", name, supportNeverList[name], supportCensusEdit)
		}
		for _, door := range callList {
			if callsAny(body, door) {
				t.Errorf("%s calls %s, a door of the support arm, but must never (%s). %s", name, door, supportNeverList[name], supportCensusEdit)
			}
		}
	}
}

// ticketBranch is the first if in body, at any depth, whose init reads the ticket actor
// (`if ta, ok := ticketActorFromContext(...); ok {`): the ticket lane's block.
func ticketBranch(body *ast.BlockStmt) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if stmt, ok := n.(*ast.IfStmt); ok && found == nil && stmt.Init != nil && callsAny(stmt.Init, "ticketActorFromContext") {
			found = stmt
		}
		return found == nil
	})
	return found
}

// callerlessBranch is the first if in body whose condition is `ta.caller == ""`, the UI gateway's rule for a
// ticket that no inbound caller minted.
func callerlessBranch(body *ast.BlockStmt) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if stmt, ok := n.(*ast.IfStmt); ok && found == nil && gotypes.ExprString(stmt.Cond) == `ta.caller == ""` {
			found = stmt
		}
		return found == nil
	})
	return found
}

// notOperatorOwnedBranch is the first if in body whose condition is `reason == "" && !run.OperatorOwned`, sshAuth's
// rule that a fresh admin key opens no shell in a person's run, the only place its support arm may clear the reason.
func notOperatorOwnedBranch(body *ast.BlockStmt) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if stmt, ok := n.(*ast.IfStmt); ok && found == nil && gotypes.ExprString(stmt.Cond) == `reason == "" && !run.OperatorOwned` {
			found = stmt
		}
		return found == nil
	})
	return found
}

// TestSupportCensus_TheArmSitsInsideTheTicketLane: in refuseAttachEntry and attachAuthority every call to the
// arm is inside the block that reads the ticket actor, so the cookie lane (a super admin's own session, which a
// helper never uses) cannot reach it; in uiTicketMayEnter inside the branch for a ticket no inbound caller
// minted; in sshAuth inside the `if reason == "" && !run.OperatorOwned` branch, so no other branch can clear the
// refusal.
func TestSupportCensus_TheArmSitsInsideTheTicketLane(t *testing.T) {
	bodies := funcBodies(t, false)
	for _, tc := range []struct {
		name   string
		branch func(*ast.BlockStmt) *ast.IfStmt
		what   string
	}{
		{"refuseAttachEntry", ticketBranch, "the `if ta, ... := ticketActorFromContext(...)` block"},
		{"attachAuthority", ticketBranch, "the `if ta, ... := ticketActorFromContext(...)` block"},
		{"uiTicketMayEnter", callerlessBranch, "the `if ta.caller == \"\"` branch"},
		{"sshAuth", notOperatorOwnedBranch, "the `if reason == \"\" && !run.OperatorOwned` branch"},
	} {
		body, ok := bodies[tc.name]
		if !ok {
			t.Errorf("%s no longer exists. %s", tc.name, supportCensusEdit)
			continue
		}
		calls := callSites(body, "supportMayEnter")
		if len(calls) == 0 {
			if !supportCallList[tc.name] {
				t.Errorf("%s does not call supportMayEnter. %s", tc.name, supportCensusEdit)
			}
			continue
		}
		branch := tc.branch(body)
		if branch == nil {
			t.Errorf("%s has no %s, which the census holds the arm to. %s", tc.name, tc.what, supportCensusEdit)
			continue
		}
		for _, call := range calls {
			if call.Pos() < branch.Body.Pos() || call.End() > branch.Body.End() {
				t.Errorf("%s calls supportMayEnter outside %s. %s", tc.name, tc.what, supportCensusEdit)
			}
		}
	}
}

// TestSupportCensus_ListsAreDisjoint: no name is on both lists, and the call list holds the design's nine.
func TestSupportCensus_ListsAreDisjoint(t *testing.T) {
	for name := range supportNeverList {
		if _, both := supportCallList[name]; both {
			t.Errorf("%s is on supportCallList and supportNeverList. %s", name, supportCensusEdit)
		}
	}
	if len(supportCallList) != 9 {
		t.Errorf("supportCallList holds %d names, want the design's nine: %s", len(supportCallList), strings.Join(sortedKeys(supportCallList), ", "))
	}
}

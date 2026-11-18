// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"os"
	"slices"
	"strings"
	"testing"
)

// The support arm's census. supportMayEnter is what lets a helper through a door of someone else's run, so
// WHICH functions call it is a security decision, not an implementation detail: the first list below is every
// door that may, the second every function that must never. A new door, or a door moved, needs a design change
// (DESIGN-support-sessions.md section 2), not an edit of these lists.

// supportCallList is every function that may mention supportMayEnter, and must call it, keyed by receiver type
// and name ("Server.sshAuth"), with the door it guards. A mention is a call or a method value, so taking the arm
// as a value and calling it later counts as a caller.
var supportCallList = map[string]string{
	"Server.handleAttachTicket":       "the ticket mint: purpose \"\" is the terminal, \"ui\" the UI doors",
	"Server.refuseAttachEntry":        "the terminal's WebSocket entry, its ticket branch only",
	"Server.attachAuthority":          "the open terminal's re-check, its ticket branch only",
	"Server.handleRunExec":            "POST /exec, and the open command's re-check",
	"Server.uiTicketMayEnter":         "the UI gateway's ticket entry, inside ta.caller == \"\"",
	"Server.uiSessionStillAuthorized": "the UI session's re-check",
	"Server.desktopRun":               "the Files panel",
	"Server.sshAuth":                  "the SSH gateway's authentication, inside the operator-owned rule's branch",
	"Server.sshCurrentKey":            "the SSH gateway's re-check of an open connection",
}

// supportNeverList is every function that must never call supportMayEnter, nor any function of the call list,
// with why. The cookie lane of refuseAttachEntry is on this list too: it is no function of its own, so the
// block check below holds it.
var supportNeverList = map[string]string{
	"Server.loadTakeover":                `"Needs you" take-over ends the owner's terminal`,
	"Server.handleAttachTakeover":        "terminal take-over",
	"Server.signinRun":                   "a desktop sign-in binds the owner's cloud identity",
	"Server.handleBeginDesktopSignin":    "a desktop sign-in binds the owner's cloud identity",
	"Server.handleShareDesktopSignin":    "a desktop sign-in binds the owner's cloud identity",
	"Server.handleRunSignIn":             "a run sign-in binds the owner's cloud identity",
	"Server.macWindowViewFor":            "needs the owner at their own Mac",
	"Server.handleMacWindow":             "needs the owner at their own Mac",
	"Server.backgroundWatchViewFor":      "background runs get no grant",
	"Server.handleBackgroundWatchTicket": "background runs get no grant",
	"Server.uiBackgroundDoorRefused":     "background runs get no grant",
	"Server.uiBackgroundWatchDenies":     "background runs get no grant",
	"Server.mayPauseDesktopAgent":        "pauses the agent for the owner's sign-in",
	"Server.inboundCaller":               "an inbound app's callers are fixed at create",
	"Server.localAttachCheck":            "local attach is the owner's alone",
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

// mentionSites is every mention of name in node, a call or a method value alike: taking supportMayEnter as a
// value and calling it later is a caller too.
func mentionSites(node ast.Node, name string) []ast.Node {
	var out []ast.Node
	ast.Inspect(node, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			out = append(out, id)
		}
		return true
	})
	return out
}

// censusReceiver is the receiver type's name of a method ("Server" for `func (s *Server) f`), "" for a function.
func censusReceiver(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	switch r := recv.(type) {
	case *ast.IndexExpr:
		recv = r.X
	case *ast.IndexListExpr:
		recv = r.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// censusUnits is every place in the package's non-test files that can hold a call: each func declaration keyed by
// receiver type and name ("Server.sshAuth"; a plain function by its name), and each package-level var with a value
// ("var x"), which covers a func literal and a method expression. A name can repeat (an init, a build-tagged
// pair), so a key holds every node. A func declaration's node is its body, a var's its spec.
func censusUnits(t *testing.T) map[string][]ast.Node {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]ast.Node{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				key := d.Name.Name
				if recv := censusReceiver(d); recv != "" {
					key = recv + "." + key
				}
				out[key] = append(out[key], d.Body)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && d.Tok == token.VAR && len(vs.Values) > 0 {
						out["var "+vs.Names[0].Name] = append(out["var "+vs.Names[0].Name], vs)
					}
				}
			}
		}
	}
	return out
}

// censusBody is the one func body under key, nil when it is absent or not a func declaration.
func censusBody(units map[string][]ast.Node, key string) *ast.BlockStmt {
	for _, n := range units[key] {
		if b, ok := n.(*ast.BlockStmt); ok {
			return b
		}
	}
	return nil
}

func mentionsArm(nodes []ast.Node) bool {
	return slices.ContainsFunc(nodes, func(n ast.Node) bool { return len(mentionSites(n, "supportMayEnter")) > 0 })
}

// TestSupportCensus_OnlyTheListedDoorsCallTheArm: every unit that mentions supportMayEnter is inside
// supportCallList, and every key on it calls it: a bare mention there (`_ = s.supportMayEnter`) asks nothing. The
// scan holds each func declaration by receiver and name and each package-level var, so a call added to any
// function sharing a bare name with another is seen.
func TestSupportCensus_OnlyTheListedDoorsCallTheArm(t *testing.T) {
	units := censusUnits(t)
	for _, key := range sortedKeys(units) {
		if _, listed := supportCallList[key]; !listed && mentionsArm(units[key]) {
			t.Errorf("%s mentions supportMayEnter and is not on supportCallList. %s", key, supportCensusEdit)
		}
	}
	for _, key := range sortedKeys(supportCallList) {
		nodes, ok := units[key]
		if !ok {
			t.Errorf("supportCallList names %s, which no longer exists: a rename or a move is a design change too. %s", key, supportCensusEdit)
			continue
		}
		if !slices.ContainsFunc(nodes, func(n ast.Node) bool { return callsAny(n, "supportMayEnter") }) {
			t.Errorf("%s is on supportCallList and does not call supportMayEnter: a door that stopped asking the arm lets a helper in or out unchecked. %s", key, supportCensusEdit)
		}
	}
}

// TestSupportCensus_RunExecKeepsItsRecheck: handleRunExec asks the arm twice, once at the door and once in the
// func literal its open command hands to holdAttachAuthority, which ends the command when the grant does. The
// door's call alone must not satisfy the census, so the re-check is pinned however it reaches the arm.
func TestSupportCensus_RunExecKeepsItsRecheck(t *testing.T) {
	body := censusBody(censusUnits(t), "Server.handleRunExec")
	if body == nil {
		t.Fatalf("Server.handleRunExec no longer exists. %s", supportCensusEdit)
	}
	if !callsAny(body, "holdAttachAuthority") {
		t.Errorf("handleRunExec does not call holdAttachAuthority: an open command would outlive its grant. %s", supportCensusEdit)
	}
	recheck := 0
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok {
			recheck += len(mentionSites(lit, "supportMayEnter"))
			return false
		}
		return true
	})
	if recheck == 0 {
		t.Errorf("handleRunExec has no func literal that asks supportMayEnter: the open command's re-check is gone. %s", supportCensusEdit)
	}
	if got := len(mentionSites(body, "supportMayEnter")); got-recheck == 0 {
		t.Errorf("handleRunExec asks supportMayEnter only in its re-check: the door's own call is gone. %s", supportCensusEdit)
	}
}

// TestSupportCensus_NeverListedFunctionsStayOut: every function on supportNeverList exists (a rename fails here
// rather than silently leaving the door unwatched) and mentions neither supportMayEnter nor any call-list function,
// so a door taken as a method value and called later counts as reaching it.
func TestSupportCensus_NeverListedFunctionsStayOut(t *testing.T) {
	units := censusUnits(t)
	var doors []string
	for _, key := range sortedKeys(supportCallList) {
		doors = append(doors, key[strings.LastIndex(key, ".")+1:])
	}
	for _, key := range sortedKeys(supportNeverList) {
		nodes, ok := units[key]
		if !ok {
			t.Errorf("supportNeverList names %s, which no longer exists: a rename moves the guard off the door. %s", key, supportCensusEdit)
			continue
		}
		if mentionsArm(nodes) {
			t.Errorf("%s mentions supportMayEnter but must never (%s). %s", key, supportNeverList[key], supportCensusEdit)
		}
		for _, door := range doors {
			if slices.ContainsFunc(nodes, func(n ast.Node) bool { return len(mentionSites(n, door)) > 0 }) {
				t.Errorf("%s mentions %s, a door of the support arm, but must never (%s). %s", key, door, supportNeverList[key], supportCensusEdit)
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
	units := censusUnits(t)
	for _, tc := range []struct {
		name   string
		branch func(*ast.BlockStmt) *ast.IfStmt
		what   string
	}{
		{"Server.refuseAttachEntry", ticketBranch, "the `if ta, ... := ticketActorFromContext(...)` block"},
		{"Server.attachAuthority", ticketBranch, "the `if ta, ... := ticketActorFromContext(...)` block"},
		{"Server.uiTicketMayEnter", callerlessBranch, "the `if ta.caller == \"\"` branch"},
		{"Server.sshAuth", notOperatorOwnedBranch, "the `if reason == \"\" && !run.OperatorOwned` branch"},
	} {
		body := censusBody(units, tc.name)
		if body == nil {
			t.Errorf("%s no longer exists. %s", tc.name, supportCensusEdit)
			continue
		}
		calls := mentionSites(body, "supportMayEnter")
		if len(calls) == 0 {
			t.Errorf("%s does not mention supportMayEnter. %s", tc.name, supportCensusEdit)
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

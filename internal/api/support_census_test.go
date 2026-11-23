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
// and name ("Server.sshAuth"), with the door it guards. Every mention of the arm is a call: the arm is called
// where it is named, so a method value (the arm passed or stored as a value) is refused in any unit.
var supportCallList = map[string]string{
	"Server.handleAttachTicket": "the ticket mint: purpose \"\" is the terminal, \"ui\" the UI doors",
	"Server.refuseAttachEntry":  "the terminal's WebSocket entry, its ticket branch only",
	"Server.attachAuthority":    "the open terminal's re-check, its ticket branch only",
	"Server.handleRunExec":      "POST /exec, and the open command's re-check",
	"Server.uiTicketMayEnter":   "the UI gateway's ticket entry, inside ta.caller == \"\"",
	// refuseDesktopKindApp is a second ask at an existing door, not a new door: the UI gateway's entry asks it
	// after uiTicketMayEnter, for a declared app of kind desktop only.
	"Server.refuseDesktopKindApp": "the UI gateway's entry for a declared app of kind desktop: the desktop door, on top of uiTicketMayEnter's web_apps door",
	"Server.uiSessionArms":        "the UI session's re-check, after the portal's arm (uiSessionAuthority)",
	"Server.desktopRun":           "the Files panel",
	"Server.sshAuth":              "the SSH gateway's authentication, inside the operator-owned rule's branch",
	"Server.sshCurrentKey":        "the SSH gateway's re-check of an open connection",
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

// mentionSites is every mention of name in node, a call or a value alike.
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
// ("var x", under each of its names), which covers a func literal and a method expression. A name can repeat (an init, a build-tagged
// pair), so a key holds every node. A func declaration's node is its body, a var's its spec.
func censusUnits(t *testing.T) map[string][]ast.Node {
	t.Helper()
	units, _ := censusParse(t)
	return units
}

// censusImportNames is the names a file's imports go by in it: the alias, or the last path element (without a
// version suffix) when there is none.
func censusImportNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if rest, ok := strings.CutSuffix(path, "/"+name); ok && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
			name = rest[strings.LastIndex(rest, "/")+1:]
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = true
	}
	return names
}

// censusParse is censusUnits with, for each unit's node, the import names of the file it sits in.
func censusParse(t *testing.T) (map[string][]ast.Node, map[ast.Node]map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]ast.Node{}
	imports := map[ast.Node]map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		names := censusImportNames(f)
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
				imports[d.Body] = names
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && d.Tok == token.VAR && len(vs.Values) > 0 {
						for _, name := range vs.Names { // `var a, b = ...` holds its spec under each name
							out["var "+name.Name] = append(out["var "+name.Name], vs)
						}
						imports[vs] = names
					}
				}
			}
		}
	}
	return out, imports
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

// armValueMentions is every mention of supportMayEnter in node that is not the function of a call: a method value,
// the arm passed or stored to be called somewhere else.
func armValueMentions(node ast.Node) []ast.Node {
	called := map[*ast.Ident]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				called[fn] = true
			case *ast.SelectorExpr:
				called[fn.Sel] = true
			}
		}
		return true
	})
	var out []ast.Node
	for _, n := range mentionSites(node, "supportMayEnter") {
		if !called[n.(*ast.Ident)] {
			out = append(out, n)
		}
	}
	return out
}

func mentionsArm(nodes []ast.Node) bool {
	return slices.ContainsFunc(nodes, func(n ast.Node) bool { return len(mentionSites(n, "supportMayEnter")) > 0 })
}

// TestSupportCensus_OnlyTheListedDoorsCallTheArm: every unit that mentions supportMayEnter is inside
// supportCallList, every key on it calls it, and no unit, listed or not, mentions it other than as a call: the arm
// is called where it is named, so a method value (`f := s.supportMayEnter`, the arm passed as an argument) is
// refused. The scan holds each func declaration by receiver and name and each package-level var, so a call added
// to any function sharing a bare name with another is seen.
func TestSupportCensus_OnlyTheListedDoorsCallTheArm(t *testing.T) {
	units := censusUnits(t)
	for _, key := range sortedKeys(units) {
		for _, node := range units[key] {
			if len(armValueMentions(node)) > 0 {
				t.Errorf("%s takes supportMayEnter as a value: the arm is called where it is named; taking it as a value hides where it is asked. %s", key, supportCensusEdit)
			}
		}
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

// recheckLiteral is the func literal whose body calls supportMayEnter that expr is, or that expr names when it is
// an identifier assigned exactly once in body (`check := func(...) {...}`), nil for anything else.
func recheckLiteral(body *ast.BlockStmt, expr ast.Expr) *ast.FuncLit {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = paren.X
	}
	armLit := func(e ast.Expr) *ast.FuncLit {
		if lit, ok := e.(*ast.FuncLit); ok && callsAny(lit, "supportMayEnter") {
			return lit
		}
		return nil
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return armLit(expr)
	}
	var assigned []ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if l, ok := lhs.(*ast.Ident); ok && l.Name == id.Name {
					if len(n.Lhs) == len(n.Rhs) {
						assigned = append(assigned, n.Rhs[i])
					} else {
						assigned = append(assigned, nil)
					}
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if name.Name == id.Name {
					if len(n.Names) == len(n.Values) {
						assigned = append(assigned, n.Values[i])
					} else {
						assigned = append(assigned, nil)
					}
				}
			}
		}
		return true
	})
	if len(assigned) != 1 || assigned[0] == nil {
		return nil
	}
	return armLit(assigned[0])
}

// TestSupportCensus_RunExecKeepsItsRecheck: handleRunExec asks the arm twice, once at the door and once in the
// func literal its open command hands to holdAttachAuthority, which ends the command when the grant does. The
// re-check is the check argument of that call (holdAttachAuthority(ctx, check, end)): a func literal that calls
// the arm, or an identifier assigned once from one. A literal that calls the arm and is never handed over is no
// re-check, and the door's call alone must not satisfy the census.
func TestSupportCensus_RunExecKeepsItsRecheck(t *testing.T) {
	body := censusBody(censusUnits(t), "Server.handleRunExec")
	if body == nil {
		t.Fatalf("Server.handleRunExec no longer exists. %s", supportCensusEdit)
	}
	holds := callSites(body, "holdAttachAuthority")
	if len(holds) == 0 {
		t.Errorf("handleRunExec does not call holdAttachAuthority: an open command would outlive its grant. %s", supportCensusEdit)
	}
	var handed []*ast.FuncLit
	for _, site := range holds {
		call := site.(*ast.CallExpr)
		if len(call.Args) != 3 {
			t.Errorf("handleRunExec calls holdAttachAuthority with %d arguments, want (ctx, check, end). %s", len(call.Args), supportCensusEdit)
			continue
		}
		lit := recheckLiteral(body, call.Args[1])
		if lit == nil {
			t.Errorf("handleRunExec hands holdAttachAuthority a check that is not a func literal calling supportMayEnter, nor a name assigned once from one: the open command's re-check is gone. %s", supportCensusEdit)
			continue
		}
		handed = append(handed, lit)
	}
	outside := 0
	for _, site := range callSites(body, "supportMayEnter") {
		if !slices.ContainsFunc(handed, func(lit *ast.FuncLit) bool { return site.Pos() >= lit.Pos() && site.End() <= lit.End() }) {
			outside++
		}
	}
	if outside == 0 {
		t.Errorf("handleRunExec asks supportMayEnter only in its re-check: the door's own call is gone. %s", supportCensusEdit)
	}
}

// censusEdges is the names of the units node calls or mentions, split by how a unit answers to them. A selector
// with an exported name whose left side is one of the file's imports is another package and no edge (an unexported
// one is a value named like an import). A bare identifier, `_` apart, is a package-level function or var (funcs). Any other
// selector `x.Name`, a call or a method value, is a method of any receiver (methods).
func censusEdges(node ast.Node, imported map[string]bool) (funcs, methods []string) {
	viaSelector := map[*ast.Ident]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			viaSelector[n.Sel] = true
			if x, ok := n.X.(*ast.Ident); ok && imported[x.Name] && ast.IsExported(n.Sel.Name) {
				return false
			}
			methods = append(methods, n.Sel.Name)
		case *ast.Ident:
			if !viaSelector[n] && n.Name != "_" {
				funcs = append(funcs, n.Name)
			}
		}
		return true
	})
	return funcs, methods
}

// censusReach is the first path from the unit start to a unit in targets, following every call and every mention
// of another unit in a unit's nodes, as censusEdges resolves them (conservative: a method name answers for every
// receiver). The path is the unit keys from start to the target; nil when targets cannot be reached.
func censusReach(units map[string][]ast.Node, imports map[ast.Node]map[string]bool, funcs, methods map[string][]string, start string, targets map[string]bool) []string {
	parent := map[string]string{start: ""}
	queue := []string{start}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		var next []string
		for _, node := range units[key] {
			f, m := censusEdges(node, imports[node])
			for _, name := range slices.Compact(slices.Sorted(slices.Values(f))) {
				next = append(next, funcs[name]...)
			}
			for _, name := range slices.Compact(slices.Sorted(slices.Values(m))) {
				next = append(next, methods[name]...)
			}
		}
		for _, k := range next {
			if _, seen := parent[k]; seen {
				continue
			}
			parent[k] = key
			if targets[k] {
				path := []string{k}
				for p := key; p != ""; p = parent[p] {
					path = append([]string{p}, path...)
				}
				return path
			}
			queue = append(queue, k)
		}
	}
	return nil
}

// TestSupportCensus_NeverListedFunctionsStayOut: every function on supportNeverList exists (a rename fails here
// rather than silently leaving the door unwatched) and reaches neither supportMayEnter nor any call-list function,
// directly or through any chain of calls and mentions across the package's units, so a wrapper or a door taken as a
// method value and called later counts as reaching it. A call into an imported package is no edge, a bare name is a
// package-level function or var, and `x.Name` is a method of any receiver.
func TestSupportCensus_NeverListedFunctionsStayOut(t *testing.T) {
	units, imports := censusParse(t)
	funcs, methods := map[string][]string{}, map[string][]string{}
	for _, key := range sortedKeys(units) {
		if i := strings.LastIndex(key, "."); i >= 0 {
			methods[key[i+1:]] = append(methods[key[i+1:]], key)
		} else {
			name := strings.TrimPrefix(key, "var ")
			funcs[name] = append(funcs[name], key)
		}
	}
	targets := map[string]bool{"Server.supportMayEnter": true}
	for key := range supportCallList {
		targets[key] = true
	}
	for _, key := range sortedKeys(supportNeverList) {
		if _, ok := units[key]; !ok {
			t.Errorf("supportNeverList names %s, which no longer exists: a rename moves the guard off the door. %s", key, supportCensusEdit)
			continue
		}
		if path := censusReach(units, imports, funcs, methods, key, targets); path != nil {
			t.Errorf("%s reaches a door of the support arm but must never (%s): %s. %s", key, supportNeverList[key], strings.Join(path, " → "), supportCensusEdit)
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

// TestSupportCensus_ListsAreDisjoint: no name is on both lists, and the call list holds the design's ten.
func TestSupportCensus_ListsAreDisjoint(t *testing.T) {
	for name := range supportNeverList {
		if _, both := supportCallList[name]; both {
			t.Errorf("%s is on supportCallList and supportNeverList. %s", name, supportCensusEdit)
		}
	}
	if len(supportCallList) != 10 {
		t.Errorf("supportCallList holds %d names, want the design's ten: %s", len(supportCallList), strings.Join(sortedKeys(supportCallList), ", "))
	}
}

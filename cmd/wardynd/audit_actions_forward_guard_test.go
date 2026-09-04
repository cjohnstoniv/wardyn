// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The FORWARD direction of the audit-actions doc parity, and the reason it had
// to be added: the mechanism the requirement names ("every new audit action has
// its doc row; the cmd/wardynd guard tests are the mechanism") only ever ran
// doc -> code. TestAuditActionsDocCitationsAreLive walks the doc's rows and
// checks each citation still points at its emit site, so RENAMING an action
// fails — but ADDING one with no row at all passed, silently. Its two sibling
// guards are both bidirectional (TestEnvDoc_Forward/Reverse,
// TestPolicyDoc_EveryFieldHasRow/EveryRowHasField); this one was the odd surface
// out, in exactly the direction new work moves.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// auditActionAllow lists action literals that are deliberately not registry
// rows. Mirrors envDocAllow's role: the allowlist is the honest record of what
// the parity does NOT cover, so an exclusion is a decision someone wrote down
// rather than a silence.
var auditActionAllow = map[string]bool{}

// auditActionRow matches a docs/AUDIT-ACTIONS.md table row's leading action
// cell: a row starts with "|", optional space, then `the.action` in backticks.
var auditActionRow = regexp.MustCompile("^\\|\\s*`([a-z0-9_.*:-]+)`")

// documentedAuditActions returns the action literals AUDIT-ACTIONS.md carries a
// row for. A row may name a wildcard family ("egress.*"), which documents every
// action under that prefix.
func documentedAuditActions(t *testing.T, root string) (exact map[string]bool, prefixes []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docs", "AUDIT-ACTIONS.md"))
	if err != nil {
		t.Fatalf("read docs/AUDIT-ACTIONS.md: %v", err)
	}
	exact = map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := auditActionRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if strings.HasSuffix(m[1], "*") {
			prefixes = append(prefixes, strings.TrimSuffix(m[1], "*"))
			continue
		}
		exact[m[1]] = true
	}
	if len(exact) == 0 {
		t.Fatal("parsed 0 action rows out of docs/AUDIT-ACTIONS.md — the table shape changed and this guard now checks nothing")
	}
	return exact, prefixes
}

// emittedAuditAction is one action literal found in non-test Go, with where it
// was found so a failure names the file to fix rather than just the string.
type emittedAuditAction struct {
	action string
	where  string
}

// emittedAuditActions walks envDocRoots' non-test Go and returns every audit
// action literal the tree emits.
//
// Parsed with go/ast rather than matched with a regex, because the action is a
// POSITIONAL argument (auditEvent(runID, actorType, actor, ACTION, target,
// outcome, data)) that routinely wraps across lines — a line-based matcher
// would silently miss those and report a clean parity over a partial scan,
// which is the failure mode this whole test exists to end.
//
// Two shapes are collected: the 4th argument of an auditEvent(...) call, and
// the Action field of an AuditEvent composite literal (the broker and the
// runner build events directly). Where the argument is a variable rather than a
// literal, the string literals assigned to that variable ANYWHERE IN THE SAME
// FUNCTION are collected instead — the shape permissions.go uses, picking an
// action name in a branch above the emit.
//
// STATED LIMIT: an action assembled at run time (concatenated, or read from a
// map keyed by something dynamic) is invisible to a static scan. Nothing in the
// tree does that today — the count this test reports is the whole emit surface
// it can see, and it fails if that count collapses.
func emittedAuditActions(t *testing.T, root string) []emittedAuditAction {
	t.Helper()
	var out []emittedAuditAction
	fset := token.NewFileSet()

	for _, sub := range envDocRoots {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // not this guard's job to police syntax
			}
			rel, _ := filepath.Rel(root, path)
			out = append(out, auditActionsInFile(f, rel)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	return out
}

func auditActionsInFile(f *ast.File, rel string) []emittedAuditAction {
	var out []emittedAuditAction
	add := func(lit ast.Expr, where string) {
		if s, ok := stringLit(lit); ok {
			out = append(out, emittedAuditAction{action: s, where: where})
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if calleeName(v.Fun) != "auditEvent" || len(v.Args) < 4 {
				return true
			}
			if _, ok := stringLit(v.Args[3]); ok {
				add(v.Args[3], rel)
				return true
			}
			// A variable action: collect every literal assigned to that name in
			// the enclosing function.
			if id, ok := v.Args[3].(*ast.Ident); ok {
				for _, s := range literalsAssignedTo(f, id.Name) {
					out = append(out, emittedAuditAction{action: s, where: rel})
				}
			}
		case *ast.CompositeLit:
			if !isAuditEventType(v.Type) {
				return true
			}
			for _, el := range v.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Action" {
					add(kv.Value, rel)
				}
			}
		}
		return true
	})
	return out
}

func calleeName(fun ast.Expr) string {
	switch v := fun.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

func isAuditEventType(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "AuditEvent"
	case *ast.SelectorExpr:
		return v.Sel.Name == "AuditEvent"
	}
	return false
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// literalsAssignedTo returns every string literal assigned to name anywhere in
// the file. Deliberately file-scoped rather than function-scoped: over-reading
// here can only ever DEMAND a doc row that is not needed, which fails loudly and
// is fixed by writing the row; under-reading lets an undocumented action ship,
// which is the defect.
func literalsAssignedTo(f *ast.File, name string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name || i >= len(as.Rhs) {
				continue
			}
			if s, ok := stringLit(as.Rhs[i]); ok {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

// TestAuditActionsDoc_EveryEmitHasRow is the forward ratchet: an audit action
// emitted by non-test Go must have a row in docs/AUDIT-ACTIONS.md. Adding an
// emit with a novel action name used to leave `go test ./cmd/wardynd/` green.
func TestAuditActionsDoc_EveryEmitHasRow(t *testing.T) {
	root := repoRoot(t)
	exact, prefixes := documentedAuditActions(t, root)
	emits := emittedAuditActions(t, root)
	if len(emits) == 0 {
		t.Fatal("found 0 audit action literals in non-test Go — the emit shape changed and this guard now checks nothing")
	}

	reported := map[string]bool{}
	for _, e := range emits {
		if auditActionAllow[e.action] || exact[e.action] || reported[e.action] {
			continue
		}
		covered := false
		for _, p := range prefixes {
			if strings.HasPrefix(e.action, p) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		reported[e.action] = true
		t.Errorf("audit action %q is emitted in %s but has no row in docs/AUDIT-ACTIONS.md — add the row "+
			"(or auditActionAllow if it is genuinely not part of the registry). An action an operator cannot look "+
			"up is one they cannot alert on", e.action, e.where)
	}
	t.Logf("checked %d emitted action literals against %d documented rows and %d wildcard families",
		len(emits), len(exact), len(prefixes))
}

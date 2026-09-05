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

// auditTree is every non-test Go file under envDocRoots, parsed once, plus the
// package-level string constants they declare.
//
// Held together rather than scanned file by file because an action's journey to
// the sink is not file-local: it is named by a const declared in one file, passed
// to a helper declared in a second, and emitted from a third.
type auditTree struct {
	files  []*ast.File
	rels   []string
	consts map[string][]string
}

func parseAuditTree(t *testing.T, root string) auditTree {
	t.Helper()
	tr := auditTree{consts: map[string][]string{}}
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
			tr.files = append(tr.files, f)
			tr.rels = append(tr.rels, rel)
			tr.collectConsts(f)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	return tr
}

// collectConsts records every package-level string const/var by NAME. Name-keyed
// rather than package-qualified on purpose: a collision can only ever make the
// guard read one extra candidate string, which at worst DEMANDS a doc row that is
// not needed and fails loudly. Missing one lets an undocumented action ship.
func (tr *auditTree) collectConsts(f *ast.File) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if v, ok := stringLit(vs.Values[i]); ok {
					tr.consts[name.Name] = append(tr.consts[name.Name], v)
				}
			}
		}
	}
}

// actionEmitters returns every function that writes an argument into an audit
// event's Action field, keyed by function name and giving the parameter index
// that carries the action.
//
// DERIVED, never enumerated — and that is the whole of F094's second round. The
// old scan matched ONE hardcoded callee name, `auditEvent`, so the two in-tree
// helpers that take the action as a PARAMETER were invisible: internal/identity/
// embedded's (*Provider).audit and internal/groundtruth's auditFor. Adding a
// brand-new undocumented action through either left `go test ./cmd/wardynd/`
// green and did not even move the emit count — the literal was never seen. Seven
// of the actions docs/AUDIT-ACTIONS.md documents (identity.*, kernel.*) sat
// outside the guard's field of view for the same reason, so deleting their rows
// failed nothing either. A guard's field of view is part of its correctness.
//
// The seed is now the SHAPE: a function that assigns one of its own parameters to
// an AuditEvent's Action field IS an emitter — which is exactly what auditEvent
// is, so the hardcoded name derives itself — and a function that passes one of
// its own parameters into a known emitter's action slot is one too, to a fixed
// point. A wrapper added tomorrow is covered on the day it is written.
func (tr auditTree) actionEmitters() map[string]int {
	emitters := map[string]int{}
	for changed := true; changed; {
		changed = false
		for _, f := range tr.files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if _, known := emitters[fn.Name.Name]; known {
					continue
				}
				if idx, found := actionParamOf(fn, emitters); found {
					emitters[fn.Name.Name] = idx
					changed = true
				}
			}
		}
	}
	return emitters
}

// paramNames flattens a function's parameters into positional order —
// `func f(a, b string)` is two parameters sharing one field, not one.
func paramNames(fn *ast.FuncDecl) []string {
	var out []string
	if fn.Type.Params == nil {
		return out
	}
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			out = append(out, "") // unnamed: positional, never referenced by name
			continue
		}
		for _, n := range field.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// actionParamOf reports which of fn's own parameters ends up as an audit action:
// written straight into an AuditEvent composite literal's Action field, or handed
// to an already-known emitter in its action slot.
func actionParamOf(fn *ast.FuncDecl, emitters map[string]int) (int, bool) {
	params := paramNames(fn)
	idx := -1
	paramIndex := func(e ast.Expr) int {
		id, ok := e.(*ast.Ident)
		if !ok {
			return -1
		}
		for i, p := range params {
			if p != "" && p == id.Name {
				return i
			}
		}
		return -1
	}
	take := func(e ast.Expr) {
		if i := paramIndex(e); i >= 0 && idx < 0 {
			idx = i
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CompositeLit:
			if !isAuditEventType(v.Type) {
				return true
			}
			for _, el := range v.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Action" {
						take(kv.Value)
					}
				}
			}
		case *ast.CallExpr:
			if at, ok := emitters[calleeName(v.Fun)]; ok && at < len(v.Args) {
				take(v.Args[at])
			}
		}
		return true
	})
	return idx, idx >= 0
}

// actions returns every audit action the tree emits: the argument in each
// emitter call's action slot, and the Action field of every AuditEvent built
// inline.
//
// Parsed with go/ast rather than matched with a regex, because the action is a
// POSITIONAL argument that routinely wraps across lines — a line-based matcher
// would silently miss those and report a clean parity over a partial scan, which
// is the failure mode this whole test exists to end.
func (tr auditTree) actions(emitters map[string]int) []emittedAuditAction {
	var out []emittedAuditAction
	for i, f := range tr.files {
		rel := tr.rels[i]
		add := func(e ast.Expr) {
			for _, s := range tr.resolveAction(f, e) {
				out = append(out, emittedAuditAction{action: s, where: rel})
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if at, ok := emitters[calleeName(v.Fun)]; ok && at < len(v.Args) {
					add(v.Args[at])
				}
			case *ast.CompositeLit:
				if !isAuditEventType(v.Type) {
					return true
				}
				for _, el := range v.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Action" {
							add(kv.Value)
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// resolveAction reads the string(s) an action expression can carry: a literal, a
// package-level string constant (named plainly or through its package), or a
// variable the file assigns literals to — the shape permissions.go uses, picking
// an action name in a branch above the emit.
//
// STATED LIMIT, and this is now the whole of it: an action assembled at RUN time
// (concatenated, or read from a map keyed by something dynamic) is invisible to a
// static scan. Nothing in the tree does that today. The previous limit paragraph
// disclosed only that one and was wrong by omission — the wrapper and const
// shapes were invisible too, and were the actual gap.
//
// Over-reading — naming a string that is not really an action — can only DEMAND a
// doc row that is not needed, which fails loudly and is fixed by writing the row
// or an auditActionAllow entry. Under-reading lets an undocumented action ship,
// which is the defect. Every ambiguity here is resolved toward over-reading.
func (tr auditTree) resolveAction(f *ast.File, e ast.Expr) []string {
	if s, ok := stringLit(e); ok {
		return []string{s}
	}
	switch v := e.(type) {
	case *ast.Ident:
		return append(append([]string(nil), tr.consts[v.Name]...), literalsAssignedTo(f, v.Name)...)
	case *ast.SelectorExpr:
		return tr.consts[v.Sel.Name]
	}
	return nil
}

// emittedAuditActions returns every audit action literal the tree emits.
func emittedAuditActions(t *testing.T, root string) []emittedAuditAction {
	t.Helper()
	tr := parseAuditTree(t, root)
	emitters := tr.actionEmitters()
	if len(emitters) == 0 {
		t.Fatal("derived 0 audit-emitting functions — the AuditEvent shape changed and this guard now checks nothing")
	}
	return tr.actions(emitters)
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

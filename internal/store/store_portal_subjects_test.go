// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// bindingRow answers the one binding read the ruling makes: a row for a bound key, no rows otherwise.
type bindingRow struct {
	bound map[string]bool
	asked []string
}

func (b *bindingRow) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	key, _ := args[0].(string)
	b.asked = append(b.asked, key)
	return bindingResult{ok: b.bound[key]}
}

type bindingResult struct{ ok bool }

func (r bindingResult) Scan(dest ...any) error {
	if !r.ok {
		return pgx.ErrNoRows
	}
	*(dest[0].(*int)) = 1
	return nil
}

// Every list the ruling names rules a portal person the way its kind says, a name that is not a portal
// person passes untouched without a read, and a mixed-case or padded spelling is ruled as the canonical one.
func TestPortalSubjectRuling_EveryStoreWriteRules(t *testing.T) {
	const bound = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const unbound = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	ctx := context.Background()
	if len(delegatePersonRulings) != 10 {
		t.Fatalf("%d lists ruled, want the 10 the design names", len(delegatePersonRulings))
	}
	for column, kind := range delegatePersonRulings {
		q := &bindingRow{bound: map[string]bool{bound: true}}
		person := func(key string) string { return "delegate:person:" + key + ":mfrgg" }

		if err := ruleDelegatePersonSubject(ctx, q, column, "alice@example.com"); err != nil || len(q.asked) != 0 {
			t.Errorf("%s: an ordinary subject = %v after %d reads, want nil and none", column, err, len(q.asked))
		}
		switch kind {
		case portalRefuse:
			for _, s := range []string{person(bound), person(unbound), "  DELEGATE:Person:" + bound + ":MFRGG "} {
				if err := ruleDelegatePersonSubject(ctx, q, column, s); !errors.Is(err, ErrDelegatePersonNotAllowed) {
					t.Errorf("%s: %q = %v, want ErrDelegatePersonNotAllowed", column, s, err)
				}
			}
			if len(q.asked) != 0 {
				t.Errorf("%s: a refuse list read the binding", column)
			}
		case portalAllow:
			if err := ruleDelegatePersonSubject(ctx, q, column, person(bound)); err != nil {
				t.Errorf("%s: a bound portal's person = %v, want nil", column, err)
			}
			if err := ruleDelegatePersonSubject(ctx, q, column, "  Delegate:Person:"+bound+":MFRGG "); err != nil {
				t.Errorf("%s: the same person, folded = %v, want nil", column, err)
			}
			for _, s := range []string{person(unbound), "delegate:person:short:mfrgg", "delegate:person:"} {
				if err := ruleDelegatePersonSubject(ctx, q, column, s); !errors.Is(err, ErrDelegatePersonUnknownPortal) {
					t.Errorf("%s: %q = %v, want ErrDelegatePersonUnknownPortal", column, s, err)
				}
			}
		default:
			t.Errorf("%s: unknown kind %d", column, kind)
		}
	}
	if err := ruleDelegatePersonSubject(ctx, &bindingRow{}, "nope.column", "x"); err == nil {
		t.Error("a column outside the table was ruled without an error")
	}
}

// ruleCalls are the calls that count as a ruling inside a function.
var ruleCalls = map[string]bool{"ruleDelegatePersonSubject": true, "ruleUIAppCallersQ": true, "RefuseDelegatePersonSubject": true}

// ruledTables are the tables whose writes carry a person list or a UI-app caller document, from the lists
// the ruling names and the policy documents ruleUIAppCallersQ reads, and key_domain_assignments, whose writer
// lives outside this package and refuses a portal person by its prefix alone.
var ruledTables = []string{
	"governance_unit_admins", "template_group_admins", "role_mappings", "remote_runners", "service_identities",
	"capability_grants", "governance_memberships", "governance_restriction_members", "user_drive_grants",
	"runner_pool_use_policies", "run_policies", "governance_units", "launch_presets", "run_template_revisions",
	"key_domain_assignments",
}

// moduleGoSources is every non-test Go file of the module with its source, found from the directory that holds
// go.mod, skipping vendored code, testdata and dot or node_modules directories.
func moduleGoSources(t *testing.T) map[string][]byte {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.mod above the test's directory")
		}
		root = parent
	}
	out := map[string][]byte{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (n == "vendor" || n == "testdata" || n == "node_modules" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = src
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// unruledWrites are the functions that write a ruled table and need no ruling, each with the reason. A
// function listed here that no longer writes one, or now calls a ruling, fails the test, so the list cannot
// go stale.
var unruledWrites = map[string]string{
	"MoveGovernanceMembershipQ":         "moves an existing membership to another unit; the subject is unchanged",
	"PromoteGovernanceMembershipQ":      "changes the membership's level; the subject is unchanged",
	"stripRunnerPoolUseSubjects":        "removes subjects from a list; removal is never ruled",
	"stripRemoteRunnerPeopleSubjects":   "removes subjects from a list; removal is never ruled",
	"EnsureRemoteRunners":               "writes the runner's limits; people_subjects is left at its default",
	"UpdateRemoteRunner":                "writes the runner's name and limits; people_subjects has its own ruled call",
	"EraseRunSetupsOf":                  "erases a person's rows; removal is never ruled",
	"EraseServiceIdentityAttributionOf": "erases a person's name from rows; removal is never ruled",
	"RetireServiceIdentity":             "retires the identity; the sponsor is unchanged",
	"RenameGovernanceUnitQ":             "writes the unit's name only",
	"WriteGovernanceUnitQ":              "recomputes the depth of every unit through refreshUnitDepths; the unit's own document is written by upsertGovernanceUnitTx, which rules it",
	"replaceUserDriveGrantOn":           "repoints the drive of a subject's existing row, then writes the grant through upsertUserDriveGrant, which rules it",
}

// writeHit is one function that holds a statement writing a ruled table.
type writeHit struct {
	File, Func, Table string
	Ruled             bool
}

var ruledWriteStatement = regexp.MustCompile(`(?is)\b(insert\s+into|update)\s+(` + strings.Join(ruledTables, "|") + `)\b`)

// stringExpr is the value of a string expression made of literals, identifiers that name a string constant or
// variable of consts, and +; ok is false for any other expression.
func stringExpr(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	case *ast.ParenExpr:
		return stringExpr(v.X, consts)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := stringExpr(v.X, consts)
		r, rok := stringExpr(v.Y, consts)
		return l + r, lok && rok
	}
	return "", false
}

// packageStrings is the package-level string constants and variables of the files of one directory, resolved
// through each other until none more resolves, so a statement held in a const that another const extends is
// found by the function that uses either.
func packageStrings(files []*ast.File) map[string]string {
	out := map[string]string{}
	for range 8 {
		for _, file := range files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if i < len(vs.Values) {
							if s, ok := stringExpr(vs.Values[i], out); ok {
								out[name.Name] = s
							}
						}
					}
				}
			}
		}
	}
	return out
}

// scanRuledWrites finds every function of the sources whose statements, literals inside it or package-level
// string constants it names, write a ruled table, and whether it calls a ruling. Sources are keyed by path; the
// package-level strings are those of the files in the same directory.
func scanRuledWrites(t *testing.T, sources map[string][]byte) []writeHit {
	t.Helper()
	paths := make([]string, 0, len(sources))
	for f := range sources {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	parsed := map[string]*ast.File{}
	byDir := map[string][]*ast.File{}
	for _, f := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), f, sources[f], 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		parsed[f] = file
		byDir[filepath.Dir(f)] = append(byDir[filepath.Dir(f)], file)
	}
	pkgStrings := map[string]map[string]string{}
	for dir, files := range byDir {
		pkgStrings[dir] = packageStrings(files)
	}
	var hits []writeHit
	for _, f := range paths {
		consts := pkgStrings[filepath.Dir(f)]
		for _, decl := range parsed[f].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var sql strings.Builder
			ruled := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if s, err := strconv.Unquote(v.Value); err == nil {
							sql.WriteString(s + " ")
						}
					}
				case *ast.Ident:
					if s, ok := consts[v.Name]; ok {
						sql.WriteString(s + " ")
					}
				case *ast.CallExpr:
					switch c := v.Fun.(type) {
					case *ast.Ident:
						ruled = ruled || ruleCalls[c.Name]
					case *ast.SelectorExpr:
						ruled = ruled || ruleCalls[c.Sel.Name]
					}
				}
				return true
			})
			if m := ruledWriteStatement.FindStringSubmatch(sql.String()); m != nil {
				hits = append(hits, writeHit{File: f, Func: fn.Name.Name, Table: m[2], Ruled: ruled})
			}
		}
	}
	return hits
}

// Every statement that writes a ruled table, in any non-test Go file of the module (inline, or in a
// package-level constant the function names), sits in a function that rules the write, or is named in
// unruledWrites with its reason; every ruled list has a call site naming it.
func TestPortalSubjectRuling_EveryWriteStatementRuled(t *testing.T) {
	sources := moduleGoSources(t)
	seenUnruled := map[string]bool{}
	ruledIn := map[string]bool{}
	var allSource strings.Builder
	for f, src := range sources {
		if f != filepath.Join("internal", "store", "store_portal_subjects.go") {
			allSource.Write(src)
		}
	}
	for _, h := range scanRuledWrites(t, sources) {
		if reason, listed := unruledWrites[h.Func]; listed {
			seenUnruled[h.Func] = true
			if h.Ruled {
				t.Errorf("%s (%s) calls a ruling and is also listed as needing none (%s)", h.Func, h.File, reason)
			}
			continue
		}
		if !h.Ruled {
			t.Errorf("%s (%s) writes %s and calls no portal person ruling", h.Func, h.File, h.Table)
		} else {
			ruledIn[h.File] = true
		}
	}
	if f := filepath.Join("internal", "secretstore", "keydomain", "service.go"); !ruledIn[f] {
		t.Errorf("the key_domain_assignments writer in %s was not found among the ruled writes: the scan does not reach it", f)
	}
	var stale []string
	for name := range unruledWrites {
		if !seenUnruled[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("unruledWrites names functions that write no ruled table: %v", stale)
	}
	for column := range delegatePersonRulings {
		if !strings.Contains(allSource.String(), `"`+column+`"`) {
			t.Errorf("no write calls the ruling for %s", column)
		}
	}
}

// The guard finds a statement held in a package-level constant, a constant built from others, and a package
// variable, whichever file of the package holds it, and tells a ruled function from an unruled one.
func TestPortalSubjectRuling_WriteGuardSeesConstSQL(t *testing.T) {
	sources := map[string][]byte{
		"x/sql.go": []byte("package x\n\nconst insertGrant = `INSERT INTO capability_grants (id) VALUES ($1)`\nconst upsertGrant = insertGrant + ` ON CONFLICT DO NOTHING`\nvar updateRoles = \"UPDATE role_mappings SET role = $1\"\nconst harmless = `UPDATE some_other_table SET a = 1`\n"),
		"x/use.go": []byte("package x\n\nfunc Unruled(q Q) { q.Exec(upsertGrant) }\nfunc Ruled(q Q) { ruleDelegatePersonSubject(q); q.Exec(insertGrant) }\nfunc Roles(q Q) { q.Exec(updateRoles) }\nfunc Other(q Q) { q.Exec(harmless) }\n"),
	}
	got := map[string]writeHit{}
	for _, h := range scanRuledWrites(t, sources) {
		got[h.Func] = h
	}
	if h, ok := got["Unruled"]; !ok || h.Ruled || h.Table != "capability_grants" {
		t.Errorf("Unruled = %+v (found %v), want an unruled write of capability_grants through a built constant", h, ok)
	}
	if h, ok := got["Ruled"]; !ok || !h.Ruled {
		t.Errorf("Ruled = %+v (found %v), want a ruled write", h, ok)
	}
	if h, ok := got["Roles"]; !ok || h.Ruled || h.Table != "role_mappings" {
		t.Errorf("Roles = %+v (found %v), want an unruled write of role_mappings through a variable", h, ok)
	}
	if _, ok := got["Other"]; ok {
		t.Error("a statement on an unruled table was reported")
	}
}

// uiAppCallerKeys reads the callers of the ui_apps entries as decoded strings, in whatever JSON spelling and
// wherever the array sits, and nothing else.
func TestPortalSubjectRuling_UICallerKeysReadDecodedCallersOnly(t *testing.T) {
	const k1 = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const k2 = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	cases := []struct {
		name string
		doc  string
		want []string
		err  bool
	}{
		{"plain policy spec", `{"ui_apps":[{"name":"a","port":80,"callers":["user:delegate:person:` + k1 + `:mfrgg"]}]}`, []string{k1}, false},
		{"escaped characters", `{"ui_apps":[{"callers":["user:delegate:person:` + k1 + `:mfrgg"]}]}`, []string{k1}, false},
		{"spacing, key order, case and padding", "{ \"ui_apps\" : [ { \"callers\" : [ \" USER:Delegate:Person:" + k1 + ":MFRGG \" ], \"name\" : \"a\" } ] }", []string{k1}, false},
		{"inside a launch request's overlay", `{"task":"x","policy_overlay":{"ui_apps":[{"callers":["user:delegate:person:` + k2 + `:mfrgg"]}]}}`, []string{k2}, false},
		{"two keys, one repeated, sorted", `{"ui_apps":[{"callers":["user:delegate:person:` + k2 + `:a","user:delegate:person:` + k1 + `:b","user:delegate:person:` + k2 + `:c"]}]}`, []string{k1, k2}, false},
		{"upper-case keys", `{"UI_APPS":[{"CALLERS":["user:delegate:person:` + k1 + `:mfrgg"]}]}`, []string{k1}, false},
		{"mixed-case keys in a launch overlay", `{"policy_overlay":{"Ui_Apps":[{"Name":"a","Callers":["user:delegate:person:` + k2 + `:mfrgg"]}]}}`, []string{k2}, false},
		{"mixed-case key beside a lower-case one", `{"ui_apps":[{"callers":["user:delegate:person:` + k1 + `:a"]}],"UI_APPS":[{"Callers":["user:delegate:person:` + k2 + `:b"]}]}`, []string{k1, k2}, false},
		{"mixed-case malformed key", `{"UI_APPS":[{"CALLERS":["user:delegate:person:short:x"]}]}`, nil, true},
		{"free text", `{"task":"ask user:delegate:person:` + k1 + `:x","description":"ui_apps callers user:delegate:person:` + k1 + `:x"}`, nil, false},
		{"callers outside ui_apps", `{"callers":["user:delegate:person:` + k1 + `:x"],"other":[{"callers":["user:delegate:person:` + k1 + `:x"]}]}`, nil, false},
		{"group and service callers", `{"ui_apps":[{"callers":["group:delegate:person:` + k1 + `:x","service:delegate:person:` + k1 + `:x","user:alice@example.com"]}]}`, nil, false},
		{"malformed key", `{"ui_apps":[{"callers":["user:delegate:person:short:x"]}]}`, nil, true},
		{"no person id", `{"ui_apps":[{"callers":["user:delegate:person:` + k1 + `:"]}]}`, nil, true},
		{"empty document", ``, nil, false},
	}
	for _, c := range cases {
		got, err := uiAppCallerKeys([]byte(c.doc))
		if c.err {
			if !errors.Is(err, ErrDelegatePersonUnknownPortal) {
				t.Errorf("%s: err = %v, want ErrDelegatePersonUnknownPortal", c.name, err)
			}
			continue
		}
		if err != nil || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: keys = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := uiAppCallerKeys([]byte(`{not json`)); err == nil {
		t.Error("a document that is not JSON was read without an error")
	}
}

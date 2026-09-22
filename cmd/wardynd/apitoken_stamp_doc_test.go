// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestAPITokenStampResidualIsPublished pins the api_tokens role/group stamp —
// its bound, and the fact that the bound is login, not a TTL — to the schema
// and the store code, and to the two documents that publish it.
//
// A token carries the role and group snapshot of the session that minted it and
// replays them on every request. The SSH-key analogue of that stamp is published
// as residual #15 and is bounded-stale: every login re-stamps the key and a TTL
// expires the override. The token stamp originally had neither the bound nor the
// residual; 0.7 gave the ROLE half the login bound, and #152 widened it to the
// GROUP half too. The remaining gap is the same shape #15 has: no TTL, so a human
// who never signs in again keeps the stamp indefinitely — and since 0.7 stamps
// `security_admin` verbatim, that now carries governance authority, so the only
// thing standing between a demoted security admin who never signs in again and
// the profile/grant/approval surface is an operator remembering to revoke.
//
// WHAT THIS GUARD READS (its scope IS part of its correctness): the migration
// that creates api_tokens, internal/store/store_apitokens.go, this package's
// boot_deps.go, docs/OPERATIONS.md and threatmodel/THREAT-MODEL.md. What it does
// NOT read: the CLI's help strings, the console's token screen, docs/MEMBERS.md
// and docs/SSH.md. Those were checked by hand when this was written and make no
// competing claim about token staleness (MEMBERS.md says only that tokens are
// independently revocable, which stays true) — but a claim added there later is
// outside this net.
func TestAPITokenStampResidualIsPublished(t *testing.T) {
	// (1) Schema premise: the stamp exists, and nothing expires it.
	file, cols := apiTokenColumns(t)
	for _, want := range []string{"role", "groups"} {
		if !slices.Contains(cols, want) {
			t.Errorf("%s no longer has an api_tokens.%s column — the stamp this residual is about may be gone; re-read it", file, want)
		}
	}
	expiryish := regexp.MustCompile(`(?i)expir|valid_until|not_after|ttl`)
	for _, c := range cols {
		if expiryish.MatchString(c) {
			t.Errorf("%s now has an api_tokens.%s column: the window may no longer be unbounded, so the docs' "+
				"\"no expiry column\" claim and residual #33 need re-reading, not deleting", file, c)
		}
	}

	// (2) Store premise: nothing UNEXPECTED re-stamps the row. Only
	// last_used_at, revoked_at, role, groups and groups_truncated are ever
	// written after mint.
	updated := apiTokenUpdatedColumns(t)
	// `role` JOINED this list when the token lane gained the login hook the key
	// lane had since 0046. `groups` and `groups_truncated` joined it with #152
	// (store.RefreshAPITokenIdentity, fired from OnLogin beside
	// RefreshSSHKeyRoles): the residual narrowed from "the group snapshot is
	// never refreshed" to "bounded-stale until the owner's next sign-in", the
	// same shape residual #15 already has. Removing either from this list means
	// the group half stopped being refreshed again and the docs would overstate
	// what is bounded.
	allowed := []string{"last_used_at", "revoked_at", "role", "groups", "groups_truncated"}
	for _, c := range updated {
		if !slices.Contains(allowed, c) {
			t.Errorf("internal/store/store_apitokens.go now UPDATEs api_tokens.%s — an unexpected column is re-stamped; "+
				"re-read OPERATIONS.md and residual #38 against what actually happens", c)
		}
	}
	for _, want := range []string{"role", "groups", "groups_truncated"} {
		if !slices.Contains(updated, want) {
			t.Errorf("internal/store/store_apitokens.go no longer UPDATEs api_tokens.%s — the login re-stamp that bounds "+
				"that half is gone, so OPERATIONS.md and residual #38 overstate what is bounded", want)
		}
	}

	// (3) The contrast the docs draw is real: SSH keys DO get re-stamped on
	// login, which is why the token stamp is the worse of the two.
	//
	// READ AS STRUCTURE, NOT AS A STRING. This was
	// strings.Contains(boot_deps.go, "RefreshAPITokenRoles"), and the refactor
	// that fixed the transposition half of this finding DEFEATED it — in the one
	// direction it was the only cover for. At the time, that name occurred in
	// boot_deps.go exactly once, at the call site, so gutting the OnLogin
	// closure made this test fail. Extracting the closure into refreshLoginStamps
	// put the same name into a loginStampStore interface declaration and a
	// paragraph of explanation IN THE SAME FILE the guard greps, and a grep
	// cannot tell a call from a comment: replacing the OnLogin body with a no-op
	// — which makes the demoted-admin bound dead in production on BOTH lanes —
	// left the whole package green.
	//
	// So it now asks the two structural questions the string was standing in
	// for: does the OnLogin callback CALL refreshLoginStamps, and does
	// refreshLoginStamps CALL both store methods on the store it was handed. It
	// is strictly stronger than the grep it replaces — a file with the name
	// nowhere in it fails both — and it is the DELETION half that
	// login_stamp_test.go, which drives the function directly, cannot see.
	assertLoginStampWiring(t)

	// (4) The operator document names the tier and the remedy, with the receipt
	// that tells an operator the revoke actually named somebody.
	ops := readDoc(t, "docs/OPERATIONS.md")
	for _, want := range []string{
		"Both halves are stamps re-checked at login",
		"re-stamps the role, the group snapshot, and the",
		"**no expiry column**",
		"A human demoted out of `security_admin`",
		"`DELETE /api/v1/tokens/{id}`",
		"/api/v1/sessions/revoke",
		"takes EITHER identity: the OIDC subject or the email",
		"count is the receipt",
	} {
		if !strings.Contains(ops, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("docs/OPERATIONS.md's API-token section is missing %q", want)
		}
	}

	// (5) And it is published where the SSH analogue is, as a numbered residual.
	tm := readDoc(t, "threatmodel/THREAT-MODEL.md")
	for _, want := range []string{
		"A per-user API token's role AND group snapshot are bounded-stale, not",
		"the residual narrows to a human who never signs in again",
		"The remediation exists, is the only one, and has to be invoked deliberately",
	} {
		if !strings.Contains(tm, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("threatmodel/THREAT-MODEL.md §5 does not publish the api_tokens stamp residual: missing %q", want)
		}
	}
}

// apiTokenColumns returns the migration that creates api_tokens and its column
// names.
func apiTokenColumns(t *testing.T) (string, []string) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	create := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?api_tokens\s*\((.*?)\n\);`)
	col := regexp.MustCompile(`(?m)^\s{2,}([a-z_]+)\s+[A-Za-z]`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		m := create.FindSubmatch(b)
		if m == nil {
			continue
		}
		var cols []string
		for _, c := range col.FindAllStringSubmatch(string(m[1]), -1) {
			cols = append(cols, c[1])
		}
		if len(cols) == 0 {
			t.Fatalf("%s: parsed no columns out of the api_tokens definition", e.Name())
		}
		// A later ALTER TABLE could add one; fold those in too.
		return e.Name(), append(cols, alteredAPITokenColumns(t, dir, entries)...)
	}
	t.Fatal("no migration creates api_tokens — revisit this guard")
	return "", nil
}

// alteredAPITokenColumns returns columns added to api_tokens by any later
// migration, so a column added by ALTER TABLE cannot slip past the scan.
func alteredAPITokenColumns(t *testing.T, dir string, entries []os.DirEntry) []string {
	t.Helper()
	add := regexp.MustCompile(`(?i)ALTER TABLE api_tokens\s+ADD COLUMN (?:IF NOT EXISTS )?([a-z_]+)`)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range add.FindAllStringSubmatch(string(b), -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// apiTokenUpdatedColumns returns every api_tokens column the store writes after
// mint. Captures the WHOLE SET clause (up to WHERE), not just its first
// column — an UPDATE that sets several columns in one statement (role, groups
// and groups_truncated together, since #152) must not read as touching only
// the first one named.
func apiTokenUpdatedColumns(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "store", "store_apitokens.go"))
	if err != nil {
		t.Fatalf("read store_apitokens.go: %v", err)
	}
	set := regexp.MustCompile(`(?is)UPDATE api_tokens\s+SET\s+(.*?)\s+WHERE`)
	col := regexp.MustCompile(`([a-z_]+)\s*=`)
	var out []string
	for _, m := range set.FindAllStringSubmatch(string(b), -1) {
		for _, c := range col.FindAllStringSubmatch(m[1], -1) {
			out = append(out, c[1])
		}
	}
	if len(out) == 0 {
		t.Fatal("no UPDATE api_tokens found in the store — revisit this guard rather than the docs")
	}
	return out
}

// assertLoginStampWiring reads boot_deps.go as a syntax tree and asserts the
// login re-stamp is actually WIRED, not merely mentioned:
//
//	OnLogin: func(...) { ... refreshLoginStamps(...) ... }
//	func refreshLoginStamps(ctx, st, ...) { st.RefreshSSHKeyRoles(...); st.RefreshAPITokenIdentity(...) }
//
// Both halves matter and neither implies the other. A gutted OnLogin leaves a
// perfectly good refreshLoginStamps nothing calls; a gutted refreshLoginStamps
// leaves a call that stamps nothing. Each is the whole demoted-admin bound gone,
// on one or both credential lanes, with every other test in this package green.
//
// The calls are required to be ON THE STORE PARAMETER, so a future refactor that
// keeps the names but stamps something else is not mistaken for the bound.
func assertLoginStampWiring(t *testing.T) {
	t.Helper()
	const rel = "boot_deps.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	// (a) The OIDC config's OnLogin callback calls refreshLoginStamps.
	wired, sawOnLogin := false, false
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "OnLogin" {
			return true
		}
		sawOnLogin = true
		ast.Inspect(kv.Value, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refreshLoginStamps" {
					wired = true
				}
			}
			return true
		})
		return true
	})
	if !sawOnLogin {
		t.Errorf("%s no longer sets an OnLogin callback on the OIDC config — every login-time re-stamp is gone, so "+
			"docs/OPERATIONS.md and residual #38 overstate what is bounded", rel)
	} else if !wired {
		t.Errorf("%s sets OnLogin but its body never calls refreshLoginStamps. Nothing re-stamps a demoted human's "+
			"ssh_public_keys or api_tokens rows at login, on either lane — the exact hole a grep for the method NAME "+
			"cannot see, because the name still appears in this file's interface declaration and comments", rel)
	}

	// (b) refreshLoginStamps calls BOTH store methods, on its store parameter.
	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "refreshLoginStamps" {
			decl = fn
		}
	}
	if decl == nil {
		t.Fatalf("%s no longer declares refreshLoginStamps; re-derive this guard against whatever replaced it rather "+
			"than deleting it — it is the only thing standing between a demoted admin and their outstanding "+
			"credentials", rel)
	}
	store := paramNamed(decl, "loginStampStore")
	if store == "" {
		t.Fatalf("refreshLoginStamps no longer takes a loginStampStore; re-derive this guard against its new shape")
	}
	called := map[string]bool{}
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == store {
			called[sel.Sel.Name] = true
		}
		return true
	})
	for _, hook := range []string{"RefreshSSHKeyRoles", "RefreshAPITokenIdentity"} {
		if !called[hook] {
			t.Errorf("refreshLoginStamps no longer calls %s.%s — one of the two stamps this residual COMPARES is no "+
				"longer re-checked at login, so docs/OPERATIONS.md and residual #38 need re-reading. Both lanes are "+
				"best-effort and independent: a failure of one is not a reason to drop the other", store, hook)
		}
	}
}

// paramNamed returns the name of fn's first parameter whose type is the named
// (unqualified) type, or "".
func paramNamed(fn *ast.FuncDecl, typeName string) string {
	for _, field := range fn.Type.Params.List {
		id, ok := field.Type.(*ast.Ident)
		if !ok || id.Name != typeName || len(field.Names) == 0 {
			continue
		}
		return field.Names[0].Name
	}
	return ""
}

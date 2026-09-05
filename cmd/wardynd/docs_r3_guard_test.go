// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Doc guards for the round-3 docs fix lane. Each test derives its expectation
// from the thing the prose describes — a migration loop, a route registration,
// a closed const set, a naming function — so the sentence cannot drift away
// from the code without going red. None of them reads a line number.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// readSrc reads a repo file verbatim (no whitespace folding), for the tests
// below that scan CODE rather than prose.
func readSrc(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, rel...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(b)
}

// mustSay fails when a folded document is missing a claim it has to carry.
func mustSay(t *testing.T, doc, name string, claims ...string) {
	t.Helper()
	for _, want := range claims {
		if !strings.Contains(doc, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("%s no longer states: %q", name, want)
		}
	}
}

// mustNotSay fails when a folded document has drifted back to a retired claim.
func mustNotSay(t *testing.T, doc, name string, claims ...string) {
	t.Helper()
	for _, gone := range claims {
		if strings.Contains(doc, strings.Join(strings.Fields(gone), " ")) {
			t.Errorf("%s is back to the retired claim: %q", name, gone)
		}
	}
}

// TestMigrationFailureIsDocumentedAsHalfApplied (F165) pins the upgrade
// runbook to what the migration loop actually leaves behind.
//
// The runbook told operators that per-migration transactions mean wardynd
// "refuses to boot rather than half-applying". The transaction is real and it
// bounds ONE migration: applyMigration commits the migration together with its
// schema_migrations row, and the loop returns on the first error, so a failure
// at N leaves 0..N-1 committed AND recorded. There are no down migrations, so
// the pre-upgrade dump is the only way back — the opposite of the reassurance
// an operator was reading at the moment they had to choose.
func TestMigrationFailureIsDocumentedAsHalfApplied(t *testing.T) {
	// (1) The premise, read off the loop itself: one transaction per migration,
	// the schema_migrations INSERT inside it, and a return on the first error.
	src := readSrc(t, "internal", "db", "db.go")
	apply := funcBody(t, src, "applyMigration")
	for _, want := range []string{"db.Begin(ctx)", "INSERT INTO schema_migrations", "tx.Commit(ctx)"} {
		if !strings.Contains(apply, want) {
			t.Errorf("applyMigration no longer contains %q — the doc's per-migration-atomicity premise has changed; re-read the runbook before trusting this guard", want)
		}
	}
	loop := funcBody(t, src, "migrateOn")
	if !strings.Contains(loop, "if err := applyMigration(ctx, db, name, string(data)); err != nil {\n\t\t\treturn err") {
		t.Error("migrateOn no longer returns on the first applyMigration error — if the sequence became atomic, the runbook's half-applied warning must be re-widened deliberately")
	}

	// (2) Forward-only: a down path would give the operator a second recovery
	// and the doc would owe them that sentence instead.
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "internal", "db", "migrations"))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name()), "down") {
			t.Errorf("a down migration exists (%s) — the runbook says the dump is the only rollback", e.Name())
		}
	}

	// (3) What the runbook must now say, and what it must never say again.
	doc := readDoc(t, "docs/OPERATIONS.md")
	mustSay(t, doc, "docs/OPERATIONS.md",
		"**Per-migration atomicity bounds ONE migration, not the sequence.**",
		"leaves `0…N-1` **committed and recorded**",
		"**restoring the pre-upgrade dump is the only supported recovery**",
	)
	mustNotSay(t, doc, "docs/OPERATIONS.md",
		"wardynd refuses to boot rather than half-applying",
	)
}

// TestDesktopDocSecretTierMatchesTheRouter (F224) pins the member-tier doc's
// secret claim to where the routes are actually registered.
//
// DESKTOP.md listed "secret writes" among the powers that "stay admin-only",
// in one sentence with three clauses that are still true — so nothing signalled
// that 0.7 moved this one. The write verbs now hang off the member-reachable
// router and the namespace comes from secretOwnerFromRequest.
func TestDesktopDocSecretTierMatchesTheRouter(t *testing.T) {
	routes := readSrc(t, "internal", "api", "routes.go")
	for _, verb := range []string{"Put", "Delete"} {
		if strings.Contains(routes, `operatorOnly.`+verb+`("/secrets/{name}"`) {
			t.Errorf(`%s /secrets/{name} is registered on operatorOnly again — DESKTOP.md now documents it as self-service`, strings.ToUpper(verb))
		}
		if !strings.Contains(routes, `r.`+verb+`("/secrets/{name}"`) {
			t.Errorf(`%s /secrets/{name} is no longer registered on the member-reachable router — re-check DESKTOP.md's secret paragraph`, strings.ToUpper(verb))
		}
	}
	// The namespace split and the two carve-outs the doc names, read off the
	// handlers rather than restated.
	for _, want := range []struct{ file, src string }{
		{"runs_policy.go", "func (s *Server) secretOwnerFromRequest("},
		{"secrets.go", `"?owner= is admin-only"`},
		{"secrets.go", "sinkReservedSecret(name) || name == bedrockAPIKeySecret"},
	} {
		if !strings.Contains(readSrc(t, "internal", "api", want.file), want.src) {
			t.Errorf("internal/api/%s no longer carries %q — DESKTOP.md's secret paragraph names it", want.file, want.src)
		}
	}

	doc := readDoc(t, "docs/DESKTOP.md")
	mustNotSay(t, doc, "docs/DESKTOP.md",
		"policy CRUD, secret writes and `PUT /site-config` all stay admin-only",
	)
	mustSay(t, doc, "docs/DESKTOP.md",
		"`PUT`/`DELETE /secrets/{name}` is self-service for any signed-in human",
		"`secretOwnerFromRequest` returns `\"\"` for an operator",
		"`?owner=<principal>`",
	)
	// Every Bedrock/SigV4 name the write boundary refuses to a member has to be
	// in the doc's list, derived from the constants rather than typed twice.
	for _, name := range bedrockReservedSecretNames(t) {
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("docs/DESKTOP.md's admin-only secret list omits %q, which writableSecretName refuses to a member", name)
		}
	}
}

// bedrockReservedSecretNames is the four-name set a non-operator PUT/DELETE is
// refused, read off the constants the refusal is written against.
func bedrockReservedSecretNames(t *testing.T) []string {
	t.Helper()
	src := readSrc(t, "internal", "api", "runs_bedrock.go")
	re := regexp.MustCompile(`bedrock\w*Secret\s+=\s+"([a-z0-9-]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	if len(out) < 4 {
		t.Fatalf("found %d Bedrock secret-name constants (%v) — this guard's matcher needs updating, it is checking almost nothing", len(out), out)
	}
	slices.Sort(out)
	return out
}

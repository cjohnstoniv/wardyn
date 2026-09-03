// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestMigrateDSNAdoptionIsDocumented pins the WARDYN_PG_MIGRATE_DSN runbook to
// the three code facts that make it necessary and correct.
//
// wardynd tells every single-DSN operator, in a boot NOTICE, to set
// WARDYN_PG_MIGRATE_DSN "to a separate owner/migrator role". Until this section
// existed, that instruction had no procedure anywhere in the tree: no SQL, no
// grants, and — the part that bites — no statement that the migrator must
// ALREADY OWN the existing objects. An operator who followed the advice by
// creating a fresh non-superuser "migrator" got a fail-closed boot on the first
// ALTER TABLE of the next upgrade, with no documented way out.
func TestMigrateDSNAdoptionIsDocumented(t *testing.T) {
	doc := readDoc(t, "docs/OPERATIONS.md")

	// (1) The premise: migrations that require ownership of a PRE-EXISTING
	// object. While any exist, the direction rule has to be written down.
	owned := ownershipRequiringMigrations(t)
	if len(owned) == 0 {
		t.Skip("no ALTER TABLE / CREATE OR REPLACE FUNCTION migrations remain — revisit this guard, not the doc")
	}
	for _, want := range []string{
		"### Splitting the migrator and app roles",
		"it only works one way",
		"PostgreSQL requires ownership for `ALTER TABLE` and for `CREATE OR REPLACE FUNCTION`",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public",
	} {
		if !strings.Contains(doc, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("docs/OPERATIONS.md's upgrade section is missing %q, while %d migrations still need object ownership (e.g. %s)",
				want, len(owned), strings.Join(owned[:min(3, len(owned))], ", "))
		}
	}

	// (2) The privilege model is 0007's, not a second opinion: the runbook's own
	// REVOKE line must name exactly the privileges that migration takes from
	// PUBLIC — compared on the line itself, not anywhere in the document, or a
	// privilege word used in passing elsewhere would answer for it.
	want := revokedAuditPrivileges(t, "0007_audit_least_privilege.sql")
	got := revokeLinePrivileges(t)
	if !slices.Equal(got, want) {
		t.Errorf("the runbook REVOKEs %v on audit_events but 0007_audit_least_privilege.sql revokes %v from PUBLIC — "+
			"the app role must not hold what the migration takes away", got, want)
	}

	// (3) The two boot lines the operator is told to look for must be the two
	// the daemon actually logs.
	boot, err := os.ReadFile("boot_deps.go")
	if err != nil {
		t.Fatalf("read boot_deps.go: %v", err)
	}
	for _, line := range []string{
		"the append-only guard is DDL-protected",
		"DDL protection is NOT in effect",
	} {
		if !strings.Contains(string(boot), line) {
			t.Errorf("boot_deps.go no longer logs %q — the runbook quotes it as the confirmation to look for", line)
		}
		if !strings.Contains(doc, line) {
			t.Errorf("docs/OPERATIONS.md does not quote the boot line %q, so an operator cannot tell whether the split took", line)
		}
	}
}

// ownershipRequiringMigrations returns the migrations carrying DDL that
// PostgreSQL only lets the object's OWNER run.
func ownershipRequiringMigrations(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	needsOwner := regexp.MustCompile(`(?mi)^\s*(ALTER TABLE|CREATE OR REPLACE FUNCTION)\b`)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if needsOwner.Match(b) {
			out = append(out, e.Name())
		}
	}
	return out
}

// revokeRE matches a REVOKE … ON audit_events statement and captures its
// privilege list.
var revokeRE = regexp.MustCompile(`(?mi)^\s*REVOKE\s+([A-Z, ]+?)\s+ON\s+audit_events`)

// revokedAuditPrivileges reads the privilege list migration `file` takes away
// from PUBLIC — the same list a non-owner app role must not hold.
func revokedAuditPrivileges(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "db", "migrations", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	m := revokeRE.FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s no longer REVOKEs on audit_events — revisit this guard", file)
	}
	return splitPrivileges(string(m[1]))
}

// revokeLinePrivileges reads the same list off the runbook's SQL block.
func revokeLinePrivileges(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "OPERATIONS.md"))
	if err != nil {
		t.Fatalf("read docs/OPERATIONS.md: %v", err)
	}
	m := revokeRE.FindSubmatch(b)
	if m == nil {
		t.Fatal("the upgrade runbook has no REVOKE … ON audit_events line")
	}
	return splitPrivileges(string(m[1]))
}

func splitPrivileges(list string) []string {
	var out []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

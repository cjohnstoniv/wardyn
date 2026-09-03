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

// TestAPITokenStampResidualIsPublished pins the api_tokens role/group stamp —
// its bound, and the fact that it HAS no bound — to the schema and the store
// code, and to the two documents that publish it.
//
// A token carries the role and group snapshot of the session that minted it and
// replays them on every request. The SSH-key analogue of that stamp is published
// as residual #15 and is bounded-stale: every login re-stamps the key and a TTL
// expires the override. The token stamp had neither the bound nor the residual.
// It still has no bound — and since 0.7 stamps `security_admin` verbatim, the
// unbounded window now carries governance authority, so the only thing standing
// between a demoted security admin and the profile/grant/approval surface is an
// operator remembering to revoke.
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

	// (2) Store premise: nothing re-stamps the row. Only last_used_at and
	// revoked_at are ever written after mint.
	updated := apiTokenUpdatedColumns(t)
	allowed := []string{"last_used_at", "revoked_at"}
	for _, c := range updated {
		if !slices.Contains(allowed, c) {
			t.Errorf("internal/store/store_apitokens.go now UPDATEs api_tokens.%s — if that re-stamps the role or groups, "+
				"the unbounded-staleness claim in OPERATIONS.md and residual #33 is no longer true", c)
		}
	}

	// (3) The contrast the docs draw is real: SSH keys DO get re-stamped on
	// login, which is why the token stamp is the worse of the two.
	boot, err := os.ReadFile("boot_deps.go")
	if err != nil {
		t.Fatalf("read boot_deps.go: %v", err)
	}
	if !strings.Contains(string(boot), "RefreshSSHKeyRoles") {
		t.Error("boot_deps.go no longer wires OnLogin to RefreshSSHKeyRoles — the SSH stamp may no longer be bounded either, " +
			"so the \"unlike an SSH key's\" contrast in docs/OPERATIONS.md needs re-checking")
	}

	// (4) The operator document names the tier and the remedy, with the receipt
	// that tells an operator the revoke actually named somebody.
	ops := readDoc(t, "docs/OPERATIONS.md")
	for _, want := range []string{
		"unlike an SSH key's, nothing ages it out",
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
		"A per-user API token's role and group snapshot are frozen at mint",
		"the demoted-admin window is UNBOUNDED",
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
// mint.
func apiTokenUpdatedColumns(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "store", "store_apitokens.go"))
	if err != nil {
		t.Fatalf("read store_apitokens.go: %v", err)
	}
	set := regexp.MustCompile(`(?is)UPDATE api_tokens\s+SET\s+([a-z_]+)`)
	var out []string
	for _, m := range set.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatal("no UPDATE api_tokens found in the store — revisit this guard rather than the docs")
	}
	return out
}

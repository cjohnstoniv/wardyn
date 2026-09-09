// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// migrationDocFloor is where the "every migration gets written down" convention
// begins. Below it is pre-convention history and is not ratcheted; a migration
// numbered at or above it must be named, by its FULL filename stem, in some
// tracked markdown — CHANGELOG.md for anything an operator or an API caller can
// observe, a runbook or reference page otherwise.
//
// 0038 is not arbitrary: it is the first of the thirteen consecutive migrations
// (0038-0050) that each got an entry, which is the convention this guard
// promotes from a habit into a gate.
const migrationDocFloor = 38

// undocumentedMigrations is a SHRINKING allowlist, not an exemption list. Each
// entry is a migration at or above the floor that nothing in the repo's
// markdown names — a gap that already existed when this guard was written, and
// that this guard exists to stop GROWING. The test fails in both directions:
// a new undocumented migration fails because it is not listed here, and a
// listed one that has since been documented fails too, so the list can only
// ever get shorter.
//
// 0060 is the one an operator can feel: it adds a CHECK that newly REJECTS a
// class of api_tokens write that previously succeeded.
var undocumentedMigrations = map[string]string{
	"0044_run_failure_hint":                       "adds agent_runs.failure_hint",
	"0049_oidc_session_revocations":               "adds the oidc_session_revocations table behind POST /sessions/revoke",
}

// TestEveryMigrationIsDocumented is the ratchet nothing supplied: a migration is
// the one change an operator cannot roll back (migrations are forward-only, and
// the upgrade runbook's only remedy is a pg_dump taken first), yet the sole
// migration-reading test in this package compares CHECK-constraint vocabularies
// against Go constants and no test read a document at all. The convention held
// for thirteen consecutive migrations and then lapsed for a whole release cycle,
// silently, because a convention with no gate is a preference.
func TestEveryMigrationIsDocumented(t *testing.T) {
	root := repoRootFromDB(t)
	docs := shippedMarkdown(t, root)
	if len(docs) < 10 {
		t.Fatalf("found %d shipped markdown files under %s — the reader, not the docs, is what changed", len(docs), root)
	}
	blob := strings.Join(docs, "\n")

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var missing, staleAllowlist []string
	seen := map[string]bool{}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		stem := strings.TrimSuffix(name, ".sql")
		num, err := strconv.Atoi(stem[:strings.Index(stem, "_")])
		if err != nil {
			t.Errorf("migration %q does not start with a number_ prefix", name)
			continue
		}
		if num < migrationDocFloor {
			continue
		}
		checked++
		documented := strings.Contains(blob, stem)
		_, allowed := undocumentedMigrations[stem]
		seen[stem] = true
		switch {
		case !documented && !allowed:
			missing = append(missing, stem)
		case documented && allowed:
			staleAllowlist = append(staleAllowlist, stem)
		}
	}
	if checked == 0 {
		t.Fatalf("checked 0 migrations at or above %04d — the floor or the embed is wrong", migrationDocFloor)
	}
	sort.Strings(missing)
	sort.Strings(staleAllowlist)
	if len(missing) > 0 {
		t.Errorf("no tracked markdown names these migration(s): %s\n"+
			"a migration is forward-only and its only rollback is a pg_dump the operator had to take FIRST — "+
			"add a CHANGELOG.md entry (say what an operator or an API caller can now observe, and name any write "+
			"that newly fails), or a runbook line for a purely internal one", strings.Join(missing, ", "))
	}
	if len(staleAllowlist) > 0 {
		t.Errorf("undocumentedMigrations still lists %s, but the repo now documents them — "+
			"drop those entries; the allowlist may only shrink", strings.Join(staleAllowlist, ", "))
	}
	for stem := range undocumentedMigrations {
		if !seen[stem] {
			t.Errorf("undocumentedMigrations names %q, which is not a migration at or above the floor — "+
				"a stale entry silently exempts nothing and hides the next real one", stem)
		}
	}
}

// repoRootFromDB walks up from this package to the module root.
func repoRootFromDB(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("no go.mod found walking up from internal/db")
	return ""
}

// docMarkdownRoots are the SHIPPED documentation trees, named explicitly rather
// than discovered by walking the repo. A walk is what makes this kind of guard
// pass by accident: a working tree carries untracked scratch and review
// artifacts that a `git archive` of the same commit does not, so a migration
// "documented" only by somebody's local notes would read as covered here and
// uncovered for everyone else. These are the paths a release actually ships.
var docMarkdownRoots = []string{"docs", "threatmodel", "examples", "deploy"}

// shippedMarkdown reads every .md the repo SHIPS: the top-level documents
// (CHANGELOG.md and its archive, README, the runbooks that live at the root)
// plus the documentation trees above.
func shippedMarkdown(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	read := func(path string) {
		if b, err := os.ReadFile(path); err == nil {
			out = append(out, string(b))
		}
	}
	top, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, e := range top {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			read(filepath.Join(root, e.Name()))
		}
	}
	for _, sub := range docMarkdownRoots {
		dir := filepath.Join(root, sub)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			read(path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return out
}

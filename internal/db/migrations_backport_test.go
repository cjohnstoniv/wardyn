// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// What a statement may be when it runs a second time over a database that
// already has its effect. An ALTER TABLE passes only if every clause is an
// ADD COLUMN IF NOT EXISTS.
var (
	idempotentStatementRe = regexp.MustCompile(`(?is)^(CREATE\s+(UNIQUE\s+)?\w+\s+IF\s+NOT\s+EXISTS|CREATE\s+OR\s+REPLACE|DROP\s+\w+\s+IF\s+EXISTS)\b`)
	alterTableRe          = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+\S+\s+ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\b`)
	addClauseRe           = regexp.MustCompile(`(?i)\bADD\b`)
	addColumnIfNotRe      = regexp.MustCompile(`(?i)\bADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\b`)
	alterOtherClauseRe    = regexp.MustCompile(`(?i)\b(DROP|ALTER\s+COLUMN|RENAME|SET|VALIDATE)\b`)
	lineCommentRe         = regexp.MustCompile(`--[^\n]*`)
)

func sqlStatements(sql string) []string {
	var out []string
	for _, s := range strings.Split(lineCommentRe.ReplaceAllString(sql, ""), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func statementIsIdempotent(stmt string) bool {
	if strings.HasPrefix(strings.ToUpper(stmt), "ALTER") {
		return alterTableRe.MatchString(stmt) && !alterOtherClauseRe.MatchString(stmt) &&
			len(addClauseRe.FindAllString(stmt, -1)) == len(addColumnIfNotRe.FindAllString(stmt, -1))
	}
	return idempotentStatementRe.MatchString(stmt)
}

// TestBackportedMigrationsStayIdempotent: a migration a release branch shipped
// under a different number (0.7.12's 0065_secret_envelope_v1.sql is this
// branch's 0069_secret_envelope_v1.sql) is re-run in full on every upgrade from
// that release, because Migrate tracks filenames. Each of its statements must
// therefore re-run cleanly over its own effect: an ADD COLUMN without IF NOT
// EXISTS, or a RENAME, fails every upgrader's boot. A later change to the same
// table (say a CHECK over enc_version for v2 rows) belongs in a new migration,
// which every database then runs exactly once.
func TestBackportedMigrationsStayIdempotent(t *testing.T) {
	const release = "testdata/migrations-0.7.12"
	shipped, err := os.ReadDir(release)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	byStem := map[string][]string{}
	have := map[string]bool{}
	for _, e := range ours {
		have[e.Name()] = true
		_, stem, _ := strings.Cut(e.Name(), "_")
		byStem[stem] = append(byStem[stem], e.Name())
	}
	renumbered := 0
	for _, e := range shipped {
		if have[e.Name()] {
			continue
		}
		_, stem, _ := strings.Cut(e.Name(), "_")
		match := byStem[stem]
		if len(match) != 1 {
			t.Errorf("%s/%s has %d counterparts here (%v): a release migration with no counterpart leaves its schema missing on this branch", release, e.Name(), len(match), match)
			continue
		}
		renumbered++
		sql, err := migrationFS.ReadFile("migrations/" + match[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range sqlStatements(string(sql)) {
			if !statementIsIdempotent(stmt) {
				t.Errorf("%s re-runs on every upgrade from a database that recorded %s, and this statement fails or changes something the second time: %q. Keep it to IF NOT EXISTS / IF EXISTS forms; put a new constraint in a new migration", match[0], e.Name(), stmt)
			}
		}
	}
	if renumbered == 0 {
		t.Fatal("no renumbered release migration found; the envelope backport is 0065_secret_envelope_v1.sql in the fixture")
	}
}

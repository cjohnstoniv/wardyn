// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestRetiredMigrationsCoverEveryReleasedName is the pure-text counterpart to
// the PG downgrade-refusal test: it does not touch Postgres at all, it proves
// retiredMigrations' CLAIM — that its keys plus the currently-shipped names
// account for every migration filename ANY v0.7.0-v0.7.12 release ever shipped.
//
// testdata/released_migrations_v0.7.txt is the checked-in, tag-derived list
// (the union of `git ls-tree --name-only <tag> internal/db/migrations/` over
// every v0.7.* tag). A name that is neither on disk today nor a
// retiredMigrations key means a real 0.7.x database has a schema_migrations
// row Migrate() would refuse as "unknown" — exactly the #675 bug — so this
// fails the day a rename lands without a matching retiredMigrations entry.
func TestRetiredMigrationsCoverEveryReleasedName(t *testing.T) {
	data, err := os.ReadFile("testdata/released_migrations_v0.7.txt")
	if err != nil {
		t.Fatalf("read testdata/released_migrations_v0.7.txt: %v", err)
	}
	var released []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			released = append(released, line)
		}
	}
	if len(released) == 0 {
		t.Fatal("testdata/released_migrations_v0.7.txt is empty; the tag-derived list generation is broken")
	}

	shipped := readMigrationNames(t) // db_test.go: sorted migrations/*.sql, same source Migrate() reads
	for _, name := range released {
		if slices.Contains(shipped, name) {
			continue
		}
		if _, retired := retiredMigrations[name]; retired {
			continue
		}
		t.Errorf("released migration %q is neither shipped today nor listed in retiredMigrations; "+
			"a v0.7.x database that applied it will be refused as an unknown (newer-wardynd) migration on upgrade", name)
	}
}

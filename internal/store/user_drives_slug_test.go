// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

// THE TWO HALVES OF THE SLUG REFUSAL THAT NEED NO DATABASE.
//
// The refusal itself is a partial unique index (migration 0061), so proving it
// refuses needs Postgres — user_drives_slug_pg_test.go does that. Two things
// about it are decided in Go and are pinned here, because both fail SILENTLY:
//
//  1. WHICH SENTINEL a unique violation becomes. UNIQUE(name) and the 0061 index
//     are different problems with different remedies, and folding them into one
//     ErrConflict hands an admin "a user drive named %q already exists" for a
//     name that is genuinely free.
//  2. THAT THE INDEX IS STILL CALLED WHAT THE MAPPING THINKS. The link between
//     the two is a string. Rename the index in a later migration and the mapping
//     degrades to the generic message with every test still green.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestUserDriveUniqueConflictTellsTheTwoRefusalsApart(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{
			name: "the 0061 slug index: the name is free and its fold is not",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: driveNameSlugIndex},
			want: ErrDriveSlugConflict,
		},
		{
			name: "UNIQUE(name): the name itself is taken",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "user_drives_name_key"},
			want: ErrConflict,
		},
		{
			name: "some other unique index is still a 409, not a guess",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "user_drives_something_later"},
			want: ErrConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := userDriveUniqueConflict(tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("userDriveUniqueConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
			if !errors.Is(got, ErrConflict) {
				t.Errorf("%v does not satisfy errors.Is(err, ErrConflict); the API's 409 branch would miss it and 500", got)
			}
			// The slug refusal must be tellable APART, or the wrap has bought
			// nothing: this is the assertion the generic mapping fails.
			if tc.want == ErrDriveSlugConflict && !errors.Is(got, ErrDriveSlugConflict) {
				t.Errorf("a slug collision reported as %v; the admin is sent looking for a drive with that name, "+
					"and there is not one — the name is free, its fold is not", got)
			}
			if tc.want == ErrConflict && errors.Is(got, ErrDriveSlugConflict) {
				t.Errorf("a %s violation reported as a SLUG collision; the remedy printed would be the wrong one",
					tc.err.(*pgconn.PgError).ConstraintName)
			}
		})
	}

	// Not a unique violation at all: nil, so the caller falls through to the
	// paths that mean something else entirely (the cross-row home-naming guard
	// returns no rows, not a 23505).
	for _, err := range []error{
		nil,
		errors.New("connection reset"),
		&pgconn.PgError{Code: "23514", ConstraintName: "user_drives_backend_check"},
	} {
		if got := userDriveUniqueConflict(err); got != nil {
			t.Errorf("userDriveUniqueConflict(%v) = %v, want nil — swallowing it as a conflict would report a 409 "+
				"for a write that failed for another reason", err, got)
		}
	}
}

func TestDriveNameSlugIndexIsTheOneAMigrationCreates(t *testing.T) {
	dir := filepath.Join("..", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the migrations directory: %v", err)
	}
	var found string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		body := string(b)
		if strings.Contains(body, "CREATE UNIQUE INDEX") && strings.Contains(body, driveNameSlugIndex) {
			found = e.Name()
			// The index has to be on the column the store WRITES, and it has to
			// leave host_path out: a share's object name is <host_root>/<home>
			// and carries no slug, so folding two share names together collides
			// with nothing.
			if !strings.Contains(body, "name_slug") {
				t.Errorf("%s creates %s but not on name_slug; the store writes types.DriveSlug(name) into that "+
					"column and nothing else", e.Name(), driveNameSlugIndex)
			}
			if !strings.Contains(body, "backend <> 'host_path'") {
				t.Errorf("%s's %s is no longer scoped to the backends whose object name Wardyn MINTS. On host_path "+
					"the object is <host_root>/<home> with no slug in it, so this would refuse a legal pair of "+
					"shares", e.Name(), driveNameSlugIndex)
			}
		}
	}
	if found == "" {
		t.Fatalf("no migration creates a unique index named %q, but userDriveUniqueConflict maps that exact name to "+
			"ErrDriveSlugConflict. Renaming the index in SQL without renaming the constant here silently degrades "+
			"every slug collision to the generic \"a user drive named %%q already exists\" — for a name that is free.",
			driveNameSlugIndex)
	}
}

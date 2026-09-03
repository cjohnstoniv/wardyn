// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for 0059's partial unique index on user_drive_grants (drive_id,
// home_override). The store's upsert carries an application guard for the same
// collision and keeps it; this pins the race-free floor UNDER that guard, which
// is the half a second concurrent writer cannot slip past.
//
// Runs against its own throwaway schema, so no lane's user_drive_grants is
// touched.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func insertDrive(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_drives (id, name, backend) VALUES ($1,$2,'docker_volume')`, id, name); err != nil {
		t.Fatalf("insert drive %s: %v", name, err)
	}
	return id
}

func insertGrant(pool *pgxpool.Pool, drive uuid.UUID, subject, override string) error {
	_, err := pool.Exec(context.Background(),
		`INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, home_override)
		 VALUES ($1,'user',$2,$3,$4)`, uuid.New(), subject, drive, override)
	return err
}

func TestPG_OneHomeOverridePerDriveIsEnforcedByTheDatabase(t *testing.T) {
	pool, _ := probeSchemaPool(t)

	driveA := insertDrive(t, pool, "nas-a")
	driveB := insertDrive(t, pool, "nas-b")

	if err := insertGrant(pool, driveA, "bob@corp.example", "bsmith"); err != nil {
		t.Fatalf("first grant: %v", err)
	}

	// The collision: a SECOND user-tier grant on the SAME drive naming the SAME
	// directory. Both principals would mount /home/agent/drive over one storage
	// object, and the per-user subdirectory the isolation rests on is gone.
	err := insertGrant(pool, driveA, "eve@corp.example", "bsmith")
	if err == nil {
		t.Fatal("two user-tier grants on one drive were allowed to carry the same home_override — " +
			"two principals now share one directory, and nothing in the database prevents it")
	}
	if got := sqlState(err); got != "23505" {
		t.Errorf("second grant failed with SQLSTATE %q (%v), want 23505 unique_violation", got, err)
	}

	t.Run("the same directory on a DIFFERENT drive is fine", func(t *testing.T) {
		// Two drives are two filesystems; the name only has to be unique within
		// the storage object it names.
		if err := insertGrant(pool, driveB, "eve@corp.example", "bsmith"); err != nil {
			t.Errorf("same override on another drive was refused: %v — the index is keyed on (drive_id, home_override), "+
				"not on home_override alone", err)
		}
	})

	t.Run("grants with no override do not collide with each other", func(t *testing.T) {
		// The empty string is the ordinary case: no override, derive the name
		// from the subject. A NON-partial index would make the SECOND grant on
		// any drive fail, i.e. break the feature for everyone who never typed a
		// directory name.
		for _, subject := range []string{"a@corp.example", "b@corp.example", "c@corp.example"} {
			if err := insertGrant(pool, driveA, subject, ""); err != nil {
				t.Fatalf("grant with no home_override for %s was refused: %v — the index must be PARTIAL on "+
					"home_override <> ''", subject, err)
			}
		}
	})

	t.Run("home_override is NOT NULL, so the partial predicate cannot silently exclude a row", func(t *testing.T) {
		// The predicate `WHERE home_override <> ''` is only sound while "unset"
		// is '' rather than NULL: a NULL would test as unknown, fall out of the
		// index, and leave those rows unconstrained with nothing to show for it.
		// 0054 declares the column NOT NULL DEFAULT ''; this asserts the live
		// catalog still agrees, so a later ALTER that drops NOT NULL fails here
		// rather than silently widening what 0059 permits.
		var notNull bool
		if err := pool.QueryRow(context.Background(), `
			SELECT attnotnull FROM pg_attribute
			 WHERE attrelid = 'user_drive_grants'::regclass AND attname = 'home_override'`).Scan(&notNull); err != nil {
			t.Fatalf("read home_override nullability: %v", err)
		}
		if !notNull {
			t.Error("user_drive_grants.home_override is nullable; 0059's `WHERE home_override <> ''` predicate " +
				"silently excludes every NULL row from the uniqueness it is supposed to enforce")
		}
	})
}

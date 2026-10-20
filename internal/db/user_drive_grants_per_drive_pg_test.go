// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_UserDriveGrantsPerDriveUpgrade applies 0169 over a database holding
// 0.8 grants (one row per subject) and requires every caller's single-drive
// resolution to be byte-identical after it: the drive behind /home/agent/drive
// does not move on upgrade. Then the new key takes a subject's second drive as
// a second row. Guarded by WARDYN_TEST_PG.
func TestPG_UserDriveGrantsPerDriveUpgrade(t *testing.T) {
	pool, _ := db.PartialSchemaPool(t, "0169")
	ctx := context.Background()
	st := store.PG{Pool: pool}

	drive := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO user_drives (id, name, backend, home_template, size_mib, writable)
			VALUES ($1, $2, 'docker_volume', 'hash', 1024, true)`, id, name); err != nil {
			t.Fatalf("seed drive %s: %v", name, err)
		}
		return id
	}
	grant := func(subjectType, subject string, driveID uuid.UUID, priority int, enabled bool) {
		if _, err := pool.Exec(ctx, `INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, priority, enabled)
			VALUES ($1, $2, $3, $4, $5, $6)`, uuid.New(), subjectType, subject, driveID, priority, enabled); err != nil {
			t.Fatalf("seed grant %s %s: %v", subjectType, subject, err)
		}
	}
	alpha, beta, gamma := drive("alpha"), drive("beta"), drive("gamma")
	grant("user", "alice", beta, 0, true)
	grant("user", "bob", alpha, 0, false) // paused: still the winner
	grant("group", "eng", alpha, 5, true)
	grant("group", "ops", gamma, 9, true)
	grant("all", "*", gamma, 0, true)

	callers := []struct {
		users, groups []string
	}{
		{[]string{"alice"}, []string{"eng"}},
		{[]string{"bob"}, []string{"eng", "ops"}},
		{[]string{"carol"}, []string{"eng", "ops"}},
		{[]string{"dave"}, nil},
	}
	resolveAll := func() []string {
		var out []string
		for _, c := range callers {
			d, g, tier, err := st.ResolveUserDrive(ctx, c.users, c.groups, "")
			if err != nil {
				t.Fatalf("resolve %v: %v", c.users, err)
			}
			b, err := json.Marshal(map[string]any{"drive": d, "grant": g, "tier": tier})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(b))
		}
		return out
	}
	// The store under test is today's and reads 0209's columns; a pre-0169
	// schema gets them as 0209 adds them, so only 0169 moves under it.
	for _, q := range []string{
		`ALTER TABLE user_drives ADD COLUMN team BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE user_drive_grants ADD COLUMN members_can_write BOOLEAN NOT NULL DEFAULT FALSE`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("add a column today's store reads: %v", err)
		}
	}
	before := resolveAll()

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate over 0.8 grants: %v", err)
	}
	after := resolveAll()
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("caller %v resolves differently after 0169:\n before %s\n after  %s", callers[i].users, before[i], after[i])
		}
	}

	// The legacy drive is also the first of the per-drive winners.
	ws, err := st.ResolveUserDrives(ctx, []string{"carol"}, []string{"eng", "ops"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Drive.ID != gamma {
		t.Errorf("carol's drives = %+v, want gamma (ops, priority 9) first and alpha second", ws)
	}

	// The new key: alice's second drive, added, is a second row.
	if _, err := st.AddUserDriveGrant(ctx, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: "alice", DriveID: alpha, Enabled: true,
	}, true); err != nil {
		t.Fatalf("grant alice a second drive: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_drive_grants WHERE subject_type = 'user' AND subject = 'alice'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("alice holds %d grant rows, want 2", n)
	}
}

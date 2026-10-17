// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestMigrate_PurgeAttributionBackfillReopensOnlyTheTwoDoneSteps: the back-fill statement reopens a
// finished purge's runners and service_identities steps, and nothing else: not its other steps, a
// pending step, or a suspend job's step of the same name.
func TestMigrate_PurgeAttributionBackfillReopensOnlyTheTwoDoneSteps(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	var identity uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (issuer, email_lower) VALUES ('https://dex.example', $1) RETURNING id`,
		uuid.NewString()+"@corp.example").Scan(&identity); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	type job struct{ kind, step, state, want string }
	jobs := []job{
		{"purge", "runners", "done", "pending"},
		{"purge", "service_identities", "done", "pending"},
		{"purge", "invitations", "done", "done"},
		{"suspend", "runners", "done", "done"},
	}
	for _, j := range jobs {
		if _, err := pool.Exec(ctx, `INSERT INTO deprovision_jobs (identity_id, kind, step, state) VALUES ($1, $2, $3, $4)`,
			identity, j.kind, j.step, j.state); err != nil {
			t.Fatalf("seed %s/%s: %v", j.kind, j.step, err)
		}
	}
	sql, err := migrationFS.ReadFile("migrations/0180_purge_attribution_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply the back-fill: %v", err)
	}
	for _, j := range jobs {
		var got string
		if err := pool.QueryRow(ctx, `SELECT state FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND step = $3`,
			identity, j.kind, j.step).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != j.want {
			t.Errorf("%s/%s after the back-fill = %s, want %s", j.kind, j.step, got, j.want)
		}
	}
}

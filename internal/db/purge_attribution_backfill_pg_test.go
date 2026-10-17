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
	pool := pgPoolIsolated(t)
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
	sql, err := migrationFS.ReadFile("migrations/0186_purge_attribution_backfill.sql")
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

// TestMigrate_PurgeAttributionBackfillGivesAnEarlierPurgeTheNewSteps: a person purged before the
// runners, invitations and service-identities steps existed has no ledger rows for them, so the
// back-fill adds the three as pending, once: not for a person whose purge never finished or who was
// never purged, and not over a row the person already has.
func TestMigrate_PurgeAttributionBackfillGivesAnEarlierPurgeTheNewSteps(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	seed := func(purged bool, steps ...[2]string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		purgedAt := "NULL"
		if purged {
			purgedAt = "now()"
		}
		if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (issuer, email_lower, purged_at) VALUES ('https://dex.example', $1, `+purgedAt+`) RETURNING id`,
			uuid.NewString()+"@corp.example").Scan(&id); err != nil {
			t.Fatalf("seed identity: %v", err)
		}
		for _, st := range steps {
			if _, err := pool.Exec(ctx, `INSERT INTO deprovision_jobs (identity_id, kind, step, state) VALUES ($1, 'purge', $2, $3)`, id, st[0], st[1]); err != nil {
				t.Fatalf("seed %s: %v", st[0], err)
			}
		}
		return id
	}
	finished := [][2]string{{"workspaces", "done"}, {"grants", "done"}, {"audit_deprovision", "done"}}
	pre090 := seed(true, finished...)
	unfinished := seed(true, [2]string{"workspaces", "done"}, [2]string{"audit_deprovision", "pending"})
	notPurged := seed(false, finished...)
	has090 := seed(true, append([][2]string{{"runners", "done"}, {"invitations", "done"}}, finished...)...)

	sql, err := migrationFS.ReadFile("migrations/0186_purge_attribution_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // the migration runs once; running it again changes nothing more
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply the back-fill: %v", err)
		}
	}
	states := func(id uuid.UUID) map[string]string {
		rows, err := pool.Query(ctx, `SELECT step, state FROM deprovision_jobs WHERE identity_id = $1 AND kind = 'purge'`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var step, state string
			if err := rows.Scan(&step, &state); err != nil {
				t.Fatal(err)
			}
			out[step] = state
		}
		return out
	}
	got := states(pre090)
	for _, step := range []string{"runners", "invitations", "service_identities"} {
		if got[step] != "pending" {
			t.Errorf("a person purged before 0.9.0 has %s = %q, want pending: nothing would re-run their purge", step, got[step])
		}
	}
	if got["audit_deprovision"] != "done" || len(got) != 6 {
		t.Errorf("the earlier purge's other rows changed: %v", got)
	}
	if g := states(unfinished); len(g) != 2 {
		t.Errorf("a purge that never finished gained steps: %v", g)
	}
	if g := states(notPurged); len(g) != 3 {
		t.Errorf("a person never purged gained steps: %v", g)
	}
	g := states(has090)
	if g["invitations"] != "done" || g["runners"] != "pending" || g["service_identities"] != "pending" {
		t.Errorf("a person purged under 0.9.x ended with %v, want invitations kept done, runners reopened, service_identities added", g)
	}
}

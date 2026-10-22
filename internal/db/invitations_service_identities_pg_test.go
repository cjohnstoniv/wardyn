// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const invitationsMigration = "0157"

// TestMigration0157_TokensAndRunsUnchanged applies the invitations migration over a 0156
// database that holds tokens and runs: every api_tokens and agent_runs row is byte-identical
// afterwards, and the new tables exist and are empty.
func TestMigration0157_TokensAndRunsUnchanged(t *testing.T) {
	pool, schema := partialSchemaPool(t, invitationsMigration)
	ctx := context.Background()

	for i, role := range []string{"user", "admin", "security_admin"} {
		pgExec(t, pool, `INSERT INTO api_tokens (id, principal, email, token_sha256, role, name, minted_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New(), "person-"+role, role+"@example.com", uuid.NewString(), role, "token-"+role, []string{"", "admin-token", "someone"}[i])
	}
	for _, by := range []string{"person-user", "person-admin"} {
		pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
			VALUES ($1, $2, 'claude-code', 'repo', 'CC1', 'STOPPED', 'spiffe://x', 'docker')`, uuid.New(), by)
	}
	// Later migrations in the same Migrate add columns (0159, 0160); they are not what 0157 may
	// change, so each row is compared on the columns the table had before the migration.
	columns := func(table string) []string {
		t.Helper()
		return pgScalar[[]string](t, pool, `SELECT array_agg(column_name::text ORDER BY column_name) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = '`+table+`'`)
	}
	tokenCols, runCols := columns("api_tokens"), columns("agent_runs")
	// 0201 renames governance_profile_id to unit_id; it is not what 0157 may change either.
	runCols = slices.DeleteFunc(runCols, func(c string) bool { return c == "governance_profile_id" })
	snapshot := func(table string, cols []string) string {
		t.Helper()
		var parts []string
		rows, err := pool.Query(ctx, `SELECT (SELECT jsonb_object_agg(k, v) FROM jsonb_each(to_jsonb(x)) AS e(k, v) WHERE k = ANY($1))::text
			FROM `+table+` x ORDER BY id`, cols)
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "\n")
	}
	tokensBefore, runsBefore := snapshot("api_tokens", tokenCols), snapshot("agent_runs", runCols)
	if tokensBefore == "" || runsBefore == "" {
		t.Fatalf("fixture seeded nothing in %s", schema)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying 0157 over a seeded 0156 database: %v", err)
	}
	if got := snapshot("api_tokens", tokenCols); got != tokensBefore {
		t.Errorf("api_tokens changed:\nbefore %s\nafter  %s", tokensBefore, got)
	}
	if got := snapshot("agent_runs", runCols); got != runsBefore {
		t.Errorf("agent_runs changed:\nbefore %s\nafter  %s", runsBefore, got)
	}
	for _, table := range []string{"person_invitations", "service_identities", "api_token_moves"} {
		if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM `+table); n != 0 {
			t.Errorf("%s holds %d rows after the migration, want none", table, n)
		}
	}
}

// TestMigration0157_ServicePrefixAbortsNamingTheTable seeds a service: principal in each table the
// collision check reads: the migration aborts, names that table and leaves nothing behind.
func TestMigration0157_ServicePrefixAbortsNamingTheTable(t *testing.T) {
	seeds := []struct{ table, sql string }{
		{"principal_identities", `INSERT INTO principal_identities (principal, issuer) VALUES ('service:ci', 'https://idp.example')`},
		{"people", `INSERT INTO people (principal, created_by) VALUES ('Service:ci', 'admin')`},
		{"api_tokens", `INSERT INTO api_tokens (id, principal, token_sha256) VALUES (gen_random_uuid(), 'service:ci', 'h')`},
		{"ssh_public_keys", `INSERT INTO ssh_public_keys (fingerprint, principal, public_key) VALUES ('fp', 'service:ci', 'k')`},
		{"agent_runs", `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
			VALUES (gen_random_uuid(), 'service:ci', 'a', 'r', 'CC1', 'STOPPED', 's', 'docker')`},
		{"workspaces", `INSERT INTO workspaces (id, name, owned_by, attachments) VALUES (gen_random_uuid(), 'w', 'service:ci', '[]')`},
		{"secrets", `INSERT INTO secrets (name, ciphertext, owned_by) VALUES ('n', '\x00', 'service:ci')`},
		{"capability_grants", `INSERT INTO capability_grants (id, subject_type, subject, capability, value, effect)
			VALUES (gen_random_uuid(), 'user', 'service:ci', 'c', 'v', 'allow')`},
		{"governance_assignments", `WITH p AS (INSERT INTO governance_profiles (id, name, ceiling) VALUES (gen_random_uuid(), 'g', '{}') RETURNING id)
			INSERT INTO governance_assignments (id, subject_type, subject, profile_id) SELECT gen_random_uuid(), 'user', 'service:ci', id FROM p`},
	}
	for _, seed := range seeds {
		t.Run(seed.table, func(t *testing.T) {
			pool, _ := partialSchemaPool(t, invitationsMigration)
			pgExec(t, pool, seed.sql)
			err := Migrate(context.Background(), pool)
			if err == nil {
				t.Fatalf("Migrate succeeded over a service: principal in %s", seed.table)
			}
			if !strings.Contains(err.Error(), seed.table+".") {
				t.Errorf("error does not name %s: %v", seed.table, err)
			}
			if pgScalar[bool](t, pool, `SELECT to_regclass('person_invitations') IS NOT NULL`) {
				t.Error("the aborted migration left person_invitations behind")
			}
		})
	}
}

// TestMigration0157_ServicePrefixInANonUserSubjectDoesNotAbort: a group or user-type subject is no
// principal, so only the user subject type of the two grant tables counts as a collision.
func TestMigration0157_ServicePrefixInANonUserSubjectDoesNotAbort(t *testing.T) {
	pool, _ := partialSchemaPool(t, invitationsMigration)
	for _, subjectType := range []string{"group", "user_type"} {
		pgExec(t, pool, `INSERT INTO capability_grants (id, subject_type, subject, capability, value, effect)
			VALUES (gen_random_uuid(), $1, 'service:ci', 'c', 'v', 'allow')`, subjectType)
		pgExec(t, pool, `WITH p AS (INSERT INTO governance_profiles (id, name, ceiling) VALUES (gen_random_uuid(), $1, '{}') RETURNING id)
			INSERT INTO governance_assignments (id, subject_type, subject, profile_id) SELECT gen_random_uuid(), $1, 'service:ci', id FROM p`, subjectType)
	}
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate over service: subjects of a non-user type: %v", err)
	}
}

// TestMigration0157_AuditActorTypeAcceptsService: the widened check admits a service actor and
// still refuses a name outside the closed set.
func TestMigration0157_AuditActorTypeAcceptsService(t *testing.T) {
	pool, _ := partialSchemaPool(t, invitationsMigration)
	appendAs := func(actorType string) error {
		_, err := pool.Exec(context.Background(), `SELECT audit_append(gen_random_uuid(), now(), NULL, $1, 'service:ci', 'run.create', '', 'success', '', NULL)`, actorType)
		return err
	}
	if err := appendAs("service"); err == nil {
		t.Fatal("a service actor was accepted before the migration widened the check")
	}
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := appendAs("service"); err != nil {
		t.Errorf("a service actor was refused after the migration: %v", err)
	}
	if err := appendAs("robot"); err == nil {
		t.Error("an actor type outside the closed set was accepted")
	}
}

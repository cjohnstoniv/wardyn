// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration test for migration 0029 (workspace composition): seed pre-0029,
// legacy-shaped `workspaces` rows against a database that has every migration
// EXCEPT 0029 applied, apply exactly 0029, and assert the resulting
// sources/base_image/status shape for each of the three retired kinds
// (local_dir/repo/container), plus that an in-flight `scanning` row keeps its
// active_run_id across the migration.
//
// This deliberately does NOT use runsPGPool/pgPool (store_runs_pg_test.go /
// internal/db/migrate_pg_test.go), which always migrate a database to HEAD:
// observing 0029's before/after transform requires a database frozen one
// migration short of it, so a pre-0029 row can still be inserted using the
// columns 0029 goes on to drop. Each (sub)test shares ONE throwaway
// CREATE DATABASE (dropped on cleanup), mirroring the single-fixture,
// multi-t.Run style already used by TestAuditEventsAppendOnlyEnforcedLive.
// Guarded by WARDYN_TEST_PG (see store_pg_test.go).
package store_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// migrationsDir is internal/db/migrations relative to this package's own
// directory (internal/store) -- `go test` runs with the package dir as cwd.
const migrationsDir = "../db/migrations"

const workspaceSourcesMigration = "0029_workspace_sources.sql"

// migrationFileNames returns every *.sql filename under migrationsDir, sorted
// lexically (== numeric order for the zero-padded NNNN prefix), mirroring
// db.Migrate's own discovery + application order.
func migrationFileNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// execMigrationFile reads and executes ONE internal/db/migrations/*.sql file
// against pool. Zero-arg Exec, mirroring db.go's applyMigration: a migration
// file is plain multi-statement SQL text (not a single parameterized query),
// and pgx's simple-query path (used when there are no args) runs it as such.
func execMigrationFile(t *testing.T, pool *pgxpool.Pool, filename string) {
	t.Helper()
	sqlBytes, err := os.ReadFile(filepath.Join(migrationsDir, filename))
	if err != nil {
		t.Fatalf("read migration %s: %v", filename, err)
	}
	if _, err := pool.Exec(context.Background(), string(sqlBytes)); err != nil {
		t.Fatalf("apply migration %s: %v", filename, err)
	}
}

// throwawayDatabase creates a fresh, empty database on the WARDYN_TEST_PG
// server (dropped on test cleanup) and returns a pool connected to it. No
// migrations are applied -- callers apply exactly the ones they need via
// execMigrationFile. A separate CREATE DATABASE (rather than a schema +
// search_path within the shared one) sidesteps pgxpool's per-call connection
// routing: a pool built from a ".../dbname" DSN unambiguously targets that
// database on EVERY physical connection it opens, whereas a shared-database
// "SET search_path" sent through one pooled call could land on a different
// underlying connection than the next statement. Skips cleanly when
// WARDYN_TEST_PG is unset.
func throwawayDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed migration test")
	}
	ctx := context.Background()

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}

	name := "wardyn_mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	// Registered before the pool's own cleanup below, so LIFO ordering closes
	// the target pool's connections FIRST and drops the database second (a
	// database with a connected client cannot be dropped).
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = admin.Exec(cctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(cctx, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to throwaway database %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// preCompositionDatabase returns a throwaway database with every migration
// STRICTLY BEFORE 0029 applied -- the schema exactly as it stood before the
// workspace-composition migration, so a test can insert a legacy-shaped
// workspaces row (kind/source/ref/default_target/writable columns 0029 later
// drops).
func preCompositionDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := throwawayDatabase(t)
	for _, name := range migrationFileNames(t) {
		if name >= workspaceSourcesMigration {
			break
		}
		execMigrationFile(t, pool, name)
	}
	return pool
}

// legacyWorkspaceRow is one pre-0029 workspaces row, in the 0008-0028 column
// shape. Zero-value string fields ride the columns' own NOT NULL DEFAULT of
// an empty string.
type legacyWorkspaceRow struct {
	id            uuid.UUID
	name          string
	kind          string
	source        string
	ref           string
	defaultTarget string
	writable      bool
	profile       string // raw JSON, or "" for SQL NULL
	status        string
	activeRunID   *uuid.UUID
}

func seedLegacyWorkspace(t *testing.T, pool *pgxpool.Pool, row legacyWorkspaceRow) {
	t.Helper()
	var profileArg any
	if row.profile != "" {
		profileArg = []byte(row.profile)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO workspaces (id, name, kind, source, ref, default_target, writable, profile, status, active_run_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		row.id, row.name, row.kind, row.source, row.ref, row.defaultTarget, row.writable, profileArg, row.status, row.activeRunID,
	); err != nil {
		t.Fatalf("seed legacy workspace %s (kind=%s): %v", row.name, row.kind, err)
	}
}

// migratedWorkspaceRow is what 0029 leaves behind for one row, read back with
// plain column types (sources/base_image decoded to generic JSON so a test
// can assert on presence/absence of individual keys, e.g. the omitted-null
// ref/target case).
type migratedWorkspaceRow struct {
	sources     []map[string]any
	baseImage   map[string]any // nil when the column is SQL NULL
	status      string
	activeRunID *uuid.UUID
}

func readMigratedWorkspace(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) migratedWorkspaceRow {
	t.Helper()
	var sourcesRaw, baseImageRaw []byte
	var out migratedWorkspaceRow
	if err := pool.QueryRow(context.Background(),
		`SELECT sources, base_image, status, active_run_id FROM workspaces WHERE id=$1`, id,
	).Scan(&sourcesRaw, &baseImageRaw, &out.status, &out.activeRunID); err != nil {
		t.Fatalf("read migrated workspace %s: %v", id, err)
	}
	if err := json.Unmarshal(sourcesRaw, &out.sources); err != nil {
		t.Fatalf("unmarshal sources for %s: %v (raw: %s)", id, err, sourcesRaw)
	}
	if baseImageRaw != nil {
		if err := json.Unmarshal(baseImageRaw, &out.baseImage); err != nil {
			t.Fatalf("unmarshal base_image for %s: %v (raw: %s)", id, err, baseImageRaw)
		}
	}
	return out
}

// TestMigration0029_TransformsLegacyWorkspaceRows seeds one pre-0029 row per
// legacy kind (plus an in-flight `scanning` repo row) against a single shared
// database frozen at 0028, applies 0029 exactly once, then asserts each row's
// resulting shape as an independent t.Run -- the "three migration tests"
// (local_dir/repo/container -> sources+base_image+status) plus the
// scanning-keeps-active_run_id case, without paying for a separate throwaway
// database per case.
func TestMigration0029_TransformsLegacyWorkspaceRows(t *testing.T) {
	pool := preCompositionDatabase(t)

	localDirID := uuid.New()
	seedLegacyWorkspace(t, pool, legacyWorkspaceRow{
		id: localDirID, name: "mig-local-dir-" + localDirID.String(), kind: "local_dir",
		source: "/srv/repo-a", writable: true, profile: `{"lang":"go"}`, status: "ready", // ready+profile -> scanned
	})

	repoFullID := uuid.New()
	seedLegacyWorkspace(t, pool, legacyWorkspaceRow{
		id: repoFullID, name: "mig-repo-full-" + repoFullID.String(), kind: "repo",
		source: "octocat/Hello-World", ref: "main", defaultTarget: "/work/checkout",
		profile: `{"lang":"node"}`, status: "verifying", // verifying+profile -> scanned
	})

	repoBareID := uuid.New()
	seedLegacyWorkspace(t, pool, legacyWorkspaceRow{
		id: repoBareID, name: "mig-repo-bare-" + repoBareID.String(), kind: "repo",
		source: "octocat/Spoon-Knife", // ref/default_target left at their '' default
		status: "build_error",         // no profile -> pending_scan
	})

	containerID := uuid.New()
	seedLegacyWorkspace(t, pool, legacyWorkspaceRow{
		id: containerID, name: "mig-container-" + containerID.String(), kind: "container",
		source: "ghcr.io/acme/custom-image:latest", profile: `{"lang":"node"}`, status: "verify_failed", // verify_failed+profile -> scanned
	})

	scanningRunID := uuid.New()
	scanningID := uuid.New()
	seedLegacyWorkspace(t, pool, legacyWorkspaceRow{
		id: scanningID, name: "mig-scanning-" + scanningID.String(), kind: "repo",
		source: "octocat/in-flight", status: "scanning", activeRunID: &scanningRunID,
	})

	// Apply the migration under test exactly once, after every row above is seeded.
	execMigrationFile(t, pool, workspaceSourcesMigration)

	t.Run("local_dir", func(t *testing.T) {
		got := readMigratedWorkspace(t, pool, localDirID)
		if got.status != "scanned" {
			t.Errorf("status = %q, want scanned (ready + profile present collapses to scanned)", got.status)
		}
		if got.baseImage != nil {
			t.Errorf("base_image = %v, want nil for a local_dir workspace", got.baseImage)
		}
		if len(got.sources) != 1 {
			t.Fatalf("len(sources) = %d, want 1: %+v", len(got.sources), got.sources)
		}
		src := got.sources[0]
		want := map[string]any{"type": "local_dir", "path": "/srv/repo-a", "target": "/home/agent/work", "writable": true}
		for k, v := range want {
			if src[k] != v {
				t.Errorf("sources[0][%q] = %#v, want %#v (full: %+v)", k, src[k], v, src)
			}
		}
	})

	t.Run("repo_with_ref_and_target", func(t *testing.T) {
		got := readMigratedWorkspace(t, pool, repoFullID)
		if got.status != "scanned" {
			t.Errorf("status = %q, want scanned (verifying + profile present collapses to scanned)", got.status)
		}
		if got.baseImage != nil {
			t.Errorf("base_image = %v, want nil for a repo workspace", got.baseImage)
		}
		if len(got.sources) != 1 {
			t.Fatalf("len(sources) = %d, want 1: %+v", len(got.sources), got.sources)
		}
		src := got.sources[0]
		want := map[string]any{"type": "repo", "source": "octocat/Hello-World", "ref": "main", "target": "/work/checkout"}
		for k, v := range want {
			if src[k] != v {
				t.Errorf("sources[0][%q] = %#v, want %#v (full: %+v)", k, src[k], v, src)
			}
		}
	})

	t.Run("repo_without_ref_or_target_omits_null_keys", func(t *testing.T) {
		got := readMigratedWorkspace(t, pool, repoBareID)
		if got.status != "pending_scan" {
			t.Errorf("status = %q, want pending_scan (build_error + no profile collapses to pending_scan)", got.status)
		}
		if len(got.sources) != 1 {
			t.Fatalf("len(sources) = %d, want 1: %+v", len(got.sources), got.sources)
		}
		src := got.sources[0]
		if src["type"] != "repo" || src["source"] != "octocat/Spoon-Knife" {
			t.Errorf("sources[0] = %+v, want type=repo source=octocat/Spoon-Knife", src)
		}
		// An empty ref/default_target must not become a stored empty string OR
		// an explicit JSON null -- the key must be ABSENT so decoding into
		// WorkspaceSource leaves Ref/Target at their Go zero value.
		if _, ok := src["ref"]; ok {
			t.Errorf("sources[0] has a \"ref\" key (%#v) for an unset ref; want the key omitted", src["ref"])
		}
		if _, ok := src["target"]; ok {
			t.Errorf("sources[0] has a \"target\" key (%#v) for an unset default_target; want the key omitted", src["target"])
		}
	})

	t.Run("container_becomes_ephemeral_source_plus_custom_base_image", func(t *testing.T) {
		got := readMigratedWorkspace(t, pool, containerID)
		if got.status != "scanned" {
			t.Errorf("status = %q, want scanned (verify_failed + profile present collapses to scanned)", got.status)
		}
		if len(got.sources) != 1 || got.sources[0]["type"] != "ephemeral" || got.sources[0]["target"] != "/home/agent/work" {
			t.Errorf("sources = %+v, want a single ephemeral source targeting /home/agent/work", got.sources)
		}
		if got.sources[0]["path"] != nil || got.sources[0]["source"] != nil {
			t.Errorf("ephemeral source carries a path/source: %+v, want neither", got.sources[0])
		}
		if got.baseImage == nil {
			t.Fatal("base_image = nil, want {kind: custom, image: <old source>}")
		}
		if got.baseImage["kind"] != "custom" || got.baseImage["image"] != "ghcr.io/acme/custom-image:latest" {
			t.Errorf("base_image = %+v, want kind=custom image=ghcr.io/acme/custom-image:latest", got.baseImage)
		}
	})

	t.Run("scanning_row_keeps_active_run_id", func(t *testing.T) {
		got := readMigratedWorkspace(t, pool, scanningID)
		if got.status != "scanning" {
			t.Errorf("status = %q, want scanning unchanged (not in the collapsed set)", got.status)
		}
		if got.activeRunID == nil || *got.activeRunID != scanningRunID {
			t.Errorf("active_run_id = %v, want %s preserved across the migration", got.activeRunID, scanningRunID)
		}
	})

	t.Run("legacy_columns_and_index_are_gone", func(t *testing.T) {
		ctx := context.Background()
		var cols []string
		rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name='workspaces'`)
		if err != nil {
			t.Fatalf("query information_schema.columns: %v", err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatalf("scan column name: %v", err)
			}
			cols = append(cols, c)
		}
		rows.Close()

		byName := map[string]bool{}
		for _, c := range cols {
			byName[c] = true
		}
		for _, dropped := range []string{
			"kind", "source", "ref", "default_target", "writable",
			"setup_commands", "verify_result", "verified_profile_hash", "verified_at",
		} {
			if byName[dropped] {
				t.Errorf("column %q still present after 0029; want it dropped. Columns: %v", dropped, cols)
			}
		}
		for _, kept := range []string{"sources", "base_image", "requirements", "record_results", "llm_cred"} {
			if !byName[kept] {
				t.Errorf("column %q missing after 0029. Columns: %v", kept, cols)
			}
		}

		var idxCount int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE indexname='workspaces_local_dir_source_idx'`,
		).Scan(&idxCount); err != nil {
			t.Fatalf("query pg_indexes: %v", err)
		}
		if idxCount != 0 {
			t.Errorf("workspaces_local_dir_source_idx still exists after 0029; want it dropped")
		}
	})
}

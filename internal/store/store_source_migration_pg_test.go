// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 0031 turns embedded workspace compositions into the shared
// three-tier shape: a deduped sources library, a base-image catalog, and
// per-workspace attachments. These tests build PRE-0031 rows (post-0029
// embedded JSONB) and assert the extraction — the properties a backfill
// mistake would silently corrupt: attachment ORDER, per-attachment
// writable/target, identity dedupe, recommended-stays-NULL, and the
// single-source-only profile carry.

const sourceLibraryMigration = "0031_source_library.sql"

// databaseBefore returns a throwaway database with every migration STRICTLY
// BEFORE `stop` applied — the generalized form of preCompositionDatabase.
func databaseBefore(t *testing.T, stop string) *pgxpool.Pool {
	t.Helper()
	pool := throwawayDatabase(t)
	for _, name := range migrationFileNames(t) {
		if name >= stop {
			break
		}
		execMigrationFile(t, pool, name)
	}
	return pool
}

// insertPre0031Workspace inserts a post-0029/pre-0031 workspaces row (embedded
// sources/base_image JSONB). Minimal column set — everything else defaults.
func insertPre0031Workspace(t *testing.T, pool *pgxpool.Pool, name string, sources any, baseImage any, profile any, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	srcJSON, _ := json.Marshal(sources)
	var biJSON, profJSON []byte
	if baseImage != nil {
		biJSON, _ = json.Marshal(baseImage)
	}
	if profile != nil {
		profJSON, _ = json.Marshal(profile)
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO workspaces (id, name, sources, base_image, profile, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6, now(), now())`,
		id, name, srcJSON, biJSON, profJSON, status)
	if err != nil {
		t.Fatalf("insert pre-0031 workspace %s: %v", name, err)
	}
	return id
}

func TestMigration0031_SourceLibraryExtraction(t *testing.T) {
	pool := databaseBefore(t, sourceLibraryMigration)
	ctx := context.Background()

	dir := map[string]any{"type": "local_dir", "path": "/home/me/payments/", "target": "/work/pay", "writable": true}
	repo := map[string]any{"type": "repo", "source": "ACME/Widgets", "ref": "main", "target": "/work/w"}
	eph := map[string]any{"type": "ephemeral", "target": "/scratch"}

	// Three workspaces sharing the same repo (case/slash variants included) —
	// must collapse to ONE sources row. One multi-source (order matters), one
	// single-source with a profile (carry), one repo-only sharing identity.
	multi := insertPre0031Workspace(t, pool, "multi", []any{eph, dir, repo}, nil,
		map[string]any{"languages": []string{"go"}}, "scanned")
	single := insertPre0031Workspace(t, pool, "single", []any{map[string]any{
		"type": "local_dir", "path": "/home/me/solo", "writable": false,
	}}, map[string]any{"kind": "custom", "image": "ubuntu:24.04", "steps": []string{"RUN apt-get update"}},
		map[string]any{"languages": []string{"ts"}}, "scanned")
	_ = insertPre0031Workspace(t, pool, "repo-twin", []any{map[string]any{
		"type": "repo", "source": "acme/widgets", "ref": " main ",
	}}, map[string]any{"kind": "custom", "image": "ubuntu:24.04", "steps": []string{"RUN apt-get update"}}, nil, "pending_scan")
	recommended := insertPre0031Workspace(t, pool, "rec", []any{eph},
		map[string]any{"kind": "recommended"}, nil, "scanned")

	execMigrationFile(t, pool, sourceLibraryMigration)

	// Dedupe: /home/me/payments (slash-trimmed) + acme/widgets@main (case/trim-
	// canonicalized) + /home/me/solo = exactly 3 library rows.
	var nSources int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sources`).Scan(&nSources); err != nil {
		t.Fatal(err)
	}
	if nSources != 3 {
		rows, _ := pool.Query(ctx, `SELECT kind, locator, ref FROM sources`)
		defer rows.Close()
		for rows.Next() {
			var k, l, r string
			_ = rows.Scan(&k, &l, &r)
			t.Logf("source: %s %s %q", k, l, r)
		}
		t.Fatalf("sources = %d, want 3 (dedupe on identity)", nSources)
	}
	var repoCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sources WHERE kind='repo' AND locator='acme/widgets' AND ref='main'`).Scan(&repoCount); err != nil {
		t.Fatal(err)
	}
	if repoCount != 1 {
		t.Errorf("canonicalized repo rows = %d, want exactly 1 (lowercase + trimmed ref collapse)", repoCount)
	}

	// Attachments: order preserved (eph, dir, repo), writable/target carried.
	var attRaw []byte
	if err := pool.QueryRow(ctx, `SELECT attachments FROM workspaces WHERE id=$1`, multi).Scan(&attRaw); err != nil {
		t.Fatal(err)
	}
	var atts []map[string]any
	if err := json.Unmarshal(attRaw, &atts); err != nil {
		t.Fatal(err)
	}
	if len(atts) != 3 {
		t.Fatalf("attachments = %d, want 3: %s", len(atts), attRaw)
	}
	if atts[0]["ephemeral"] != true || atts[0]["target"] != "/scratch" {
		t.Errorf("attachments[0] = %v, want the ephemeral row first (order preserved)", atts[0])
	}
	if atts[1]["writable"] != true || atts[1]["target"] != "/work/pay" || atts[1]["source_id"] == nil {
		t.Errorf("attachments[1] = %v, want the dir with writable+target carried", atts[1])
	}
	if atts[2]["source_id"] == nil || atts[2]["target"] != "/work/w" {
		t.Errorf("attachments[2] = %v, want the repo attachment", atts[2])
	}

	// Base images: the two identical custom recipes collapse to ONE catalog
	// row; both workspaces point at it; recommended stays NULL.
	var nImages int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM base_images`).Scan(&nImages); err != nil {
		t.Fatal(err)
	}
	if nImages != 1 {
		t.Errorf("base_images = %d, want 1 (identity dedupe)", nImages)
	}
	var refs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE base_image_id IS NOT NULL`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 2 {
		t.Errorf("workspaces referencing the catalog = %d, want 2", refs)
	}
	var recNull bool
	if err := pool.QueryRow(ctx, `SELECT base_image_id IS NULL FROM workspaces WHERE id=$1`, recommended).Scan(&recNull); err != nil {
		t.Fatal(err)
	}
	if !recNull {
		t.Error("recommended workspace must keep base_image_id NULL — derived, not a catalog row")
	}

	// Profile carry: ONLY the single-non-ephemeral-source workspace's profile
	// moves onto its source; the multi-source workspace's merged profile does
	// NOT get attributed.
	var soloProfiled, payProfiled bool
	if err := pool.QueryRow(ctx,
		`SELECT profile IS NOT NULL FROM sources WHERE locator='/home/me/solo'`).Scan(&soloProfiled); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT profile IS NOT NULL FROM sources WHERE locator='/home/me/payments'`).Scan(&payProfiled); err != nil {
		t.Fatal(err)
	}
	if !soloProfiled {
		t.Error("single-source workspace's profile must carry onto its source")
	}
	if payProfiled {
		t.Error("multi-source workspace's merged profile must NOT be attributed to a source")
	}
	_ = single

	// 0031 is expand-only: the embedded columns survive untouched.
	var legacyIntact int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE sources IS NOT NULL`).Scan(&legacyIntact); err != nil {
		t.Fatal(err)
	}
	if legacyIntact != 4 {
		t.Errorf("embedded sources column rows = %d, want all 4 intact (expand-only)", legacyIntact)
	}
}

// The catalog CHECK is structural: 'recommended' can never become a row.
func TestMigration0031_RecommendedIsUninsertable(t *testing.T) {
	pool := databaseBefore(t, sourceLibraryMigration)
	execMigrationFile(t, pool, sourceLibraryMigration)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO base_images (id, kind, name, image, created_at, updated_at)
		VALUES ($1, 'recommended', 'x', 'y', now(), now())`, uuid.New())
	if err == nil {
		t.Fatal("inserting kind='recommended' must violate the CHECK constraint")
	}
}

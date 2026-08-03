// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration test for migration 0030 (egress redirects): seed a pre-0030,
// legacy-shaped site_config row (artifact_overrides, ecosystem-keyed) against a
// database that has every migration EXCEPT 0030 applied, apply exactly 0030,
// and assert the resulting egress_redirects shape -- the From-URL-per-ecosystem
// mapping, the token_secret_ref fold, sorted-by-ecosystem-key array order (the
// determinism artifactMirrorRows/planArtifactRedirect's shared-host dedup
// depends on), upstream_proxy_secret_ref left untouched, and artifact_overrides
// gone from the stored document afterward.
//
// Reuses migrationFileNames/execMigrationFile/throwawayDatabase from
// store_workspace_migration_pg_test.go (same package, same style: a database
// frozen one migration short of the one under test, so a pre-migration row can
// still be inserted using the shape that migration goes on to transform).
// Guarded by WARDYN_TEST_PG (see store_pg_test.go).
package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const egressRedirectsMigration = "0030_egress_redirects.sql"

// preEgressRedirectsDatabase returns a throwaway database with every migration
// STRICTLY BEFORE 0030 applied -- the schema exactly as it stood before the
// egress-redirects migration, so a test can insert a legacy-shaped
// artifact_overrides site_config row.
func preEgressRedirectsDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := throwawayDatabase(t)
	for _, name := range migrationFileNames(t) {
		if name >= egressRedirectsMigration {
			break
		}
		execMigrationFile(t, pool, name)
	}
	return pool
}

// seedLegacySiteConfig inserts the singleton site_config row directly (bypassing
// the Go store layer entirely) so a body shaped exactly like a pre-0030 write
// can be seeded, including the ecosystem-keyed artifact_overrides key 0030 goes
// on to fold.
func seedLegacySiteConfig(t *testing.T, pool *pgxpool.Pool, configJSON string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO site_config (singleton, config) VALUES (true, $1)`, []byte(configJSON),
	); err != nil {
		t.Fatalf("seed legacy site_config: %v", err)
	}
}

// readSiteConfigJSON reads the singleton row's config column as generic JSON
// (not types.SiteConfig) so a test can assert on individual key
// presence/absence/order -- exactly what a Go struct decode would hide.
func readSiteConfigJSON(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT config FROM site_config WHERE singleton`).Scan(&raw); err != nil {
		t.Fatalf("read site_config: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal site_config: %v (raw: %s)", err, raw)
	}
	return out
}

// TestMigration0030_TransformsLegacyArtifactOverrides is the primary data-shape
// regression: a two-ecosystem legacy document (one with a token, one without,
// sharing no host) becomes a two-element egress_redirects array with the
// correct From-per-ecosystem URL, the token folded onto the right entry (and
// omitted, not null, where there wasn't one), sorted by ecosystem key, while
// upstream_proxy_secret_ref/scm_hosts ride through untouched.
func TestMigration0030_TransformsLegacyArtifactOverrides(t *testing.T) {
	pool := preEgressRedirectsDatabase(t)

	seedLegacySiteConfig(t, pool, `{
		"upstream_proxy_secret_ref": "corp-proxy-url",
		"artifact_overrides": {
			"npm": {"base_url": "https://artifactory.corp/api/npm/npm-remote/", "token_secret_ref": "npm-token"},
			"go":  {"base_url": "https://artifactory.corp/api/go/go-remote"}
		},
		"scm_hosts": ["dev.azure.com"]
	}`)

	execMigrationFile(t, pool, egressRedirectsMigration)

	got := readSiteConfigJSON(t, pool)

	if _, present := got["artifact_overrides"]; present {
		t.Errorf("artifact_overrides still present after migration: %+v", got)
	}
	if got["upstream_proxy_secret_ref"] != "corp-proxy-url" {
		t.Errorf("upstream_proxy_secret_ref = %v, want untouched", got["upstream_proxy_secret_ref"])
	}
	scmHosts, _ := got["scm_hosts"].([]any)
	if len(scmHosts) != 1 || scmHosts[0] != "dev.azure.com" {
		t.Errorf("scm_hosts = %v, want untouched [dev.azure.com]", got["scm_hosts"])
	}

	redirects, ok := got["egress_redirects"].([]any)
	if !ok || len(redirects) != 2 {
		t.Fatalf("egress_redirects = %+v, want 2 entries", got["egress_redirects"])
	}

	// Sorted by ecosystem key ("go" < "npm") -- the determinism the shared-host
	// "first sighted wins" dedup (artifactMirrorRows, planArtifactRedirect)
	// depends on for byte-identical behavior after the fold.
	goRedirect, _ := redirects[0].(map[string]any)
	npmRedirect, _ := redirects[1].(map[string]any)

	if goRedirect["ecosystem"] != "go" || goRedirect["from"] != "https://proxy.golang.org" ||
		goRedirect["to"] != "https://artifactory.corp/api/go/go-remote" {
		t.Errorf("go redirect = %+v", goRedirect)
	}
	if _, present := goRedirect["token_secret_ref"]; present {
		t.Errorf("go redirect has a token_secret_ref key (%+v), want it OMITTED (no token was configured, and a "+
			"stored JSON null would decode into a non-empty-looking field)", goRedirect)
	}

	if npmRedirect["ecosystem"] != "npm" || npmRedirect["from"] != "https://registry.npmjs.org/" ||
		npmRedirect["to"] != "https://artifactory.corp/api/npm/npm-remote/" ||
		npmRedirect["token_secret_ref"] != "npm-token" {
		t.Errorf("npm redirect = %+v", npmRedirect)
	}
}

// TestMigration0030_EveryEcosystemMapsToItsPublicHost pins the full From-URL
// table (all six ecosystems, not just the two the primary test exercises) so a
// future edit to the mapping is caught row-by-row rather than only for
// whichever ecosystem happens to appear in another test's fixture.
func TestMigration0030_EveryEcosystemMapsToItsPublicHost(t *testing.T) {
	pool := preEgressRedirectsDatabase(t)
	seedLegacySiteConfig(t, pool, `{"artifact_overrides": {
		"npm":   {"base_url": "https://corp/npm"},
		"pip":   {"base_url": "https://corp/pip"},
		"cargo": {"base_url": "https://corp/cargo"},
		"maven": {"base_url": "https://corp/maven"},
		"go":    {"base_url": "https://corp/go"},
		"nuget": {"base_url": "https://corp/nuget"}
	}}`)

	execMigrationFile(t, pool, egressRedirectsMigration)

	got := readSiteConfigJSON(t, pool)
	redirects, _ := got["egress_redirects"].([]any)
	wantFrom := map[string]string{
		"npm":   "https://registry.npmjs.org/",
		"pip":   "https://pypi.org/simple/",
		"cargo": "https://index.crates.io/",
		"maven": "https://repo.maven.apache.org/maven2/",
		"go":    "https://proxy.golang.org",
		"nuget": "https://api.nuget.org/v3/index.json",
	}
	if len(redirects) != len(wantFrom) {
		t.Fatalf("egress_redirects has %d entries, want %d: %+v", len(redirects), len(wantFrom), redirects)
	}
	for _, raw := range redirects {
		r, _ := raw.(map[string]any)
		eco, _ := r["ecosystem"].(string)
		want, known := wantFrom[eco]
		if !known {
			t.Errorf("unexpected ecosystem %q in %+v", eco, r)
			continue
		}
		if r["from"] != want {
			t.Errorf("ecosystem %q: from = %v, want %v", eco, r["from"], want)
		}
	}
}

// TestMigration0030_NoArtifactOverridesIsANoOp asserts a site_config row with
// no artifact_overrides key (the common already-current-shape case, or a bare
// upstream-proxy-only config) round-trips through 0030 untouched -- the
// migration's WHERE guard must not manufacture an empty egress_redirects out
// of nothing, and must not disturb any other field.
func TestMigration0030_NoArtifactOverridesIsANoOp(t *testing.T) {
	pool := preEgressRedirectsDatabase(t)
	seedLegacySiteConfig(t, pool, `{"upstream_proxy_secret_ref": "corp-proxy-url"}`)

	execMigrationFile(t, pool, egressRedirectsMigration)

	got := readSiteConfigJSON(t, pool)
	if _, present := got["egress_redirects"]; present {
		t.Errorf("egress_redirects manufactured out of nothing: %+v", got)
	}
	if got["upstream_proxy_secret_ref"] != "corp-proxy-url" {
		t.Errorf("upstream_proxy_secret_ref = %v, want untouched", got["upstream_proxy_secret_ref"])
	}
}

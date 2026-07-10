// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration test for the site-config store singleton. Guarded by
// WARDYN_TEST_PG (see store_pg_test.go).
package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_SiteConfigGetPutSingleton(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	// site_config is a singleton (one row for the whole DB, not per-workspace
	// like the other PG-gated fixtures), so this test must own its cleanup on
	// BOTH ends: clear any row a prior run left behind before asserting
	// "absent", and clear again on exit so the next run starts clean too.
	clear := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM site_config`); err != nil {
			t.Fatalf("clear site_config: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	// Absent config: zero value, not an error.
	got, err := store.NewPG(pool).GetSiteConfig(ctx)
	if err != nil {
		t.Fatalf("get absent config: %v", err)
	}
	if !reflect.DeepEqual(got, types.SiteConfig{}) {
		t.Fatalf("absent config = %+v, want zero value", got)
	}

	cfg := types.SiteConfig{
		UpstreamProxySecretRef: "corp-proxy-url",
		ArtifactOverrides: map[string]types.ArtifactOverride{
			"npm": {BaseURL: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "npm-token"},
			"go":  {BaseURL: "https://artifactory.corp/api/go/go-remote"},
		},
		ScmHosts: []string{"dev.azure.com", "github.example.com"},
	}
	saved, err := store.NewPG(pool).PutSiteConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("put config: %v", err)
	}
	if !reflect.DeepEqual(saved, cfg) {
		t.Fatalf("put returned %+v, want %+v", saved, cfg)
	}

	// Round trip via a fresh Get.
	got, err = store.NewPG(pool).GetSiteConfig(ctx)
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("get after put = %+v, want %+v", got, cfg)
	}

	// A second Put is an upsert (singleton: never a second row), and REPLACES
	// the whole document — clearing a field must actually clear it, not merge.
	cfg2 := types.SiteConfig{ScmHosts: []string{"github.com"}}
	if _, err := store.NewPG(pool).PutSiteConfig(ctx, cfg2); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err = store.NewPG(pool).GetSiteConfig(ctx)
	if err != nil {
		t.Fatalf("get after second put: %v", err)
	}
	if !reflect.DeepEqual(got, cfg2) {
		t.Fatalf("get after second put = %+v, want %+v (full replace, not merge)", got, cfg2)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_config`).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("site_config row count = %d, want 1 (singleton)", rowCount)
	}
}

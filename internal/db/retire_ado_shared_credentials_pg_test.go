// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// retireADOMigration is the retirement migration's own filename, found by name
// so its number can move when a lane ahead of it renumbers: the seed documents
// below are written for the schema every EARLIER migration leaves.
func retireADOMigration(t *testing.T) string {
	t.Helper()
	for _, name := range readMigrationNames(t) {
		if strings.HasSuffix(name, "_retire_ado_shared_credentials.sql") {
			return name
		}
	}
	t.Fatal("no *_retire_ado_shared_credentials.sql migration")
	return ""
}

// retiredDoc is the provider rows of a site config, in stored order and by id.
type retiredDoc struct {
	order []string
	rows  map[string]map[string]any
	scm   []string
}

func readRetired(t *testing.T, pool *pgxpool.Pool) retiredDoc {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT config FROM site_config WHERE singleton`).Scan(&raw); err != nil {
		t.Fatalf("read site_config: %v", err)
	}
	var doc struct {
		WorkspaceProviders struct {
			Git []map[string]any `json:"git"`
		} `json:"workspace_providers"`
		ScmHosts []string `json:"scm_hosts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode site_config %s: %v", raw, err)
	}
	out := retiredDoc{rows: map[string]map[string]any{}, scm: doc.ScmHosts}
	for _, r := range doc.WorkspaceProviders.Git {
		id, _ := r["id"].(string)
		out.order = append(out.order, id)
		out.rows[id] = r
	}
	return out
}

// retireRows seeds a site config holding rows (a JSON array of provider rows),
// runs the retirement migration over it through Migrate(), and returns what it
// left. It also runs the migration's SQL a second time and requires the
// document not to change.
func retireRows(t *testing.T, rows string) retiredDoc {
	t.Helper()
	name := retireADOMigration(t)
	pool, _ := partialSchemaPool(t, name)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO site_config (config) VALUES ($1)`,
		`{"workspace_providers":{"git":`+rows+`},"scm_hosts":["dev.azure.com"]}`); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate over the seed: %v", err)
	}
	got := readRetired(t, pool)
	if _, err := pool.Exec(ctx, readMigration(t, name)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again := readRetired(t, pool); !slices.Equal(again.order, got.order) || !reflect.DeepEqual(again.rows, got.rows) {
		t.Errorf("a second run changed the rows:\n first %v\nsecond %v", got.rows, again.rows)
	}
	if !slices.Equal(got.scm, []string{"dev.azure.com"}) {
		t.Errorf("scm_hosts = %v, want the seed untouched", got.scm)
	}
	return got
}

// strs reads a JSON array of strings out of a decoded row.
func strs(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	return out
}

// wantOwnPAT is what a hosted (Services) row with no per-person lane becomes:
// disabled, the entra lane, per_user, and an own_pat block that names no tenant
// or client.
func wantOwnPAT(t *testing.T, id string, r map[string]any) {
	t.Helper()
	if r["disabled"] != true || r["credential_source"] != "per_user" || !slices.Equal(strs(r["lanes"]), []string{"entra"}) {
		t.Errorf("%s = %v, want disabled, lanes [entra], per_user", id, r)
	}
	e, _ := r["entra"].(map[string]any)
	if e == nil || e["token_mode"] != "own_pat" || !slices.Equal(strs(e["capability_ceiling"]), []string{"project_read", "code_read"}) {
		t.Errorf("%s entra = %v, want own_pat with the ceiling project_read + code_read", id, e)
	}
	if dp, ok := e["default_profile"].([]any); !ok || len(dp) != 0 {
		t.Errorf("%s default_profile = %v, want empty", id, e["default_profile"])
	}
	if e["tenant_id"] != nil || e["client_id"] != nil {
		t.Errorf("%s entra = %v, want no tenant or client: nothing signs in", id, e)
	}
}

// wantServerPAT is what a Server row with no per-person lane becomes:
// disabled, the pat lane, per_user, and NO entra block (Azure DevOps Server has
// no Entra sign-in, and an entra block is refused there).
func wantServerPAT(t *testing.T, id string, r map[string]any) {
	t.Helper()
	if r["disabled"] != true || r["credential_source"] != "per_user" || !slices.Equal(strs(r["lanes"]), []string{"pat"}) {
		t.Errorf("%s = %v, want disabled, lanes [pat], per_user", id, r)
	}
	if _, has := r["entra"]; has {
		t.Errorf("%s kept an entra block: %v", id, r["entra"])
	}
}

// TestPG_RetireADOShared_HostedRows: every hosted Azure DevOps row that names a
// shared lane — pat only, ssh only, both, or nothing (which read as the legacy
// lanes) — becomes a disabled, per-person own_pat row. It keeps its id, its
// addresses and its place in the list; nothing is deleted.
func TestPG_RetireADOShared_HostedRows(t *testing.T) {
	got := retireRows(t, `[
		{"id":"pat-only","kind":"azure_devops","base_urls":["https://dev.azure.com/acme"],"lanes":["pat"]},
		{"id":"ssh-only","kind":"azure_devops","base_urls":["https://dev.azure.com/beta"],"lanes":["ssh"]},
		{"id":"both","kind":"azure_devops","base_urls":["https://dev.azure.com/gamma"],"lanes":["pat","ssh"],"credential_source":"shared"},
		{"id":"empty","kind":"azure_devops","base_urls":["https://dev.azure.com/delta"]},
		{"id":"empty-list","kind":"azure_devops","base_urls":["https://dev.azure.com/eps"],"lanes":[]},
		{"id":"vs","kind":"azure_devops","base_urls":["https://acme.visualstudio.com"],"lanes":["pat","ssh"]},
		{"id":"off","kind":"azure_devops","disabled":true,"base_urls":["https://dev.azure.com/zeta"],"lanes":["pat"]},
		{"id":"both-hosted","kind":"azure_devops","base_urls":["https://dev.azure.com/eta","https://theta.visualstudio.com"],"lanes":["ssh"]}
	]`)
	wantOrder := []string{"pat-only", "ssh-only", "both", "empty", "empty-list", "vs", "off", "both-hosted"}
	if !slices.Equal(got.order, wantOrder) {
		t.Fatalf("rows = %v, want %v: the migration deletes and reorders nothing", got.order, wantOrder)
	}
	for _, id := range wantOrder {
		wantOwnPAT(t, id, got.rows[id])
	}
	if urls := strs(got.rows["both-hosted"]["base_urls"]); !slices.Equal(urls, []string{"https://dev.azure.com/eta", "https://theta.visualstudio.com"}) {
		t.Errorf("base_urls = %v, want them kept", urls)
	}
}

// TestPG_RetireADOShared_HostedRowKeepsItsEntraLane: a hosted row that already
// carries the entra lane is a per-person row. Only pat and ssh leave its lanes;
// it stays enabled, and its entra block is exactly as the admin wrote it.
func TestPG_RetireADOShared_HostedRowKeepsItsEntraLane(t *testing.T) {
	const block = `{"tenant_id":"0f2c1f1e-9d3a-4b8c-8f2d-1a2b3c4d5e6f","client_id":"7a6b5c4d-3e2f-4a1b-9c8d-7e6f5a4b3c2d","capability_ceiling":["code_read","pr"],"token_mode":"bearer"}`
	got := retireRows(t, `[
		{"id":"both-plus-entra","kind":"azure_devops","base_urls":["https://dev.azure.com/acme"],"lanes":["pat","ssh","entra"],"credential_source":"per_user","entra":`+block+`},
		{"id":"entra-only","kind":"azure_devops","base_urls":["https://dev.azure.com/beta"],"lanes":["entra"],"credential_source":"per_user","entra":`+block+`},
		{"id":"entra-off","kind":"azure_devops","disabled":true,"base_urls":["https://dev.azure.com/gamma"],"lanes":["entra","pat"],"credential_source":"per_user","entra":`+block+`}
	]`)
	var want map[string]any
	if err := json.Unmarshal([]byte(block), &want); err != nil {
		t.Fatal(err)
	}
	for id, wantOff := range map[string]bool{"both-plus-entra": false, "entra-only": false, "entra-off": true} {
		r := got.rows[id]
		if !slices.Equal(strs(r["lanes"]), []string{"entra"}) || r["credential_source"] != "per_user" {
			t.Errorf("%s = %v, want lanes [entra], per_user", id, r)
		}
		if off, _ := r["disabled"].(bool); off != wantOff {
			t.Errorf("%s disabled = %v, want %v (a per-person row's state is the admin's)", id, off, wantOff)
		}
		if !reflect.DeepEqual(r["entra"], want) {
			t.Errorf("%s entra block = %v, want it untouched %v", id, r["entra"], want)
		}
	}
}

// TestPG_RetireADOShared_ServerRows: a row on any other host (Azure DevOps
// Server) with no per-person lane becomes a disabled per_user pat row with NO
// entra block — Server has no Entra sign-in, and an entra block on a Server
// host is refused at the write door, so the row must round-trip through PUT.
// A row mixing a hosted and a Server address is a Server row.
func TestPG_RetireADOShared_ServerRows(t *testing.T) {
	got := retireRows(t, `[
		{"id":"srv-pat","kind":"azure_devops","base_urls":["https://tfs.corp.example/acme"],"lanes":["pat"]},
		{"id":"srv-ssh","kind":"azure_devops","base_urls":["https://tfs.corp.example/beta"],"lanes":["ssh"]},
		{"id":"srv-both","kind":"azure_devops","base_urls":["https://tfs.corp.example"],"lanes":["pat","ssh"]},
		{"id":"srv-empty","kind":"azure_devops","base_urls":["https://ado.corp.example/gamma"]},
		{"id":"srv-off","kind":"azure_devops","disabled":true,"base_urls":["https://tfs.corp.example/delta"],"lanes":["pat"]},
		{"id":"srv-mixed","kind":"azure_devops","base_urls":["https://dev.azure.com/eps","https://tfs.corp.example/eps"],"lanes":["pat"]},
		{"id":"srv-stale-block","kind":"azure_devops","base_urls":["https://tfs.corp.example/zeta"],"lanes":["pat"],"entra":{"token_mode":"own_pat"}}
	]`)
	for _, id := range []string{"srv-pat", "srv-ssh", "srv-both", "srv-empty", "srv-off", "srv-mixed", "srv-stale-block"} {
		wantServerPAT(t, id, got.rows[id])
	}
}

// TestPG_RetireADOShared_PerPersonAndOtherRowsAreLeftAlone: GitHub rows keep
// their lanes whatever they are (the retirement is Azure DevOps only), and a
// row already in the per-person Server shape is not touched, so the migration
// is safe to re-run.
func TestPG_RetireADOShared_PerPersonAndOtherRowsAreLeftAlone(t *testing.T) {
	seed := `[
		{"id":"gh","kind":"github","base_urls":["https://github.com/acme"],"lanes":["app","pat","ssh"]},
		{"id":"gh-empty","kind":"github","base_urls":["https://git.corp.example/acme"]},
		{"id":"srv-mine","kind":"azure_devops","base_urls":["https://tfs.corp.example/acme"],"lanes":["pat"],"credential_source":"per_user"},
		{"id":"srv-mine-on","kind":"azure_devops","disabled":false,"base_urls":["https://tfs.corp.example/beta"],"lanes":["pat"],"credential_source":"per_user"}
	]`
	got := retireRows(t, seed)
	var want []map[string]any
	if err := json.Unmarshal([]byte(seed), &want); err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		id := w["id"].(string)
		if !reflect.DeepEqual(got.rows[id], w) {
			t.Errorf("%s = %v, want it untouched %v", id, got.rows[id], w)
		}
	}
}

// TestPG_RetireADOShared_NoProviderBlock: an install with no provider rows, or
// no site config at all, is untouched and the migration does not fail.
func TestPG_RetireADOShared_NoProviderBlock(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"no site config row", ""},
		{"no workspace_providers", `{"scm_hosts":["dev.azure.com"]}`},
		{"an empty git list", `{"workspace_providers":{"git":[]}}`},
		{"a git value that is not a list", `{"workspace_providers":{"git":"x"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := retireADOMigration(t)
			pool, _ := partialSchemaPool(t, name)
			ctx := context.Background()
			if tc.config != "" {
				if _, err := pool.Exec(ctx, `INSERT INTO site_config (config) VALUES ($1)`, tc.config); err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(ctx, pool); err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if tc.config == "" {
				return
			}
			var got, want map[string]any
			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(raw, &got)
			_ = json.Unmarshal([]byte(tc.config), &want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("site config = %s, want it untouched %s", raw, tc.config)
			}
		})
	}
}

// TestPG_RetireADOShared_WritesTheSweepMarker (#1429 review F2): the migration
// leaves the boot sweep's marker pending — the sweep runs while done_at is null
// — and a second run of the migration neither duplicates it nor clears one the
// sweep has set.
func TestPG_RetireADOShared_WritesTheSweepMarker(t *testing.T) {
	name := retireADOMigration(t)
	pool, _ := partialSchemaPool(t, name)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pending := func() (n int, doneAtNull bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(bool_and(done_at IS NULL), false) FROM boot_once
			WHERE name = 'ado_shared_credential_retire'`).Scan(&n, &doneAtNull); err != nil {
			t.Fatalf("read the marker: %v", err)
		}
		return n, doneAtNull
	}
	if n, null := pending(); n != 1 || !null {
		t.Fatalf("after the migration: %d marker rows, done_at null = %v; want one, pending", n, null)
	}
	if _, err := pool.Exec(ctx, `UPDATE boot_once SET done_at = now() WHERE name = 'ado_shared_credential_retire'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readMigration(t, name)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n, null := pending(); n != 1 || null {
		t.Fatalf("after a second run: %d marker rows, done_at null = %v; want one, still done", n, null)
	}
}

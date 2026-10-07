// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The boot-time site-config seed against a real Postgres. Guarded by
// WARDYN_TEST_PG (via throwawayPGPool): skipped cleanly when unset, must PASS
// when set.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedNetworkKeys are the keys a seed may write; every other key of the stored
// document must come through a seed byte-for-byte.
var seedNetworkKeys = []string{"upstream_proxy_url", "upstream_proxy_secret_ref", "upstream_proxy_no_proxy", "internal_hosts"}

func testSeed() *SiteConfigSeed {
	return &SiteConfigSeed{
		UpstreamProxySecretRef: "corp-proxy",
		UpstreamProxyNoProxy:   []string{".corp.internal", "100.64.0.0/10"},
		InternalHosts:          []types.InternalHost{{HostSuffix: "git.corp.internal", CIDRs: []string{"10.0.0.0/8"}}},
	}
}

// storedSiteConfig is the raw stored document with drop's keys removed, as
// Postgres prints it (jsonb output is canonical, so equal documents print the
// same bytes). ok is false when there is no row.
func storedSiteConfig(t *testing.T, pool *pgxpool.Pool, drop ...string) (raw []byte, ok bool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM site_config`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return nil, false
	}
	if err := pool.QueryRow(context.Background(), `SELECT (config - $1::text[])::text FROM site_config WHERE singleton`, drop).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw, true
}

func putRawSiteConfig(t *testing.T, pool *pgxpool.Pool, doc string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO site_config (singleton, config) VALUES (true, $1::jsonb)`, doc); err != nil {
		t.Fatal(err)
	}
}

func wantNetworkSettings(t *testing.T, srv *Server, seed *SiteConfigSeed) {
	t.Helper()
	got, err := srv.cfg.Store.GetSiteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Nothing left to fill and nothing that differs: the stored settings are the seed's.
	if missing, differs := seed.apply(&got); len(missing)+len(differs) > 0 {
		t.Errorf("stored network settings: missing %v, differing %v; want the seed's %+v", missing, differs, *seed)
	}
}

func seedAuditSettings(t *testing.T, a *memAudit) [][]string {
	t.Helper()
	var out [][]string
	for _, ev := range a.find("site_config.seed") {
		var d struct{ Settings []string }
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if ev.ActorType != types.ActorSystem || ev.Target != "site_config" || ev.Outcome != "success" {
			t.Errorf("site_config.seed row = %+v", ev)
		}
		out = append(out, d.Settings)
	}
	return out
}

// No row at all (a wiped or rebuilt database): every seeded setting is written
// and one audit row names them.
func TestPG_SiteConfigSeed_NoRow(t *testing.T) {
	p := newReplicaPair(t)
	if _, ok := storedSiteConfig(t, p.poolA); ok {
		t.Fatal("a fresh database already has a site_config row")
	}
	seed := testSeed()
	if err := p.a.SeedSiteConfig(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	wantNetworkSettings(t, p.a, seed)
	rows := seedAuditSettings(t, p.audit)
	if len(rows) != 1 || strings.Join(rows[0], ",") != "upstream_proxy,upstream_proxy_no_proxy,internal_hosts" {
		t.Errorf("site_config.seed rows = %v, want one naming all three settings", rows)
	}
}

// A row holding integrations, a model provider, the onboarding mark and a key
// only a newer wardynd knows, but no network settings: the settings are added
// and every other key is byte-for-byte what it was.
func TestPG_SiteConfigSeed_KeepsOtherKeys(t *testing.T) {
	p := newReplicaPair(t)
	putRawSiteConfig(t, p.poolA, `{
		"integrations": [{"id": "jira", "name": "Jira", "kind": "jira", "egress": ["jira.corp.example"],
			"created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-03T03:04:05Z"}],
		"model_providers": {"providers": [{"id": "corp-gateway", "uid": "mp-0001", "name": "Corp gateway", "kind": "anthropic"}]},
		"scm_hosts": ["git.corp.example"],
		"sign_in_help_text": "Ask the platform team.",
		"onboarding_completed_at": "2026-01-02T03:04:05Z",
		"future_key": {"kept": true}}`)
	before, _ := storedSiteConfig(t, p.poolA, seedNetworkKeys...)
	seed := testSeed()
	if err := p.a.SeedSiteConfig(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	after, _ := storedSiteConfig(t, p.poolA, seedNetworkKeys...)
	if !bytes.Equal(before, after) {
		t.Errorf("the seed changed keys it does not own:\n before %s\n after  %s", before, after)
	}
	wantNetworkSettings(t, p.a, seed)
	if rows := seedAuditSettings(t, p.audit); len(rows) != 1 {
		t.Errorf("site_config.seed rows = %v, want one", rows)
	}
}

// A row whose network settings differ from the seed: nothing is written, one
// warning is logged, and /setup/status shows the database value is in effect.
func TestPG_SiteConfigSeed_DifferentValuesUntouched(t *testing.T) {
	p := newReplicaPair(t)
	putRawSiteConfig(t, p.poolA, `{
		"upstream_proxy_url": "http://proxy-a.corp.example:3128",
		"upstream_proxy_no_proxy": [".a.internal"],
		"internal_hosts": [{"host_suffix": "a.corp.internal"}]}`)
	before, _ := storedSiteConfig(t, p.poolA)
	logs := captureSlog(t)
	seed := testSeed()
	if err := p.a.SeedSiteConfig(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	if after, _ := storedSiteConfig(t, p.poolA); !bytes.Equal(before, after) {
		t.Errorf("the seed overwrote a setting the database has:\n before %s\n after  %s", before, after)
	}
	if rows := seedAuditSettings(t, p.audit); len(rows) != 0 {
		t.Errorf("site_config.seed rows = %v, want none", rows)
	}
	if n := strings.Count(logs.String(), "the site-config seed file and the database disagree"); n != 1 {
		t.Errorf("logged %d disagreement warnings, want 1; log:\n%s", n, logs.String())
	}
	stored, err := p.a.cfg.Store.GetSiteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := statusCheckSeeded(stored, p.a.siteConfigSeed); !ok {
		t.Error("no site_config_seed row on /setup/status while the database differs from the seed")
	}
}

// Two replicas booting together over one empty database: one write.
func TestPG_SiteConfigSeed_ConcurrentReplicasWriteOnce(t *testing.T) {
	p := newReplicaPair(t)
	seed := testSeed()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, srv := range []*Server{p.a, p.b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = srv.SeedSiteConfig(context.Background(), seed)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	wantNetworkSettings(t, p.b, seed)
	if rows := seedAuditSettings(t, p.audit); len(rows) != 1 {
		t.Errorf("site_config.seed rows = %v, want exactly one write across both replicas", rows)
	}
}

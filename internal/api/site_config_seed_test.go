// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func writeSeedFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A seed that is not exactly the four network keys, or is not one well-formed
// JSON object PUT /site-config would accept, refuses boot.
func TestLoadSiteConfigSeed_Refusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown key is named", `{"internal_hosts":[],"egress_redirects":[]}`, `unknown field "egress_redirects"`},
		{"unknown nested key is named", `{"internal_hosts":[{"host_suffix":"git.corp.internal","cidr":"10.0.0.0/8"}]}`, `unknown field "cidr"`},
		{"malformed JSON", `{"upstream_proxy_url": "http://proxy.corp.example:3128"`, "malformed seed"},
		{"not an object", `["upstream_proxy_url"]`, "malformed seed"},
		{"two values", `{} {}`, "more than one JSON value"},
		{"credentialed proxy URL", `{"upstream_proxy_url":"http://user:pass@proxy.corp.example:3128"}`, "must not embed a credential"},
		{"https proxy URL", `{"upstream_proxy_url":"https://proxy.corp.example:3128"}`, "must be http://"},
		{"wildcard bypass", `{"upstream_proxy_no_proxy":["*"]}`, "upstream_proxy_no_proxy[0]"},
		{"public CIDR", `{"internal_hosts":[{"host_suffix":"git.corp.internal","cidrs":["8.8.8.0/24"]}]}`, "internal_hosts[0].cidrs[0]"},
		{"reserved secret name", `{"upstream_proxy_secret_ref":"bad name"}`, "upstream_proxy_secret_ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed, err := LoadSiteConfigSeed(writeSeedFile(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadSiteConfigSeed = %+v, %v; want an error containing %q", seed, err, tc.want)
			}
			if !strings.Contains(err.Error(), "WARDYN_SITE_CONFIG_SEED_FILE") {
				t.Errorf("error %q does not name WARDYN_SITE_CONFIG_SEED_FILE", err)
			}
		})
	}
	if _, err := LoadSiteConfigSeed(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing seed file loaded, want a boot refusal")
	}
}

func TestLoadSiteConfigSeed_Loads(t *testing.T) {
	if seed, err := LoadSiteConfigSeed(""); seed != nil || err != nil {
		t.Fatalf("empty path = %+v, %v; want no seed", seed, err)
	}
	seed, err := LoadSiteConfigSeed(writeSeedFile(t, `{
		"upstream_proxy_url": " HTTP://Proxy.Corp.Example:3128 ",
		"upstream_proxy_no_proxy": [".corp.internal"],
		"internal_hosts": [{"host_suffix": "git.corp.internal", "cidrs": ["10.0.0.0/8"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Normalized as PUT /site-config normalizes it, so a stored value written by
	// either door compares equal.
	if seed.UpstreamProxyURL != "http://proxy.corp.example:3128" {
		t.Errorf("UpstreamProxyURL = %q, want the normalized form", seed.UpstreamProxyURL)
	}
	if len(seed.UpstreamProxyNoProxy) != 1 || len(seed.InternalHosts) != 1 {
		t.Errorf("seed = %+v", seed)
	}
}

// The row appears only while the database holds a seeded setting with another
// value; it never blocks.
func TestSiteConfigSeedCheck(t *testing.T) {
	seed := &SiteConfigSeed{
		UpstreamProxySecretRef: "corp-proxy",
		UpstreamProxyNoProxy:   []string{".corp.internal"},
		InternalHosts:          []types.InternalHost{{HostSuffix: "git.corp.internal", CIDRs: []string{"10.0.0.0/8"}}},
	}
	same := types.SiteConfig{
		UpstreamProxySecretRef: "corp-proxy",
		UpstreamProxyNoProxy:   []string{".corp.internal"},
		InternalHosts:          []types.InternalHost{{HostSuffix: "git.corp.internal", CIDRs: []string{"10.0.0.0/8"}}},
	}
	for _, tc := range []struct {
		name string
		seed *SiteConfigSeed
		sc   types.SiteConfig
	}{
		{"no seed", nil, types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.example:3128"}},
		{"empty database: the seed fills it, nothing differs", seed, types.SiteConfig{}},
		{"database matches the seed", seed, same},
	} {
		if _, ok := statusCheckSeeded(tc.sc, tc.seed); ok {
			t.Errorf("%s: site_config_seed row present, want absent", tc.name)
		}
	}
	differs := same
	differs.UpstreamProxySecretRef = ""
	differs.UpstreamProxyURL = "http://proxy.corp.example:3128"
	differs.InternalHosts = []types.InternalHost{{HostSuffix: "git.corp.internal"}}
	chk, ok := statusCheckSeeded(differs, seed)
	if !ok {
		t.Fatal("site_config_seed row absent while the database differs from the seed")
	}
	if chk.Status != "info" || !strings.Contains(chk.Detail, "upstream_proxy, internal_hosts") || strings.Contains(chk.Detail, "no_proxy") {
		t.Errorf("row = %+v; want an info row naming upstream_proxy and internal_hosts only", chk)
	}
	assertSetupCheckBlocking(t, chk)
}

func statusCheckSeeded(sc types.SiteConfig, seed *SiteConfigSeed) (SetupCheck, bool) {
	for _, c := range siteConfigStatusChecks(nil, sc, nil, seed) {
		if c.ID == "site_config_seed" {
			return c, true
		}
	}
	return SetupCheck{}, false
}

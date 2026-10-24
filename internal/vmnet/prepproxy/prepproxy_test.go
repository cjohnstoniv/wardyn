// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package prepproxy

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

var testPeers = []netip.Prefix{netip.MustParsePrefix("172.20.0.0/24")}

const shippedPath = "../../../deploy/macos-guest/prep-allowlist.json"

// pinnedHosts is this test's own copy of the shipped list. Changing
// prep-allowlist.json fails TestShippedAllowlistIsPinned until this copy is
// changed in the same commit, so the list never grows without a reviewed test
// change. Do not derive it from the file.
var pinnedHosts = []string{
	"swscan.apple.com",
	"swcdn.apple.com",
	"swdist.apple.com",
	"swdownload.apple.com",
	"updates.cdn-apple.com",
	"mesu.apple.com",
	"gdmf.apple.com",
}

func shipped(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(shippedPath)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestShippedAllowlistIsPinned(t *testing.T) {
	hosts, err := ParseAllowlist(shipped(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hosts, pinnedHosts) {
		t.Fatalf("prep-allowlist.json hosts changed without this test's copy:\n shipped %v\n pinned  %v", hosts, pinnedHosts)
	}
}

func TestConfigIsAllowlistOnly(t *testing.T) {
	peers := []netip.Prefix{netip.MustParsePrefix("172.20.0.0/24")}
	raw, err := Config(shipped(t), 3128, peers)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, h := range pinnedHosts {
		want = append(want, h+":443")
	}
	p := cfg.Policy
	if !reflect.DeepEqual(p.AllowedDomains, want) || len(p.DeniedDomains) != 0 || p.AllowAllEgress ||
		p.FirstUseApproval != "always_deny" || len(p.EligibleGrants) != 0 {
		t.Fatalf("policy is not the prep allowlist alone: %+v", p)
	}
	if cfg.Listen != ":3128" || !cfg.VM || !reflect.DeepEqual(cfg.AllowedPeers, peers) {
		t.Fatalf("listen/vm/peers: %q %v %v", cfg.Listen, cfg.VM, cfg.AllowedPeers)
	}
	// No credential and no MITM: every key that could carry one is absent from the wire.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"injection", "mitm_ca_cert_pem", "mitm_ca_key_pem", "mitm_hosts", "mitm_llm", "git_grants",
		"pat_grants", "resident_mints", "ado_grant", "azure_gates", "github_api", "azure_arm", "aws_sigv4", "kube_api",
		"upstream_proxy_url", "trusted_ca_pem", "internal_hosts", "llm_upstreams", "local_models", "mcp_servers",
		"harness_credentials", "decision_sink", "decision_spool_dir", "signin_listen", "control_plane_ca_pem"} {
		if _, ok := keys[k]; ok {
			t.Errorf("config carries %q", k)
		}
	}
	if strings.Contains(string(raw), "BEGIN") {
		t.Error("config carries PEM material")
	}
}

func TestConfigRefusesEverythingElse(t *testing.T) {
	raw, err := Config(shipped(t), 3128, testPeers)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	ev := proxy.NewBuiltinEvaluator(cfg.Policy)
	for _, tc := range []struct {
		host string
		port int
		want egress.HostVerdict
	}{
		{"swscan.apple.com", 443, egress.VerdictAllow},
		{"gdmf.apple.com", 443, egress.VerdictAllow},
		{"swscan.apple.com", 80, egress.VerdictUnknown},
		{"swscan.apple.com", 8443, egress.VerdictUnknown},
		{"oscdn.apple.com", 443, egress.VerdictUnknown},
		{"apple.com", 443, egress.VerdictUnknown},
		{"evil.swscan.apple.com", 443, egress.VerdictUnknown},
		{"github.com", 443, egress.VerdictUnknown},
	} {
		got, err := ev.EvaluateHost(context.Background(), egress.Request{Host: tc.host, Port: tc.port, Method: "CONNECT"})
		if err != nil || got != tc.want {
			t.Errorf("%s:%d = %v, %v; want %v", tc.host, tc.port, got, err, tc.want)
		}
	}
}

func TestParseAllowlistRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"unknown key":  `{"version":1,"hosts":["a.apple.com"],"port":80}`,
		"bad version":  `{"version":2,"hosts":["a.apple.com"]}`,
		"empty":        `{"version":1,"hosts":[]}`,
		"wildcard":     `{"version":1,"hosts":["*.apple.com"]}`,
		"port":         `{"version":1,"hosts":["a.apple.com:80"]}`,
		"scheme":       `{"version":1,"hosts":["https://a.apple.com"]}`,
		"upper case":   `{"version":1,"hosts":["A.apple.com"]}`,
		"one label":    `{"version":1,"hosts":["apple"]}`,
		"duplicate":    `{"version":1,"hosts":["a.apple.com","a.apple.com"]}`,
		"not an array": `{"version":1,"hosts":"a.apple.com"}`,
	} {
		if _, err := ParseAllowlist([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := Config([]byte(in), 3128, testPeers); err == nil {
			t.Errorf("%s: Config accepted", name)
		}
	}
}

func TestConfigRequiresPeers(t *testing.T) {
	for _, peers := range [][]netip.Prefix{nil, {}} {
		if _, err := Config(shipped(t), 3128, peers); err == nil {
			t.Errorf("Config accepted peers %v", peers)
		}
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestTerminatesTLSMatchesHandleConnect pins Config.TerminatesTLS to the decision handleConnect
// makes at its two TLS-terminating branches, over a real Proxy built from the same settings.
func TestTerminatesTLSMatchesHandleConnect(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	ghGrant := &GitHubAPIGrant{Grants: map[string]uuid.UUID{"o": uuid.New()}}
	patGrants := map[string]PATGrant{"git.corp.example": {GrantID: uuid.New(), API: true}}
	mitm := []string{"artifacts.corp.example:8443", "plain.corp.example", "api.openai.com:8443"}

	cases := []struct {
		name  string
		ca    bool
		llm   bool
		hosts []string
		host  string
		port  int
		want  bool
	}{
		{"llm host, MITMLLM on", true, true, nil, "api.anthropic.com", 443, true},
		{"llm host, MITMLLM off", true, false, nil, "api.anthropic.com", 443, false},
		{"openai host, MITMLLM on", true, true, nil, "api.openai.com", 443, true},
		{"llm host, other port, no entry", true, true, nil, "api.anthropic.com", 8080, true},
		{"llm host entry scoped to another port", true, true, mitm, "api.openai.com", 443, false},
		{"llm host entry scoped, its port", true, true, mitm, "api.openai.com", 8443, true},
		{"llm host, no CA", false, true, nil, "api.anthropic.com", 443, false},
		{"corp host, its port", true, false, mitm, "artifacts.corp.example", 8443, true},
		{"corp host, other port", true, false, mitm, "artifacts.corp.example", 443, false},
		{"corp host, no port in the entry", true, false, mitm, "plain.corp.example", 9999, true},
		{"corp host, no CA", false, false, mitm, "artifacts.corp.example", 8443, false},
		{"host in neither", true, true, mitm, "pypi.org", 443, false},
		{"uppercase spelling", true, false, mitm, "ARTIFACTS.Corp.Example", 8443, true},
		{"trailing dot", true, false, mitm, "artifacts.corp.example.", 8443, true},
		{"uppercase llm host", true, true, nil, "API.Anthropic.COM.", 443, true},
		{"github api door host", true, false, nil, "api.github.com", 443, true},
		{"github api door host, other port", true, false, nil, "api.github.com", 8443, false},
		{"pat api door host", true, false, nil, "git.corp.example", 443, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{MITMHosts: tc.hosts, MITMLLM: tc.llm, GitHubAPI: ghGrant, PATGrants: patGrants}
			opts := Options{
				RunID: uuid.New(), Sink: &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
				MITMHosts: tc.hosts, MITMLLM: tc.llm, GitHubAPI: ghGrant, PATGrants: patGrants,
			}
			if tc.ca {
				cfg.MITMCACertPEM, cfg.MITMCAKeyPEM = string(certPEM), string(keyPEM)
				opts.CA = ca
			}
			p := newProxy(opts)
			real := p.ca != nil && p.isCorpMITMHost(tc.host) && p.mitmPortAllowed(tc.host, tc.port) ||
				p.isLLMHost(tc.host) && p.mitmLLMHost(tc.host, tc.port)
			if real != tc.want {
				t.Fatalf("the proxy's own decision = %v, case expects %v", real, tc.want)
			}
			if got := cfg.TerminatesTLS(tc.host, tc.port); got != real {
				t.Fatalf("TerminatesTLS(%q, %d) = %v, the proxy decides %v", tc.host, tc.port, got, real)
			}
		})
	}
}

func TestInspectsEntry(t *testing.T) {
	cfg := func(llm, ca bool, hosts ...string) *Config {
		c := &Config{MITMHosts: hosts, MITMLLM: llm}
		if ca {
			c.MITMCACertPEM, c.MITMCAKeyPEM = "cert", "key"
		}
		return c
	}
	for _, tc := range []struct {
		name  string
		c     *Config
		entry string
		want  bool
	}{
		{"bare llm host, MITMLLM on", cfg(true, true), "api.anthropic.com", true},
		{"bare llm host, MITMLLM off", cfg(false, true), "api.anthropic.com", false},
		{"bare corp host on a scoped port, read as 443", cfg(false, true, "artifacts.corp.example:8443"), "artifacts.corp.example", false},
		{"corp host entry with its port", cfg(false, true, "artifacts.corp.example:8443"), "artifacts.corp.example:8443", true},
		{"wildcard over an inspected llm host", cfg(true, true), "*.anthropic.com", true},
		{"wildcard over an inspected corp host", cfg(false, true, "artifacts.corp.example"), "*.corp.example", true},
		{"wildcard over a scoped corp host, bare", cfg(false, true, "artifacts.corp.example:8443"), "*.corp.example", true},
		{"wildcard with another port than the corp host", cfg(false, true, "artifacts.corp.example:8443"), "*.corp.example:443", false},
		{"wildcard with the corp host's port", cfg(false, true, "artifacts.corp.example:8443"), "*.corp.example:8443", true},
		{"wildcard covering nothing", cfg(true, true, "artifacts.corp.example"), "*.pypi.org", false},
		{"wildcard over the llm host with MITMLLM off", cfg(false, true), "*.anthropic.com", false},
		{"wildcard over a corp host, no CA", cfg(true, false, "artifacts.corp.example"), "*.corp.example", false},
		{"wildcard does not match its own apex", cfg(false, true, "corp.example"), "*.corp.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.InspectsEntry(tc.entry); got != tc.want {
				t.Fatalf("InspectsEntry(%q) = %v, want %v", tc.entry, got, tc.want)
			}
		})
	}
}

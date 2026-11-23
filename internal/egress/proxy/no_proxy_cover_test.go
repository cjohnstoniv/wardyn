// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noProxyCoverProxy builds a Proxy whose only setting that matters here is its bypass list.
func noProxyCoverProxy(entries []string) *Proxy {
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		UpstreamNoProxy: entries,
	})
}

// TestNoProxyCoversMatchesBypassUpstream: over host and CIDR entries, a trailing dot, upper case and IP
// literals, NoProxyCovers answers every host as a Proxy built with the same bypass list answers
// bypassUpstream.
func TestNoProxyCoversMatchesBypassUpstream(t *testing.T) {
	lists := [][]string{
		nil,
		{},
		{".corp.internal", "vpce.amazonaws.com", "100.64.0.0/10", "fd00::/8"},
		{"CORP.INTERNAL.", "Registry.Example.COM", "10.20.0.0/16"},
		{"*", "not a host", "*.corp.internal", "http://proxy", "100.64.0.0/99"},
		{"registry", "203.0.113.7/32"},
	}
	hosts := []string{
		"", "   ",
		"corp.internal", "mirror.corp.internal", "MIRROR.CORP.INTERNAL.", "notcorp.internal",
		"corp.internal.evil.example", "vpce-1.vpce.amazonaws.com", "vpce.amazonaws.com.",
		"registry.example.com", "REGISTRY.EXAMPLE.COM", "a.registry.example.com", "registry",
		"100.64.5.7", "100.128.0.1", "10.20.3.4", "10.21.0.1", "203.0.113.7", "203.0.113.8",
		"fd00::1", "FD00::1", "fe80::1", "anything.example.com",
	}
	for _, entries := range lists {
		p := noProxyCoverProxy(entries)
		for _, h := range hosts {
			if got, want := NoProxyCovers(entries, h), p.bypassUpstream(h); got != want {
				t.Errorf("NoProxyCovers(%q, %q) = %v, bypassUpstream = %v", entries, h, got, want)
			}
		}
	}
	// The comparison above is not vacuous: the list covers some hosts and not others.
	entries := lists[2]
	if !NoProxyCovers(entries, "mirror.corp.internal") || !NoProxyCovers(entries, "100.64.5.7") ||
		NoProxyCovers(entries, "notcorp.internal") || NoProxyCovers(entries, "10.0.0.5") {
		t.Errorf("NoProxyCovers(%q) does not separate covered from uncovered hosts", entries)
	}
}

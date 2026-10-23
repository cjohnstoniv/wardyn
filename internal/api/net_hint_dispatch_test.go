// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNetHintDispatchWireConfig is the wire half of the private-range forge pin: a
// self-hosted runner whose resolver answers the forge host into CGNAT or RFC1918.
// The organisation's own local-placement dispatch renders the runner proxy's
// config for a site with no internal_hosts, and for one declaring the forge host
// scoped to each block; each crosses the runner wire and is pinned as the golden
// the proxy package's TestNetHint_PrivateForgeOnARunner boots.
func TestNetHintDispatchWireConfig(t *testing.T) {
	for _, c := range []struct {
		name, golden string
		hosts        []types.InternalHost
	}{
		{"undeclared", "../egress/proxy/testdata/dispatched_net_hint_undeclared.json", nil},
		{"cgnat", "../egress/proxy/testdata/dispatched_net_hint_cgnat.json",
			[]types.InternalHost{{HostSuffix: "dev.azure.com", CIDRs: []string{"100.64.0.0/10"}}}},
		{"rfc1918", "../egress/proxy/testdata/dispatched_net_hint_rfc1918.json",
			[]types.InternalHost{{HostSuffix: "dev.azure.com", CIDRs: []string{"10.0.0.0/8"}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := types.SiteConfig{InternalHosts: c.hosts}
			_, _, spec := dispatchLocalRunAllowing(t, &sc, uuid.New(), "busybox:latest", "http://127.0.0.1:9", "", "dev.azure.com")
			cfg := checkRunnerProxyGolden(t, spec, c.golden)
			if !slices.EqualFunc(cfg.InternalHosts, c.hosts, func(a, b types.InternalHost) bool {
				return a.HostSuffix == b.HostSuffix && slices.Equal(a.CIDRs, b.CIDRs)
			}) {
				t.Fatalf("the runner's proxy config carries internal_hosts %+v, want the site's %+v", cfg.InternalHosts, c.hosts)
			}
		})
	}
}

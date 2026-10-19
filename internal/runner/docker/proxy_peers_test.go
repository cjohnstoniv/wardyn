// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"
)

func allowedPeersOf(t *testing.T, cfg []byte) []netip.Prefix {
	t.Helper()
	var c struct {
		AllowedPeers []netip.Prefix `json:"allowed_peers"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	return c.AllowedPeers
}

// With ProxySandboxPeersOnly on, the run's proxy is told to serve only its own
// network's subnet, on create and on revive; off, its config carries none.
func TestProxySandboxPeersOnly_OnCreateAndRevive(t *testing.T) {
	for _, on := range []bool{true, false} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", ProxySandboxPeersOnly: on})
		ctx := context.Background()
		spec := testSpec()
		sb, err := d.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		f.mu.Lock()
		subnet := f.subnets[internalNetName(spec.RunID)]
		f.mu.Unlock()
		want := []netip.Prefix{subnet}
		if !on {
			want = nil
		}
		name := proxyContainerName(spec.RunID)
		if got := allowedPeersOf(t, f.stdinOf(name)); !slices.Equal(got, want) {
			t.Errorf("on %v: created proxy allowed_peers = %v, want %v", on, got, want)
		}
		if err := d.StopProxy(ctx, sb.Ref); err != nil {
			t.Fatalf("StopProxy: %v", err)
		}
		if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
			t.Fatalf("ReplaceProxy: %v", err)
		}
		if got := allowedPeersOf(t, f.stdinOf(name)); !slices.Equal(got, want) {
			t.Errorf("on %v: revived proxy allowed_peers = %v, want %v", on, got, want)
		}
	}
}

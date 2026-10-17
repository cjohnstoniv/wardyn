// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"testing"
)

func upstreamOf(t *testing.T, cfg []byte) string {
	t.Helper()
	var c struct {
		UpstreamProxyURL string `json:"upstream_proxy_url"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	return c.UpstreamProxyURL
}

// The runner's own upstream proxy reaches its proxy on create AND on revive, whatever the stored config
// held, and a runner without one leaves the config alone.
func TestRunnerUpstreamProxy_ProxyOnCreateAndRevive(t *testing.T) {
	const upstream = "http://corp-proxy.example:3128"
	for _, set := range []string{upstream, ""} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", UpstreamProxyURL: set})
		ctx := context.Background()
		spec := testSpec()
		sb, err := d.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		name := proxyContainerName(spec.RunID)
		if got := upstreamOf(t, f.stdinOf(name)); got != set {
			t.Errorf("config %q: created proxy upstream_proxy_url = %q, want %q", set, got, set)
		}
		if err := d.StopProxy(ctx, sb.Ref); err != nil {
			t.Fatalf("StopProxy: %v", err)
		}
		if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
			t.Fatalf("ReplaceProxy: %v", err)
		}
		if got := upstreamOf(t, f.stdinOf(name)); got != set {
			t.Errorf("config %q: revived proxy upstream_proxy_url = %q, want %q", set, got, set)
		}
	}
}

// withUpstreamProxy replaces a value already in the config and leaves the config alone with none.
func TestWithUpstreamProxy(t *testing.T) {
	in := []byte(`{"run_token":"tok","upstream_proxy_url":"http://old.example:1"}`)
	out, err := withUpstreamProxy(in, "http://new.example:2")
	if err != nil || upstreamOf(t, out) != "http://new.example:2" {
		t.Fatalf("withUpstreamProxy = %s, %v; want the runner's URL in place of the stored one", out, err)
	}
	if out, err := withUpstreamProxy(in, ""); err != nil || string(out) != string(in) {
		t.Fatalf("withUpstreamProxy(none) = %s, %v; want the config untouched", out, err)
	}
}

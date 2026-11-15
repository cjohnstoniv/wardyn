// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// TestRunnerProxyConfigChainsTheRunnersUpstream: in runner mode the proxy config CreateSandbox puts in
// the run's Secret carries runnerd's own --upstream-proxy as upstream_proxy_url, replacing any value
// already there, as the Docker runner's does; with none set, no upstream is added.
func TestRunnerProxyConfigChainsTheRunnersUpstream(t *testing.T) {
	h, _ := sinkHarness(t)
	h.d.cfg.UpstreamProxyURL = "http://proxy.corp.example:3128"
	out, err := h.d.runnerProxyConfig(context.Background(), h.runID, []byte(`{"upstream_proxy_url":"http://stale.example:1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Upstream string `json:"upstream_proxy_url"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil || cfg.Upstream != "http://proxy.corp.example:3128" {
		t.Fatalf("upstream_proxy_url = %q (%v), want the runner's own", cfg.Upstream, err)
	}

	h2, _ := sinkHarness(t)
	out, err = h2.d.runnerProxyConfig(context.Background(), h2.runID, []byte(`{"self_lapse":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("upstream_proxy_url")) {
		t.Fatalf("no runner upstream set, yet the proxy config carries one: %s", out)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A revive whose relay the runner now refuses fails before the running proxy is touched: a plain
// error, not ErrProxyReplaceFailed (which the revive reads as the old proxy being gone), and the old
// proxy is neither drained away nor removed.
func TestReplaceProxy_ARefusedRelayKeepsTheOldProxy(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "host-gateway", nil }
	spec := testSpec()
	spec.ProxyConfig.ControlPlaneURL = "https://wardynd.internal:8443"
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	old := f.containers[proxyContainerName(spec.RunID)]

	d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "", errors.New("injected: the relay is not listening") }
	pc := spec.ProxyConfig
	pc.RunToken = "fresh"
	cfg, err := runner.BuildProxyConfig(spec.RunID, pc, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	err = d.ReplaceProxy(ctx, sb.Ref, cfg)
	if err == nil || errors.Is(err, runner.ErrProxyReplaceFailed) {
		t.Fatalf("ReplaceProxy with the relay refused = %v; want a plain error", err)
	}
	if old.removed {
		t.Error("the running proxy was removed for a revive that could not start its successor")
	}
}

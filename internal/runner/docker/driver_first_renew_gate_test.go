// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestReplaceProxy_SetsTheFirstRenewGateAndCreateDoesNot: a replacement proxy is told to serve only after
// its first token renewal; the proxy a create starts is not.
func TestReplaceProxy_SetsTheFirstRenewGateAndCreateDoesNot(t *testing.T) {
	d, f := dnsTestDriver(Config{InternalNetwork: "wardyn-internal"})
	spec := testSpec()
	ctx := context.Background()
	gate := proxy.FirstRenewGateEnv + "=1"
	t.Setenv(proxy.FirstRenewGateEnv, "1") // set before the create: even "=1" in the runner's own environment is not copied
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if env := f.containers[proxyContainerName(spec.RunID)].cfg.Env; slices.Contains(env, gate) {
		t.Fatalf("the created proxy env = %v, want no %s", env, gate)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if env := f.containers[proxyContainerName(spec.RunID)].cfg.Env; !slices.Contains(env, gate) {
		t.Fatalf("the replacement proxy env = %v, want %s", env, gate)
	}
}

// TestReplaceProxy_OperatorGateSettingCannotOverrideIt: the flag ends the replacement's environment as
// "=1" whatever was copied in ahead of it, and the sidecar knob list does not carry it.
func TestReplaceProxy_OperatorGateSettingCannotOverrideIt(t *testing.T) {
	key := proxy.FirstRenewGateEnv
	got := withFirstRenewGate([]string{"A=1", key + "=0", "B=2", key + "=", "C=3"})
	if want := []string{"A=1", "B=2", "C=3", key + "=1"}; !slices.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	if slices.Contains(runner.ProxySidecarEnvKnobNames(), key) {
		t.Fatalf("%s is in runner.ProxySidecarEnvKnobNames", key)
	}
	t.Setenv(key, "0")
	d, f := dnsTestDriver(Config{InternalNetwork: "wardyn-internal"})
	spec := testSpec()
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	env := f.containers[proxyContainerName(spec.RunID)].cfg.Env
	if n := len(env); n == 0 || env[n-1] != key+"=1" || slices.Contains(env, key+"=0") {
		t.Fatalf("the replacement proxy env = %v, want %s=1 last and no =0", env, key)
	}
}

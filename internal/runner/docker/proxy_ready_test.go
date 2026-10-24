// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The agent is created only after the proxy has printed its ready marker: a proxy that is slow to
// listen must not leave the agent's first requests refused.
func TestCreateSandbox_AgentNotCreatedBeforeProxyReady(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.proxyReadyDelay = 400 * time.Millisecond
	d := newTestDriver(f)
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	agentAt := f.createdAt[agentContainerName(testSpec().RunID)]
	if f.proxyReadyAt.IsZero() || agentAt.IsZero() {
		t.Fatalf("proxy ready at %v, agent created at %v; want both recorded", f.proxyReadyAt, agentAt)
	}
	if agentAt.Before(f.proxyReadyAt) {
		t.Errorf("agent created at %v, before the proxy was ready at %v", agentAt, f.proxyReadyAt)
	}
}

// A proxy that stays up but never prints the marker fails the run with a named reason at the bound, and
// the agent is never created. An image older than the runner is the usual cause.
func TestCreateSandbox_ProxyNeverReadyFailsTheRun(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.proxyNeverReady = true
	d := newTestDriver(f)
	d.proxyReady = 200 * time.Millisecond
	runID := testSpec().RunID
	_, err := d.CreateSandbox(context.Background(), testSpec())
	var notReady *proxyNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("CreateSandbox = %v; want a *proxyNotReadyError", err)
	}
	if _, made := f.createdAt[agentContainerName(runID)]; made {
		t.Error("the agent must never be created behind a proxy that never became ready")
	}
	if p := f.containers[proxyContainerName(runID)]; p != nil && !p.removed {
		t.Error("the unready proxy must be removed")
	}
	if _, ok := f.networks[internalNetName(runID)]; ok {
		t.Error("the per-run network must be rolled back")
	}
}

// Revive starts its proxy through the same wait: ReplaceProxy returns only once the new proxy is ready,
// and one that never is fails the revive as a replace failure.
func TestReplaceProxy_WaitsForReady(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	cfg := renderedTestConfig(t, "fresh")

	f.proxyReadyDelay = 300 * time.Millisecond
	before := time.Now()
	if err := d.ReplaceProxy(ctx, sb.Ref, cfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if f.proxyReadyAt.Before(before) || time.Since(f.proxyReadyAt) < 0 {
		t.Fatalf("ReplaceProxy returned without the new proxy having signalled ready (ready at %v, started at %v)", f.proxyReadyAt, before)
	}

	f.proxyNeverReady = true
	d.proxyReady = 200 * time.Millisecond
	err = d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "again"))
	var notReady *proxyNotReadyError
	if !errors.Is(err, runner.ErrProxyReplaceFailed) || !errors.As(err, &notReady) {
		t.Fatalf("ReplaceProxy with a proxy that never signals = %v; want ErrProxyReplaceFailed wrapping *proxyNotReadyError", err)
	}
}

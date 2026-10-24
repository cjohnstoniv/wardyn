// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"strings"
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

	f.mu.Lock()
	f.proxyReadyDelay = 300 * time.Millisecond
	f.mu.Unlock()
	before := time.Now()
	if err := d.ReplaceProxy(ctx, sb.Ref, cfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if f.proxyReadyAt.Before(before) {
		t.Fatalf("ReplaceProxy returned without the new proxy having signalled ready (ready at %v, started at %v)", f.proxyReadyAt, before)
	}

	f.mu.Lock()
	f.proxyNeverReady = true
	f.mu.Unlock()
	d.proxyReady = 200 * time.Millisecond
	err = d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "again"))
	var notReady *proxyNotReadyError
	if !errors.Is(err, runner.ErrProxyReplaceFailed) || !errors.As(err, &notReady) {
		t.Fatalf("ReplaceProxy with a proxy that never signals = %v; want ErrProxyReplaceFailed wrapping *proxyNotReadyError", err)
	}
}

// A proxy image from before the ready marker (no label, and its proxy prints nothing) still starts a
// run, and a revive still replaces its proxy: the runner follows the organisation's image, which can be
// older than the runner.
func TestCreateSandbox_ProxyImageWithoutTheMarkerStillStarts(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.legacyProxyImages = map[string]bool{"wardyn-proxy:dev": true}
	d := newTestDriver(f)
	d.proxyReady = 200 * time.Millisecond // far under what an unlabelled image would wait for if it were waited on
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox with a proxy image that predates the ready marker: %v", err)
	}
	if _, made := f.createdAt[agentContainerName(testSpec().RunID)]; !made {
		t.Error("the agent was never created behind an older proxy image")
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy with a proxy image that predates the ready marker: %v", err)
	}
}

// An older image that exits at config load is still named, by the exit-watch.
func TestCreateSandbox_ProxyImageWithoutTheMarkerExitingIsStillNamed(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.legacyProxyImages = map[string]bool{"wardyn-proxy:dev": true}
	d := newTestDriver(f)
	d.proxySettle = proxyStartSettle
	runID := testSpec().RunID
	f.exitAfterInspects = map[string]int{proxyContainerName(runID): 3}
	f.logs = map[string][]byte{proxyContainerName(runID): muxFrame(1, `parse config: unexpected end of JSON input`)}
	_, err := d.CreateSandbox(context.Background(), testSpec())
	if err == nil || !strings.Contains(err.Error(), "proxy exited at config load (exit 1)") {
		t.Fatalf("CreateSandbox = %v; want the named config-load cause", err)
	}
}

// The wardyn-proxy image declares the label the runner reads, so a release that prints the marker is
// the one the runner waits on.
func TestProxyDockerfileDeclaresTheReadyMarkerLabel(t *testing.T) {
	body := ccRead(t, ccAbs(t, "../../../deploy/compose/Dockerfile.proxy"))
	if !strings.Contains(body, proxyReadyLabel+`="1"`) {
		t.Errorf("deploy/compose/Dockerfile.proxy does not set LABEL %s=\"1\"", proxyReadyLabel)
	}
}

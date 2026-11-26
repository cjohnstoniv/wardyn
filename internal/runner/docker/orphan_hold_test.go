// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

var _ api.SandboxOrphanHolder = (*Driver)(nil)

// holdFixture is one aged, row-less sandbox the sweep lists, created through CreateSandbox as a run's would be.
type holdFixture struct {
	f     *fakeDocker
	d     *Driver
	runID uuid.UUID
	ref   string
}

func newHoldFixture(t *testing.T, aws bool) *holdFixture {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	spec := testSpec()
	if aws {
		spec = awsSpec()
	}
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	f.execExited = true
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{
		{ID: sb.Ref, Created: old, Labels: map[string]string{labelComponent: componentAgent, labelRun: spec.RunID.String()}},
	}
	return &holdFixture{f: f, d: d, runID: spec.RunID, ref: sb.Ref}
}

func (h *holdFixture) removed(name string) bool {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	c := h.f.containers[name]
	return c != nil && c.removed
}

func (h *holdFixture) stopped() bool {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	c := h.f.containers[h.ref]
	return c != nil && c.state != nil && !c.state.Running
}

func (h *holdFixture) sweep(t *testing.T, held func(uuid.UUID) bool) (swept, heldN int) {
	t.Helper()
	swept, heldN, err := h.d.SweepOrphanedSandboxesHolding(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return true }, held)
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxesHolding: %v", err)
	}
	return swept, heldN
}

func holdAll(uuid.UUID) bool { return true }

func TestSweepOrphanedSandboxesHolding_StopsTheProxyAndKeepsTheAgentContainer(t *testing.T) {
	h := newHoldFixture(t, false)
	swept, heldN := h.sweep(t, holdAll)
	if swept != 0 || heldN != 1 {
		t.Fatalf("swept %d, held %d; want 0, 1", swept, heldN)
	}
	if h.removed(h.ref) || !h.stopped() {
		t.Error("the agent container must be stopped and kept, with its files")
	}
	if !h.removed(proxyContainerName(h.runID)) {
		t.Error("the proxy survived: a held sandbox must have no network path")
	}
}

func TestSweepOrphanedSandboxesHolding_AFailedEndIsTornDown(t *testing.T) {
	h := newHoldFixture(t, false)
	h.f.stopFailures = map[string]int{h.ref: 1} // the end's stop fails once; the teardown's goes through
	swept, heldN := h.sweep(t, holdAll)
	if swept != 1 || heldN != 0 {
		t.Fatalf("swept %d, held %d; want 1, 0", swept, heldN)
	}
	if !h.removed(h.ref) {
		t.Error("a hold that could not be confirmed must fall back to the teardown")
	}
}

func TestSweepOrphanedSandboxesHolding_AFirstPassWipeFailureIsTornDown(t *testing.T) {
	h := newHoldFixture(t, true)
	h.f.containers[h.ref].state.Running = false // the wipe cannot run: ErrAWSCacheWipeFailed on a first hold
	swept, heldN := h.sweep(t, holdAll)
	if swept != 1 || heldN != 0 {
		t.Fatalf("swept %d, held %d; want 1, 0: the credential caches were not wiped", swept, heldN)
	}
	if !h.removed(h.ref) {
		t.Error("a sandbox whose caches could not be wiped must not be kept")
	}
}

func TestSweepOrphanedSandboxesHolding_AReassertOnAStoppedSandboxStaysHeld(t *testing.T) {
	h := newHoldFixture(t, true)
	if swept, heldN := h.sweep(t, holdAll); swept != 0 || heldN != 1 {
		t.Fatalf("first pass: swept %d, held %d; want 0, 1", swept, heldN)
	}
	// The second pass finds the agent stopped: its wipe cannot run, which this process accepts for a sandbox it
	// already held cleanly.
	if swept, heldN := h.sweep(t, holdAll); swept != 0 || heldN != 1 {
		t.Fatalf("second pass: swept %d, held %d; want 0, 1", swept, heldN)
	}
	if h.removed(h.ref) {
		t.Error("a re-assert tore down a sandbox it had held")
	}
}

func TestSweepOrphanedSandboxesHolding_KeepsTheRunNetwork(t *testing.T) {
	h := newHoldFixture(t, false)
	h.sweep(t, holdAll)
	if _, ok := h.f.networks[internalNetName(h.runID)]; !ok {
		t.Error("the held sandbox's per-run network was removed")
	}
}

func TestSweepOrphanedSandboxesHolding_ANilHoldIsTodaysSweep(t *testing.T) {
	h := newHoldFixture(t, false)
	swept, heldN := h.sweep(t, nil)
	if swept != 1 || heldN != 0 || !h.removed(h.ref) {
		t.Fatalf("swept %d, held %d, agent removed %v; want 1, 0, true", swept, heldN, h.removed(h.ref))
	}
}

// With the run subnet pool set, the network sweep leaves a held run's network alone, and removes it once the hold
// no longer names the run.
func TestSweepOrphanedSandboxesHolding_TheNetworkSweepLeavesAHeldRunsNetwork(t *testing.T) {
	f := newFakeDocker()
	d := newPoolDriver(f, "10.200.0.0/24")
	held := uuid.New()
	ctx := context.Background()
	if _, err := d.createRunNetwork(ctx, held, d.labels(held, componentNetwork, nil), engineVersionOf(t, d)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.netCreated[internalNetName(held)] = time.Now().Add(-time.Hour)
	f.mu.Unlock()
	all := func(uuid.UUID) bool { return true }
	if n, h, err := d.SweepOrphanedSandboxesHolding(ctx, 10*time.Minute, all, all); err != nil || n != 0 || h != 0 {
		t.Fatalf("sweep = %d swept, %d held, %v; want 0, 0: the hold names the run", n, h, err)
	}
	if _, ok := f.networks[internalNetName(held)]; !ok {
		t.Fatal("a held run's network was swept")
	}
	if n, _, err := d.SweepOrphanedSandboxesHolding(ctx, 10*time.Minute, all, func(uuid.UUID) bool { return false }); err != nil || n != 1 {
		t.Fatalf("sweep with the hold over = %d, %v; want the network swept", n, err)
	}
}

// The same, with no run subnet pool: the network sweep runs on every host, and still leaves a held run's network
// alone until the hold no longer names the run.
func TestSweepOrphanedSandboxesHolding_TheNetworkSweepLeavesAHeldRunsNetworkWithoutAPool(t *testing.T) {
	f := newFakeDocker()
	d := newTestDriver(f)
	held := uuid.New()
	ctx := context.Background()
	if _, err := d.createRunNetwork(ctx, held, d.labels(held, componentNetwork, nil), engineVersionOf(t, d)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.netCreated[internalNetName(held)] = time.Now().Add(-time.Hour)
	f.mu.Unlock()
	all := func(uuid.UUID) bool { return true }
	if n, h, err := d.SweepOrphanedSandboxesHolding(ctx, 10*time.Minute, all, all); err != nil || n != 0 || h != 0 {
		t.Fatalf("sweep = %d swept, %d held, %v; want 0, 0: the hold names the run", n, h, err)
	}
	if _, ok := f.networks[internalNetName(held)]; !ok {
		t.Fatal("a held run's network was swept")
	}
	if n, _, err := d.SweepOrphanedSandboxesHolding(ctx, 10*time.Minute, all, func(uuid.UUID) bool { return false }); err != nil || n != 1 {
		t.Fatalf("sweep with the hold over = %d, %v; want the network swept", n, err)
	}
	if _, ok := f.networks[internalNetName(held)]; ok {
		t.Error("the network is still there once the hold is over")
	}
}

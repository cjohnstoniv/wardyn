// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// fakeSweepImageBuilder implements api.ImageBuilder and the optional
// api.ImageBuildSweeper capability, so ReconcileOnBoot's type assertion finds
// it — mirrors sweepableImageBuilder in internal/api/reconcile_exec_test.go
// (unexported there, so this package needs its own).
type fakeSweepImageBuilder struct{ swept int }

func (*fakeSweepImageBuilder) BuildDevcontainer(context.Context, string, string, string, io.Writer) (string, error) {
	return "", nil
}
func (*fakeSweepImageBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string, io.Writer) (string, error) {
	return "", nil
}
func (*fakeSweepImageBuilder) FinalizeBase(context.Context, string, string, io.Writer) (string, error) {
	return "", nil
}
func (f *fakeSweepImageBuilder) SweepOrphanedBuilds(context.Context) error {
	f.swept++
	return nil
}

// TestStartBackgroundWorkers_ReconcilesBootIndependentOfRunner pins the
// wave-1 regression at its actual call site: api.Server.ReconcileOnBoot
// already self-limits correctly on a nil Runner (internal/api/reconcile.go
// runs the envbuild sweep BEFORE checking s.cfg.Runner), but this package's
// own call to it used to be wrapped in `if run != nil`, so a
// `-runner none -envbuild` headless-API deployment never swept orphaned
// build containers even though ReconcileOnBoot was fully able to. Constructs
// exactly that configuration: run == nil, an ImageBuilder wired standalone.
func TestStartBackgroundWorkers_ReconcilesBootIndependentOfRunner(t *testing.T) {
	fb := &fakeSweepImageBuilder{}
	srv := api.New(api.Config{ImageBuilder: fb, RunnerTarget: "none"})

	zeroDur := time.Duration(0)
	zeroInt := 0
	f := &bootFlags{
		// Every OTHER background worker stays off so this test only needs
		// run + srv: no runner (asserted below), no autostop, no groundtruth
		// rotator (env unset), no approval sweeper, no recording sweeper.
		autoStopInterval:       &zeroDur,
		approvalExpiryInterval: &zeroDur,
		approvalExpiryAfter:    &zeroDur,
		recordingRetention:     &zeroInt,
	}
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", "")

	startBackgroundWorkers(context.Background(), f, srv, nil /* run */, nil, nil, nil, nil, nil)

	if fb.swept != 1 {
		t.Errorf("SweepOrphanedBuilds called %d times, want 1 — startBackgroundWorkers with a nil runner must still run ReconcileOnBoot's runner-independent envbuild orphan sweep", fb.swept)
	}
}

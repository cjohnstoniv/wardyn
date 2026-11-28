// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// SnapshotSandbox forwards a snapshot's cut to ref's substrate when it implements runner.Snapshotter.
func (o *Orchestrator) SnapshotSandbox(ctx context.Context, ref string, req runner.SnapshotRequest) (runner.SnapshotCut, error) {
	sn, err := o.snapshotterForRef(ctx, ref)
	if err != nil {
		return runner.SnapshotCut{}, err
	}
	return sn.SnapshotSandbox(ctx, ref, req)
}

// CommitSnapshot forwards a snapshot's commit to ref's substrate; see SnapshotSandbox.
func (o *Orchestrator) CommitSnapshot(ctx context.Context, ref string, id uuid.UUID) (runner.SnapshotLayer, error) {
	sn, err := o.snapshotterForRef(ctx, ref)
	if err != nil {
		return runner.SnapshotLayer{}, err
	}
	return sn.CommitSnapshot(ctx, ref, id)
}

// AbortSnapshot forwards a snapshot's abort to ref's substrate; see SnapshotSandbox.
func (o *Orchestrator) AbortSnapshot(ctx context.Context, ref string, id uuid.UUID) error {
	sn, err := o.snapshotterForRef(ctx, ref)
	if err != nil {
		return err
	}
	return sn.AbortSnapshot(ctx, ref, id)
}

// SnapshotterFor is the runner.Snapshotter of the runner that holds a snapshot's layer, for the calls that
// name no sandbox (DeleteSnapshot, ListSnapshots): runnerID's registered substrate, or for "" the first local
// substrate that snapshots. A layer outlives its run, so it is found by its runner, never by a ref.
func (o *Orchestrator) SnapshotterFor(runnerID string) (runner.Snapshotter, error) {
	if runnerID == "" {
		for _, s := range o.locals() {
			if sn, ok := s.(runner.Snapshotter); ok {
				return sn, nil
			}
		}
		return nil, runner.ErrSnapshotUnsupported
	}
	s, err := o.substrateForRunner(runnerID)
	if err != nil {
		return nil, err
	}
	sn, ok := s.(runner.Snapshotter)
	if !ok {
		return nil, runner.ErrSnapshotUnsupported
	}
	return sn, nil
}

func (o *Orchestrator) snapshotterForRef(ctx context.Context, ref string) (runner.Snapshotter, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	sn, ok := s.(runner.Snapshotter)
	if !ok {
		return nil, runner.ErrSnapshotUnsupported
	}
	return sn, nil
}

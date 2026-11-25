// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The snapshot calls (runner.Snapshotter) go only to a runner whose caps carry Snapshots; any other answers
// runner.ErrSnapshotUnsupported with no call made. A refusal comes back as its sentinel (SnapshotReply.Err).

func (s *Substrate) snapshotCall(ctx context.Context, method string, a runnerwire.SnapshotArgs, ref bool) (runnerwire.SnapshotReply, error) {
	var r runnerwire.SnapshotReply
	if c, ok := s.Caps(); !ok || !c.Snapshots {
		return r, runner.ErrSnapshotUnsupported
	}
	if ref {
		if err := s.own(a.Ref); err != nil {
			return r, err
		}
	}
	if err := s.call(ctx, method, a, &r); err != nil {
		return r, err
	}
	return r, r.Err()
}

// SnapshotSandbox implements runner.Snapshotter.
func (s *Substrate) SnapshotSandbox(ctx context.Context, ref string, req runner.SnapshotRequest) (runner.SnapshotCut, error) {
	r, err := s.snapshotCall(ctx, runnerwire.MethodSnapshotSandbox, runnerwire.SnapshotArgs{Ref: ref, ID: req.ID}, true)
	if err != nil {
		return runner.SnapshotCut{}, err
	}
	if r.Cut == nil || r.Cut.ID != req.ID {
		return runner.SnapshotCut{}, errors.New("remote: the runner answered the snapshot with no cut of it")
	}
	return *r.Cut, nil
}

// CommitSnapshot implements runner.Snapshotter.
func (s *Substrate) CommitSnapshot(ctx context.Context, ref string, id uuid.UUID) (runner.SnapshotLayer, error) {
	r, err := s.snapshotCall(ctx, runnerwire.MethodCommitSnapshot, runnerwire.SnapshotArgs{Ref: ref, ID: id}, true)
	if err != nil {
		return runner.SnapshotLayer{}, err
	}
	if r.Layer == nil || r.Layer.ID != id {
		return runner.SnapshotLayer{}, errors.New("remote: the runner answered the commit with no layer of it")
	}
	return *r.Layer, nil
}

// AbortSnapshot implements runner.Snapshotter.
func (s *Substrate) AbortSnapshot(ctx context.Context, ref string, id uuid.UUID) error {
	_, err := s.snapshotCall(ctx, runnerwire.MethodAbortSnapshot, runnerwire.SnapshotArgs{Ref: ref, ID: id}, true)
	return err
}

// DeleteSnapshot implements runner.Snapshotter.
func (s *Substrate) DeleteSnapshot(ctx context.Context, id uuid.UUID) error {
	_, err := s.snapshotCall(ctx, runnerwire.MethodDeleteSnapshot, runnerwire.SnapshotArgs{ID: id}, false)
	return err
}

// ListSnapshots implements runner.Snapshotter.
func (s *Substrate) ListSnapshots(ctx context.Context) ([]runner.SnapshotLayer, error) {
	r, err := s.snapshotCall(ctx, runnerwire.MethodListSnapshots, runnerwire.SnapshotArgs{}, false)
	return r.Layers, err
}

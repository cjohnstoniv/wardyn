// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// snapshot serves the snapshot calls on the local substrate's runner.Snapshotter. A cut, commit or abort names
// a sandbox this runner holds; a snapshot refusal is answered as the reply's Refusal.
func (d *Dispatcher) snapshot(ctx context.Context, method string, args json.RawMessage) (any, error) {
	var a runnerwire.SnapshotArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	s, ok := d.cfg.Substrate.(runner.Snapshotter)
	if !ok {
		return runnerwire.SnapshotReply{Refusal: runnerwire.SnapshotRefusal(runner.ErrSnapshotUnsupported)}, nil
	}
	ref := ""
	switch method {
	case runnerwire.MethodSnapshotSandbox, runnerwire.MethodCommitSnapshot, runnerwire.MethodAbortSnapshot:
		var err error
		if ref, err = d.local(a.Ref); err != nil {
			return nil, err
		}
		if err := d.owned(ctx, ref); err != nil {
			return nil, err
		}
	}
	var reply runnerwire.SnapshotReply
	var err error
	switch method {
	case runnerwire.MethodSnapshotSandbox:
		var cut runner.SnapshotCut
		cut, err = s.SnapshotSandbox(ctx, ref, runner.SnapshotRequest{ID: a.ID})
		reply.Cut = &cut
	case runnerwire.MethodCommitSnapshot:
		var l runner.SnapshotLayer
		l, err = s.CommitSnapshot(ctx, ref, a.ID)
		reply.Layer = &l
	case runnerwire.MethodAbortSnapshot:
		err = s.AbortSnapshot(ctx, ref, a.ID)
	case runnerwire.MethodDeleteSnapshot:
		err = s.DeleteSnapshot(ctx, a.ID)
	default:
		reply.Layers, err = s.ListSnapshots(ctx)
	}
	if r := runnerwire.SnapshotRefusal(err); r != "" {
		return runnerwire.SnapshotReply{Refusal: r}, nil
	}
	if err != nil {
		return nil, err
	}
	return reply, nil
}

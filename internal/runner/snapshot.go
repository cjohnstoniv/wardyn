// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Snapshotter is an OPTIONAL Runner capability: a disk-only snapshot of a running Linux VM sandbox, made in
// two phases so no layer becomes usable without the caller's say. SnapshotSandbox cuts a point-in-time copy of
// the run's disk into a new file that no running disk backs onto, and returns at the cut's end while the copy
// still runs. The caller then sends exactly one of CommitSnapshot (the copy is finished, flattened onto the
// image's base and kept as the snapshot's layer) or AbortSnapshot (the copy is cancelled and deleted). A
// snapshot with neither within a bound the runner keeps is aborted by the runner itself. The source run's own
// disk and its backing are never changed, and guest memory is never saved. Only the Docker driver's Linux VMs
// implement it; a substrate without it answers ErrSnapshotUnsupported.
type Snapshotter interface {
	SnapshotSandbox(ctx context.Context, ref string, req SnapshotRequest) (SnapshotCut, error)
	// CommitSnapshot waits for the copy, then keeps the layer. It answers ErrSnapshotNotPending for an id the
	// runner does not hold pending: aborted, timed out, deleted, or cut by an earlier runner process. A repeat
	// of a Commit that succeeded answers the same layer.
	CommitSnapshot(ctx context.Context, ref string, id uuid.UUID) (SnapshotLayer, error)
	// AbortSnapshot cancels the copy and deletes it. An id the runner does not hold pending is already gone.
	AbortSnapshot(ctx context.Context, ref string, id uuid.UUID) error
	// DeleteSnapshot deletes the snapshot's layer, or aborts it while it is pending, and answers only once no
	// file of it is left. It answers ErrSnapshotInUse while a run on this runner backs onto the layer, and
	// success for an id it holds nothing for.
	DeleteSnapshot(ctx context.Context, id uuid.UUID) error
	// ListSnapshots is every committed layer the runner holds.
	ListSnapshots(ctx context.Context) ([]SnapshotLayer, error)
}

// SnapshotRequest is one snapshot to cut. ID is the caller's id for it, which names the layer.
type SnapshotRequest struct {
	ID uuid.UUID `json:"id"`
}

// SnapshotCut is SnapshotSandbox's answer: the copy's point in time lies between CutStart and CutEnd.
type SnapshotCut struct {
	ID       uuid.UUID `json:"id"`
	CutStart time.Time `json:"cut_start"`
	CutEnd   time.Time `json:"cut_end"`
}

// SnapshotLayer is a committed layer: its size and digest, the stamp of the base file it backs onto, and the
// image that base was made from (ImageID, the runner's image id; ImageDigest, the image's repository digest,
// empty when it has none or several). It names no content and no credential.
type SnapshotLayer struct {
	ID          uuid.UUID `json:"id"`
	Bytes       int64     `json:"bytes"`
	SHA256      string    `json:"sha256"`
	BaseStamp   string    `json:"base_stamp"`
	ImageID     string    `json:"image_id"`
	ImageDigest string    `json:"image_digest,omitempty"`
}

// SnapshotSource makes a linux_vm create a fork: its disk is a new overlay on the snapshot's layer. ImageID
// and BaseStamp are the layer's, and the runner refuses ErrForkBaseChanged when the run's image now resolves
// to another id, that id's base is gone, or the base file is not the one the layer was cut on. It names no
// credential.
type SnapshotSource struct {
	ID        uuid.UUID `json:"id"`
	ImageID   string    `json:"image_id"`
	BaseStamp string    `json:"base_stamp"`
}

// The snapshot refusals. Each carries its reason, which the wire keeps (runnerwire.SnapshotRefusal).
var (
	ErrSnapshotUnsupported = errors.New("runner: this sandbox cannot be snapshotted here: only a Linux VM on a runner with snapshots can")
	ErrSnapshotNotPending  = errors.New("runner: snapshot_not_pending: this runner holds no pending snapshot of that id")
	ErrSnapshotInUse       = errors.New("runner: snapshot_in_use: a run on this runner backs onto that snapshot")
	ErrSnapshotBusy        = errors.New("runner: snapshot_busy: another snapshot of this run is still being made")
	ErrSnapshotNoSpace     = errors.New("runner: snapshot_no_space: the runner has too little free disk for this snapshot")
	ErrForkBaseChanged     = errors.New("runner: fork_base_changed: the image the snapshot was taken on is no longer the one on this runner")
)

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// MachineSnapshotState is where a snapshot of a Linux VM run's disk is in its life.
type MachineSnapshotState string

const (
	// MachineSnapshotCreating: the copy is being made, and the layer is not usable yet.
	MachineSnapshotCreating MachineSnapshotState = "creating"
	// MachineSnapshotReady: the runner committed the layer, and a fork may use it.
	MachineSnapshotReady MachineSnapshotState = "ready"
	// MachineSnapshotDeleting: a delete has started; no fork may use it any more.
	MachineSnapshotDeleting MachineSnapshotState = "deleting"
	// MachineSnapshotFailed: the snapshot was refused or lost; Failure says why.
	MachineSnapshotFailed MachineSnapshotState = "failed"
)

// MachineSnapshot is one snapshot of a Linux VM run's disk: a layer kept on the runner that made it, which
// backs only onto the image's base. It names no credential.
type MachineSnapshot struct {
	ID uuid.UUID `json:"id"`
	// SourceRunID is the run the snapshot was taken of; nil once that run's row is gone.
	SourceRunID  *uuid.UUID  `json:"source_run_id,omitempty"`
	OwnerSubject string      `json:"owner_subject"`
	RunnerID     uuid.UUID   `json:"runner_id"`
	MachineKind  MachineKind `json:"machine_kind"`
	// ImageRef, ImageDigest and ImageID are the image the layer backs onto: the ref the run was admitted with,
	// its digest ("" when it has none, and then no fork can use the snapshot) and the runner's image id.
	ImageRef    string    `json:"image_ref"`
	ImageDigest string    `json:"image_digest"`
	ImageID     string    `json:"image_id"`
	VMMachine   VMMachine `json:"vm_machine"`
	LayerName   string    `json:"layer_name"`
	LayerSHA256 string    `json:"layer_sha256,omitempty"`
	Bytes       int64     `json:"bytes,omitempty"`
	// BaseStamp is the base file's inode, size, mtime and ctime when the layer was cut.
	BaseStamp    string               `json:"base_stamp,omitempty"`
	UnitID       *uuid.UUID           `json:"unit_id,omitempty"`
	WorkspaceIDs []uuid.UUID          `json:"workspace_ids,omitempty"`
	State        MachineSnapshotState `json:"state"`
	// Failure is the reason a failed snapshot failed.
	Failure    string     `json:"failure,omitempty"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CutStart   *time.Time `json:"cut_start,omitempty"`
	CutEnd     *time.Time `json:"cut_end,omitempty"`
	// LayerGoneAt is when the runner confirmed it holds no layer for a failed snapshot.
	LayerGoneAt *time.Time `json:"layer_gone_at,omitempty"`
}

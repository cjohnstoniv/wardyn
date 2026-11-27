// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// SnapDir is where a fork's layer is mounted, read-only, in its overlay step and its QEMU container: a
// snapshot's StateDir/snapshots/<id> directory, which holds SnapLayer.
const (
	SnapDir   = "/vm/snap"
	SnapLayer = "layer.qcow2"
)

// SnapshotTarget is the file a snapshot's copy is made into, in the run's DiskDir. No running disk backs onto
// it, so the source VM's own chain never changes.
func SnapshotTarget(id uuid.UUID) string { return "snap-" + id.String() + ".qcow2" }

// SnapshotNode is the QMP node name and job id of a snapshot's copy: QEMU keeps node names under 32 bytes.
func SnapshotNode(id uuid.UUID) string {
	return "wsnap" + strings.ReplaceAll(id.String(), "-", "")[:16]
}

// SnapshotTargetArgs creates the empty target with the source disk's own backing: the image's base for a
// plain run, or the layer a fork's overlay backs onto (fork true).
func SnapshotTargetArgs(id uuid.UUID, fork bool) []string {
	backing, format := BaseDir+"/"+RootFS, "raw"
	if fork {
		backing, format = SnapDir+"/"+SnapLayer, "qcow2"
	}
	return []string{"qemu-img", "create", "-q", "-f", "qcow2", "-F", format, "-b", backing, DiskDir + "/" + SnapshotTarget(id)}
}

// FlattenArgs rebases a fork's finished copy onto the image's base. It is the safe (copying) rebase, run
// only on the closed target, which no VM has open, so a layer never backs onto another layer.
func FlattenArgs(id uuid.UUID) []string {
	return []string{"qemu-img", "rebase", "-q", "-f", "qcow2", "-F", "raw", "-b", BaseDir + "/" + RootFS, DiskDir + "/" + SnapshotTarget(id)}
}

// ForkOverlayArgs creates a fork's overlay on the snapshot's layer, mounted read-only at SnapDir.
func ForkOverlayArgs() []string {
	return []string{"qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", SnapDir + "/" + SnapLayer, DiskDir + "/" + Overlay}
}

// SnapshotCommands are the only QMP and qemu-ga commands the snapshot code sends: a frozen filesystem, a
// copy into a new node, and that copy's job and node. None saves guest memory or changes a live disk's chain.
var SnapshotCommands = []string{"guest-fsfreeze-freeze", "guest-fsfreeze-thaw", "query-block", "blockdev-add",
	"blockdev-backup", "query-jobs", "job-cancel", "job-dismiss", "blockdev-del"}

// DiskDevice is the block device QEMU opened DiskDir/Overlay as: its backend name, or its node name when it
// has none.
func (q *QMP) DiskDevice(ctx context.Context) (string, error) {
	var devs []struct {
		Device   string `json:"device"`
		Inserted *struct {
			File     string `json:"file"`
			NodeName string `json:"node-name"`
		} `json:"inserted"`
	}
	if err := q.Execute(ctx, "query-block", nil, &devs); err != nil {
		return "", err
	}
	for _, d := range devs {
		if d.Inserted != nil && d.Inserted.File == DiskDir+"/"+Overlay {
			if d.Device != "" {
				return d.Device, nil
			}
			return d.Inserted.NodeName, nil
		}
	}
	return "", errors.New("qemu: no block device holds the run's disk")
}

// StartBackup opens the snapshot's target as a node and starts its copy from device: sync top, so the copy
// holds what the disk holds above its backing at this moment, and kept after it ends (no auto-dismiss), so
// the job's outcome can be read before it is dismissed.
func (q *QMP) StartBackup(ctx context.Context, device string, id uuid.UUID) error {
	node := SnapshotNode(id)
	add := map[string]any{"driver": "qcow2", "node-name": node,
		"file": map[string]any{"driver": "file", "filename": DiskDir + "/" + SnapshotTarget(id)}}
	if err := q.Execute(ctx, "blockdev-add", add, nil); err != nil {
		return err
	}
	backup := map[string]any{"job-id": node, "device": device, "target": node, "sync": "top", "auto-dismiss": false}
	if err := q.Execute(ctx, "blockdev-backup", backup, nil); err != nil {
		_ = q.Execute(ctx, "blockdev-del", map[string]any{"node-name": node}, nil)
		return err
	}
	return nil
}

// Job is a block job as query-jobs reports it. Found is false when QEMU holds no job of that id.
type Job struct {
	Found  bool
	Status string // e.g. running, concluded
	Error  string // set when a concluded job failed or was cancelled
}

// Ended says the job holds no more writes: it concluded, or there is none.
func (j Job) Ended() bool { return !j.Found || j.Status == "concluded" || j.Status == "null" }

// SnapshotJob is the snapshot's copy job.
func (q *QMP) SnapshotJob(ctx context.Context, id uuid.UUID) (Job, error) {
	var jobs []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := q.Execute(ctx, "query-jobs", nil, &jobs); err != nil {
		return Job{}, err
	}
	for _, j := range jobs {
		if j.ID == SnapshotNode(id) {
			return Job{Found: true, Status: j.Status, Error: j.Error}, nil
		}
	}
	return Job{}, nil
}

// CancelSnapshotJob asks the copy to stop.
func (q *QMP) CancelSnapshotJob(ctx context.Context, id uuid.UUID) error {
	return q.Execute(ctx, "job-cancel", map[string]any{"id": SnapshotNode(id)}, nil)
}

// CloseSnapshot dismisses the ended copy job and closes its node, so QEMU holds the target open no longer.
// A job or node already gone is no error.
func (q *QMP) CloseSnapshot(ctx context.Context, id uuid.UUID) error {
	node := SnapshotNode(id)
	if j, err := q.SnapshotJob(ctx, id); err != nil {
		return err
	} else if j.Found {
		if err := q.Execute(ctx, "job-dismiss", map[string]any{"id": node}, nil); err != nil {
			return err
		}
	}
	err := q.Execute(ctx, "blockdev-del", map[string]any{"node-name": node}, nil)
	if err != nil && strings.Contains(err.Error(), "Failed to find node") {
		return nil
	}
	return err
}

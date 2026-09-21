// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ReclaimDrive implements runner.DriveReclaimer (#166): the ONE call in this
// package that destroys a member's data, and the only caller of VolumeRemove.
//
// INSPECT, JUDGE, THEN REMOVE — never remove by name. ensureDriveVolume
// already refuses to MOUNT a volume that answers to a drive's name but is not
// that drive's object (driveVolumeAdoptable: a hand-made volume with driver
// options, another drive's id under a colliding minted name, another
// principal's subject under a home template that folds two people onto one).
// The identical predicate runs here, against the same three refusals, because
// the two mistakes are the same mistake: a wrong adoption shows one member
// another member's files, and a wrong reclaim deletes them.
//
// What the caller reads is a SENTINEL, not driveVolumeAdoptable's sentence.
// Those sentences are written for the member whose run was refused and carry
// mount remedies ("give this drive a home template that names one directory
// per person") that mean nothing to an operator who asked to destroy
// something. The evidence goes to the operator's log line instead, the same
// split refuseForeignDriveClaim draws on the Kubernetes side.
//
// FORCE IS NEVER SET. Docker refuses to remove a volume a container still
// uses and answers 409; that refusal is the "a run still holds this" check,
// so it is mapped to runner.ErrDriveInUse rather than overridden. A forced
// remove would pull the storage out from under a live agent mid-write.
//
// Scoped to docker_volume, and the refusal for anything else is the point
// rather than an omission: a host_path drive's object is a DIRECTORY ON THE
// OPERATOR'S OWN FILESYSTEM, inside a tree they mounted and named. Wardyn
// never created it and must never delete it — there is no rm -rf here, at any
// privilege, for any backend.
func (d *Driver) ReclaimDrive(ctx context.Context, drive types.DriveMount) (runner.DriveReclaimOutcome, error) {
	if drive.Backend != types.DriveBackendDockerVolume {
		return "", fmt.Errorf("docker: drive: backend %q allocates no object this substrate may destroy "+
			"(a host_path drive's home is a directory on your own filesystem): %w", drive.Backend, runner.ErrDriveNotReclaimable)
	}
	name := drive.ObjectName
	if name == "" {
		// The same fail-closed refusal ensureDriveVolume makes, for a worse
		// reason: an empty name there allocates an anonymous volume, and here
		// it would be a delete against whatever the daemon makes of "".
		return "", fmt.Errorf("docker: drive: no object name to reclaim (backend %s): %w", drive.Backend, runner.ErrDriveNotReclaimable)
	}
	res, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if isNotFound(err) {
		return runner.DriveReclaimAlreadyAbsent, nil
	}
	if err != nil {
		// Fail closed, exactly as ensureDriveVolume does on the same answer: a
		// daemon that cannot say what the volume IS must not have it deleted.
		return "", fmt.Errorf("docker: drive: inspect volume %q before reclaiming it: %w", name, err)
	}
	if adoptErr := driveVolumeAdoptable(res.Volume, name, &drive); adoptErr != nil {
		slog.Warn("wardynd: docker substrate: refusing to reclaim a volume that is not this drive's object",
			slog.String("volume", name), slog.String("drive", drive.DriveName),
			slog.String("drive_id", drive.DriveID.String()), slog.String("reason", adoptErr.Error()))
		return "", fmt.Errorf("docker: drive: refusing to reclaim volume %q: %w", name, runner.ErrDriveNotReclaimable)
	}
	if _, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil {
		switch {
		case isNotFound(err):
			// Raced by another reclaim between the inspect and the remove: the
			// asked-for end state, not caused by this call.
			return runner.DriveReclaimAlreadyAbsent, nil
		case errdefs.IsConflict(err):
			return "", fmt.Errorf("docker: drive: volume %q is still mounted by a container: %w", name, runner.ErrDriveInUse)
		}
		return "", fmt.Errorf("docker: drive: remove volume %q: %w", name, err)
	}
	slog.Warn("wardynd: docker substrate: a drive's volume was deleted by an operator reclaim",
		slog.String("volume", name), slog.String("drive_id", drive.DriveID.String()),
		slog.String("home", drive.HomeName))
	return runner.DriveReclaimDeleted, nil
}

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

// ReclaimDrive implements runner.DriveReclaimer: the ONE call in this package that destroys
// a member's data, and the only caller of VolumeRemove.
//
// INSPECT, JUDGE, THEN REMOVE — never remove by name. ensureDriveVolume already refuses to
// mount a volume that answers to a drive's name but is not that drive's object
// (driveVolumeAdoptable checks the same three cases); the same predicate runs here, since a
// wrong adoption shows one member another's files and a wrong reclaim deletes them — the
// same mistake either way.
//
// The caller sees a SENTINEL, not driveVolumeAdoptable's mount-remedy sentence, which means
// nothing to an operator asking to destroy something; the evidence goes to the operator's
// log line instead.
//
// FORCE IS NEVER SET: Docker's 409 on a volume still in use by a container IS the "a run
// still holds this" check, mapped to runner.ErrDriveInUse rather than overridden. A forced
// remove would pull storage out from under a live agent mid-write.
//
// Scoped to docker_volume; refusing everything else is the point, not an omission — a
// host_path drive's object is a directory on the OPERATOR'S OWN FILESYSTEM. Wardyn never
// created it and must never delete it: there is no rm -rf here, at any privilege, for any backend.
func (d *Driver) ReclaimDrive(ctx context.Context, drive types.DriveMount) (runner.DriveReclaimOutcome, error) {
	if drive.Backend != types.DriveBackendDockerVolume {
		return "", fmt.Errorf("docker: drive: backend %q allocates no object this substrate may destroy "+
			"(a host_path drive's home is a directory on your own filesystem): %w", drive.Backend, runner.ErrDriveNotReclaimable)
	}
	name := drive.ObjectName
	if name == "" {
		// Same fail-closed refusal as ensureDriveVolume, for a worse reason: an empty name
		// there allocates an anonymous volume; here it would delete whatever the daemon makes of "".
		return "", fmt.Errorf("docker: drive: no object name to reclaim (backend %s): %w", drive.Backend, runner.ErrDriveNotReclaimable)
	}
	res, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if isNotFound(err) {
		return runner.DriveReclaimAlreadyAbsent, nil
	}
	if err != nil {
		// Fail closed, exactly as ensureDriveVolume: a daemon that can't say what the volume IS must not have it deleted.
		return "", fmt.Errorf("docker: drive: inspect volume %q before reclaiming it: %w", name, err)
	}
	if adoptErr := driveVolumeAdoptable(res.Volume, name, &drive); adoptErr != nil {
		slog.Warn("wardynd: docker substrate: refusing to reclaim a volume that is not this drive's object",
			slog.String("volume", name), slog.String("drive", drive.DriveName),
			slog.String("drive_id", drive.DriveID.String()), slog.String("reason", adoptErr.Error()))
		return "", fmt.Errorf("docker: drive: refusing to reclaim volume %q: %w", name, runner.ErrDriveNotReclaimable)
	}
	// RESIDUAL, stated not hidden: unlike Kubernetes claim delete (UID + resourceVersion
	// preconditions), Docker's DELETE /volumes/{name} takes no precondition — a volume
	// removed and re-created under this name between the inspect and this call is removed unjudged.
	if _, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil {
		switch {
		case isNotFound(err):
			// Raced by another reclaim between inspect and remove: the asked-for end state, not caused by this call.
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

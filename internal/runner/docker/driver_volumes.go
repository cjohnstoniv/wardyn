// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Docker named volumes for MANAGED user drives (migration 0054) — the only
// place in this package that creates a volume, and deliberately the only shape
// of volume Wardyn will ever create.
//
// WHY A NAMED VOLUME AND NOT A BIND: a docker_volume drive is MANAGED — the
// operator has told Wardyn "allocate the storage", not "bind this tree" — so
// there is no host path at all. That is a security property, not an
// implementation detail: with no host path there is nothing for the bind
// deny-list to deny, no symlink to re-point between validate and create, and no
// WARDYN_USER_DRIVE_HOST_ROOTS ceiling to be outside of. The host_path backend
// is the one that binds, and it runs the full deny matrix (driver_mounts.go).
//
// OWNERSHIP IS THE IMAGE'S JOB, NOT wardynd's. A fresh named volume mounted
// over a directory that exists in the image inherits that directory's contents
// AND its uid/gid (Docker's copy-up). EVERY agent image therefore `mkdir -p
// /home/agent/drive` owned by agent (uid 1000) — the ones that build on a
// public base do it themselves, the ones that build on a sibling inherit it,
// and cmd/wardynd's TestAgentImagesPreCreateDriveDir holds the whole set to it
// — so the first run against a new drive lands on a directory the agent can
// write. wardynd never chowns anything, never runs a privileged helper, and
// never needs to know the uid.

const (
	// driveVolumeDriver is the volume driver every managed drive is created
	// with. LOCAL, ALWAYS, and never operator-configurable.
	//
	// This is the credential guardrail, stated as code: Docker's own
	// "local" driver can mount a CIFS/NFS share directly when it is handed
	// `--opt type=cifs --opt o=username=…,password=…`, and those options are
	// stored with the volume and echoed verbatim by `docker volume inspect` to
	// anybody who can reach the daemon. Wardyn NEVER creates a volume with
	// driver options for exactly that reason. A share is mounted HOST-SIDE by
	// the operator (fstab/systemd, credentials= file) and reaches Wardyn as a
	// host_path drive, whose credential Wardyn never holds. See
	// docs/OPERATIONS.md "User drives on Docker".
	driveVolumeDriver = "local"

	// labelDrive marks a volume as the storage of one Wardyn user drive, and
	// labelDriveHome names the per-person home segment inside it. Together they
	// are what an operator filters on when reclaiming
	// (`docker volume ls --filter label=wardyn.home=<home>`), which the product
	// deliberately does not do for them (reclaim is a documented command in
	// v1, not code — see DESIGN §3.4).
	//
	// NEITHER IS A RUN LABEL, and that is load-bearing: teardown reaps by
	// wardyn.run-id (labelRun), so a drive volume carrying one would be deleted
	// with the run that happened to mount it — taking the member's persistent
	// storage with it. A drive object outlives every run by definition.
	//
	// labelDrive carries the drive's OBJECT NAME (types.DriveMount.ObjectName),
	// which for this backend is the volume's own name. The drive ROW's uuid
	// would be the stronger discriminator — two managed drives whose home
	// templates collide resolve to one volume name — but DriveMount is the
	// run-facing type and deliberately carries only what a mount needs, so it
	// has no id to stamp. Adding types.DriveMount.DriveID and setting it here
	// is the change to make when a reclaim sweep needs to group volumes by
	// drive row; the label KEY is already the one that grouping wants.
	labelDrive     = "wardyn.drive"
	labelDriveHome = "wardyn.home"
)

// ensureDriveVolume makes sure the named volume backing a MANAGED (docker_volume)
// user drive exists, and returns without touching the daemon's state when it
// already does.
//
// INSPECT-THEN-CREATE rather than create-unconditionally, even though Docker's
// VolumeCreate is itself idempotent on the name: a VolumeCreate against an
// EXISTING volume silently keeps the existing volume's labels and driver and
// reports success, so an unconditional create would look identical whether it
// had just allocated a person's storage or re-affirmed storage they have been
// writing to for months. The inspect makes the difference legible here (and, on
// a real daemon, keeps the create out of the audit-visible event stream for
// every run after the first).
//
// It is NOT rolled back by CreateSandbox's rollback. A drive volume is the
// member's persistent storage, not a per-run object: an empty volume left
// behind by a sandbox that failed to come up is inert, costs nothing, and is
// exactly what the NEXT run for that person expects to find. Deleting it on a
// rollback would be a data-loss path that a transient image pull failure could
// trigger.
//
// A drive is docker_volume BY CONSTRUCTION here: driveMount calls this from
// inside its `case types.DriveBackendDockerVolume` arm and nothing else calls
// it, so there is no nil/backend guard to fall through — a second one would be
// a branch no input can reach, and a test of it would assert nothing.
func ensureDriveVolume(ctx context.Context, cli dockerAPI, drive *types.DriveMount) error {
	name := drive.ObjectName
	if name == "" {
		// Fail closed. An empty object name would make VolumeCreate allocate an
		// ANONYMOUS volume with a random id — storage that is neither the
		// member's drive nor findable by any reclaim command.
		return fmt.Errorf("docker: user drive has no object name (backend %s)", drive.Backend)
	}
	if res, err := cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); err == nil {
		// A NAME COLLISION IS NOT AN ADOPTION. The volume that already answers
		// to this name was not necessarily created by the block below: an
		// operator can precreate one by hand, and `docker volume create --opt
		// type=cifs --opt o=username=…,password=…` is the documented way to do
		// it. Mounting that into a member's sandbox because the names happened
		// to match would hand the drive whatever share those options point at,
		// on a credential Wardyn never chose and cannot see — the exact outcome
		// driveVolumeDriver's no-DriverOpts rule exists to prevent, arrived at
		// through the back door. So adopt only WARDYN's OWN SHAPE: the local
		// driver, and no driver options at all.
		//
		// when DriveMount.DriveID lands: also refuse Labels[labelDrive] != drive id
		if v := res.Volume; v.Driver != driveVolumeDriver || len(v.Options) != 0 {
			return fmt.Errorf("docker: volume %q already exists with driver %q and %d driver option(s), which is not a Wardyn-managed drive "+
				"(a managed drive is always driver %q with no options — a precreated share volume must never be adopted as one); "+
				"rename or remove it, or point this drive at a host_path backend",
				name, v.Driver, len(v.Options), driveVolumeDriver)
		}
		return nil
	} else if !isNotFound(err) {
		// Any error other than "no such volume" is fail-closed: a daemon that
		// cannot answer whether the volume exists must not have one created
		// over the top of whatever it could not read.
		return fmt.Errorf("docker: inspect user drive volume %q: %w", name, err)
	}
	if _, err := cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   name,
		Driver: driveVolumeDriver,
		// NO DriverOpts. Ever. See driveVolumeDriver.
		Labels: map[string]string{
			labelManaged:   "true",
			labelDrive:     name,
			labelDriveHome: drive.HomeName,
		},
	}); err != nil {
		// A concurrent run for the same person can win the race between the
		// inspect above and this create; Docker answers that with success (the
		// name already resolves), so there is no already-exists arm to special-
		// case here — anything that DOES come back is a real failure.
		return fmt.Errorf("docker: create user drive volume %q: %w", name, err)
	}
	return nil
}

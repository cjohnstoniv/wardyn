// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Docker named volumes for MANAGED user drives — the only place in this
// package that creates a volume, and the only shape it ever creates.
//
// SECURITY: MANAGED (no host path) means nothing for the bind deny-list to
// deny and no host-root ceiling to escape; host_path runs that deny matrix
// instead (driver_mounts.go). Ownership is the image's job: a named volume
// mounted over an image directory inherits its uid/gid via Docker's copy-up,
// so every agent image pre-creates /home/agent/drive owned by agent — wardynd
// never chowns or runs a privileged helper.

const (
	// driveVolumeDriver: LOCAL, ALWAYS, never operator-configurable. SECURITY:
	// Docker's "local" driver can mount a CIFS/NFS share via
	// `--opt type=cifs --opt o=username=…,password=…`, and `docker volume
	// inspect` echoes those options verbatim to anybody who can reach the
	// daemon — so Wardyn NEVER creates a volume with driver options. A share
	// is mounted HOST-SIDE by the operator instead, as a host_path drive.
	driveVolumeDriver = "local"

	// labelDrive marks a volume as one user drive's storage; labelDriveHome
	// names the per-person home segment — what an operator filters on when
	// reclaiming. NEITHER IS A RUN LABEL: teardown reaps by wardyn.run-id, so
	// a drive volume carrying one would be deleted with whatever run mounted
	// it. labelDrive carries the DRIVE ROW's uuid, not the volume's own name,
	// since that's what tells two drives apart when home templates collide
	// onto one volume name.
	labelDrive     = "wardyn.drive"
	labelDriveHome = "wardyn.home"

	// labelDriveSubject fingerprints the PRINCIPAL a drive was allocated to
	// (sha256 of the sign-in subject, first 20 hex) — closes the gap
	// labelDrive can't, when a home template folds two principals onto one
	// home (`email_local` sharing a local part) so both carry the same drive
	// id. SECURITY: a digest, never the claim — `docker volume inspect`
	// echoes labels verbatim, so the subject must not be written here raw.
	labelDriveSubject = "wardyn.subject"
)

// ensureDriveVolume makes sure the named volume backing a MANAGED
// (docker_volume) user drive exists, touching nothing when it already does.
//
// INSPECT-THEN-CREATE: an unconditional VolumeCreate against an EXISTING
// volume silently keeps its labels/driver and reports success, so it couldn't
// tell "just allocated" from "re-affirmed". AND CREATE-THEN-VERIFY: that same
// silent success is a RACE between colliding first runs, so
// driveVolumeAdoptable also runs on CREATE's returned Volume, refusing the
// loser instead of letting it silently adopt.
//
// NOT rolled back by CreateSandbox's rollback: a drive volume is the
// member's persistent storage, not a per-run object — deleting it on a
// failed rollback would be a data-loss path a transient image pull failure
// could trigger.
func ensureDriveVolume(ctx context.Context, cli dockerAPI, drive *types.DriveMount) error {
	name := drive.ObjectName
	if name == "" {
		// SECURITY fail closed: an empty name would make VolumeCreate
		// allocate an ANONYMOUS volume, findable by no reclaim command.
		return fmt.Errorf("docker: user drive has no object name (backend %s)", drive.Backend)
	}
	if res, err := cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); err == nil {
		return driveVolumeAdoptable(res.Volume, name, drive)
	} else if !isNotFound(err) {
		// Fail closed: a daemon that can't answer whether the volume exists
		// must not have one created over whatever it couldn't read.
		return fmt.Errorf("docker: inspect user drive volume %q: %w", name, err)
	}
	// Omitted, not blank: adoption checks read an absent label as "predates it".
	labels := map[string]string{
		labelManaged:   "true",
		labelDrive:     drive.DriveID.String(),
		labelDriveHome: drive.HomeName,
	}
	if drive.SubjectHash != "" {
		labels[labelDriveSubject] = drive.SubjectHash
	}
	created, err := cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   name,
		Driver: driveVolumeDriver,
		// NO DriverOpts. Ever. See driveVolumeDriver.
		Labels: labels,
	})
	if err != nil {
		return fmt.Errorf("docker: create user drive volume %q: %w", name, err)
	}
	// Same predicate on both arms, or the first-run race above stays open.
	return driveVolumeAdoptable(created.Volume, name, drive)
}

// driveVolumeAdoptable reports whether v — found by inspect or handed back by
// a create that lost a race — may be mounted as drive's storage. nil means
// yes.
//
// SECURITY, THREE REFUSALS, each of which hands a member somebody else's
// bytes if missing:
//  1. Not Wardyn's shape: a hand-precreated volume could carry CIFS/NFS
//     credentials via DriverOpts; adopting on a name match would hand the
//     drive that share. Only the local driver with no options is adopted.
//  2. Another drive's id: colliding home templates can resolve two drives to
//     one volume name; without this the second would silently adopt the
//     first's storage.
//  3. Another principal's subject: case #2 can't catch this, since the id
//     still matches when one `email_local` home serves two people sharing a
//     local part. Refused here as the last layer.
//
// PRESENT-AND-DIFFERENT ONLY: a volume with no wardyn.drive or wardyn.subject
// label is still adopted — restoring one by hand is a documented operator
// gesture, and refusing a label-less volume would turn that into an outage.
func driveVolumeAdoptable(v volume.Volume, name string, drive *types.DriveMount) error {
	if v.Driver != driveVolumeDriver || len(v.Options) != 0 {
		return fmt.Errorf("docker: volume %q already exists with driver %q and %d driver option(s), which is not a Wardyn-managed drive "+
			"(a managed drive is always driver %q with no options — a precreated share volume must never be adopted as one); "+
			"rename or remove it, or point this drive at a host_path backend",
			name, v.Driver, len(v.Options), driveVolumeDriver)
	}
	if got := v.Labels[labelDrive]; got != "" && got != drive.DriveID.String() {
		return fmt.Errorf("docker: volume %q is labelled %s=%s and this drive is %s — two drives resolved to one object name, "+
			"and adopting it would hand this member another drive's storage; give one of the drives a home_override for this "+
			"principal, or remove the stale volume",
			name, labelDrive, got, drive.DriveID)
	}
	// SECURITY: the subject digest is NOT reproduced in the message — it
	// fingerprints a person, and an error string is where an unreversable
	// label would start getting copied around.
	if got := v.Labels[labelDriveSubject]; got != "" && got != drive.SubjectHash {
		return fmt.Errorf("docker: volume %q was allocated to a DIFFERENT principal (it carries another %s label) and this drive "+
			"resolved the same object name %q for this one — adopting it would hand this member another person's storage; "+
			"give this drive a home template that names one directory per person (%s), or set a home_override for this principal",
			name, labelDriveSubject, drive.HomeName, types.HomeTemplateHash)
	}
	return nil
}

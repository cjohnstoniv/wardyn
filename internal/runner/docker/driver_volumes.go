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

// Docker named volumes for MANAGED user drives (migration 0054) — the only
// place in this package that creates a volume, and deliberately the only
// shape of volume Wardyn will ever create.
//
// A docker_volume drive is MANAGED (no host path), which is a security
// property: nothing for the bind deny-list to deny, no symlink to re-point
// between validate and create, and no WARDYN_USER_DRIVE_HOST_ROOTS ceiling
// to be outside of. The host_path backend is the one that binds and runs
// the full deny matrix (driver_mounts.go).
//
// Ownership is the image's job, not wardynd's: a fresh named volume mounted
// over an existing image directory inherits that directory's contents AND
// its uid/gid (Docker's copy-up), so every agent image pre-creates
// /home/agent/drive owned by agent (uid 1000). wardynd never chowns
// anything, never runs a privileged helper, and never needs to know the uid.

const (
	// driveVolumeDriver is the volume driver every managed drive is created
	// with. LOCAL, ALWAYS, and never operator-configurable.
	//
	// This is the credential guardrail, stated as code: Docker's own
	// "local" driver can mount a CIFS/NFS share directly when handed
	// `--opt type=cifs --opt o=username=…,password=…`, and those options are
	// echoed verbatim by `docker volume inspect` to anybody who can reach the
	// daemon. Wardyn NEVER creates a volume with driver options for exactly
	// that reason. A share is mounted HOST-SIDE by the operator and reaches
	// Wardyn as a host_path drive, whose credential Wardyn never holds.
	driveVolumeDriver = "local"

	// labelDrive marks a volume as the storage of one Wardyn user drive, and
	// labelDriveHome names the per-person home segment inside it. Together
	// they are what an operator filters on when reclaiming.
	//
	// NEITHER IS A RUN LABEL, and that is load-bearing: teardown reaps by
	// wardyn.run-id (labelRun), so a drive volume carrying one would be
	// deleted with the run that happened to mount it, taking the member's
	// persistent storage with it.
	//
	// labelDrive carries the DRIVE ROW's uuid, deliberately NOT the volume's
	// own name (which the volume already answers to). The id is the
	// discriminator a reclaim or offboarding sweep actually asks for —
	// "every object THIS drive allocated" — and the one value that can tell
	// two managed drives apart when their home templates collide onto a
	// single volume name.
	labelDrive     = "wardyn.drive"
	labelDriveHome = "wardyn.home"

	// labelDriveSubject fingerprints the PRINCIPAL the object was allocated
	// to (sha256 of the sign-in subject, first 20 hex).
	//
	// It closes the one gap labelDrive cannot: a volume name is per-HOME, and
	// a home template can fold two principals onto one home (`email_local`
	// over two addresses sharing a local part), so both allocations carry
	// the SAME drive id and the labelDrive check below would MATCH and
	// adopt. The write boundary and the resolver both already refuse that
	// pair; this is the third layer, at the object itself.
	//
	// A digest, never the claim: `docker volume inspect` echoes labels
	// verbatim to anybody who can reach the daemon, so the subject —
	// routinely an email address — must not be written here.
	labelDriveSubject = "wardyn.subject"
)

// ensureDriveVolume makes sure the named volume backing a MANAGED
// (docker_volume) user drive exists, and returns without touching the
// daemon's state when it already does.
//
// INSPECT-THEN-CREATE rather than create-unconditionally: a VolumeCreate
// against an EXISTING volume silently keeps its labels/driver and reports
// success, so an unconditional create couldn't tell "just allocated" from
// "re-affirmed storage written to for months".
//
// AND THEN CREATE-THEN-VERIFY, because that same silent success is a RACE:
// two colliding drives whose first runs start together both see the 404 and
// both create, and the loser's create hands back the winner's volume
// unchanged. Every adoption rule (driveVolumeAdoptable, one predicate for
// both arms) therefore also runs on the CREATE's returned Volume, so the
// second drive is refused instead of silently adopting.
//
// NOT rolled back by CreateSandbox's rollback: a drive volume is the
// member's persistent storage, not a per-run object, so deleting it on a
// failed sandbox's rollback would be a data-loss path a transient image
// pull failure could trigger.
//
// A drive is docker_volume BY CONSTRUCTION here: only driveMount's
// DriveBackendDockerVolume arm calls this, so there is no nil/backend guard
// to fall through.
func ensureDriveVolume(ctx context.Context, cli dockerAPI, drive *types.DriveMount) error {
	name := drive.ObjectName
	if name == "" {
		// Fail closed: an empty object name would make VolumeCreate allocate
		// an ANONYMOUS volume, findable by no reclaim command.
		return fmt.Errorf("docker: user drive has no object name (backend %s)", drive.Backend)
	}
	if res, err := cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); err == nil {
		return driveVolumeAdoptable(res.Volume, name, drive)
	} else if !isNotFound(err) {
		// Fail-closed: a daemon that cannot answer whether the volume exists
		// must not have one created over the top of whatever it couldn't read.
		return fmt.Errorf("docker: inspect user drive volume %q: %w", name, err)
	}
	// An empty label value is OMITTED rather than written blank: the
	// adoption checks read an absent label as "predates the label", so
	// writing "" would mint a value that reads like a fact but means nothing.
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
	// The same driveVolumeAdoptable predicate answers both arms: an adoption
	// rule that held on the inspect path and not on the create path would be
	// a rule with a documented window (see the FIRST-RUN RACE above).
	return driveVolumeAdoptable(created.Volume, name, drive)
}

// driveVolumeAdoptable reports whether v — a volume that already answers to
// name, found either by the inspect probe or handed back by a create that lost
// a race — may be mounted as drive's storage. nil means yes.
//
// THREE REFUSALS, each of which hands a member somebody else's bytes if it is
// missing:
//
//  1. Not Wardyn's shape. The volume was not necessarily created by this
//     driver: an operator can precreate one by hand, and `docker volume create
//     --opt type=cifs --opt o=username=…,password=…` is the documented way to do
//     it. Mounting that into a member's sandbox because the names happened to
//     match would hand the drive whatever share those options point at, on a
//     credential Wardyn never chose and cannot see — the exact outcome
//     driveVolumeDriver's no-DriverOpts rule exists to prevent, arrived at
//     through the back door. So adopt only the local driver with no options.
//
//  2. Another drive's id. Two managed drives whose home templates collide
//     resolve to a single volume name (DriveObjectName is per-principal, not
//     per-drive), and without this the second drive would silently adopt the
//     first drive's storage.
//
//  3. Another principal's subject. The case #2 cannot see, because the id
//     matches: ONE drive templated on `email_local` derives one home for two
//     people whose addresses share a local part. Refused here, at the object,
//     after the write boundary and the resolver have both already refused the
//     row that produces it.
//
// PRESENT-AND-DIFFERENT ONLY, for both labels. A volume with NO wardyn.drive or
// no wardyn.subject label is still adopted, deliberately: restoring one by hand
// is a documented operator gesture (docs/OPERATIONS.md "User drives on
// Docker"), the labels are a convenience for `docker volume ls --filter`, and
// every volume created before a label existed has none. Refusing a label-less
// volume would turn a restore-from-backup into an outage.
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
	// The subject digest is NOT reproduced in the message: it is the fingerprint
	// of a person, and an error string is the one place a label an operator
	// cannot reverse would start being copied around. What an admin needs is
	// which drive and which directory to fix, and the remedy is the same one.
	if got := v.Labels[labelDriveSubject]; got != "" && got != drive.SubjectHash {
		return fmt.Errorf("docker: volume %q was allocated to a DIFFERENT principal (it carries another %s label) and this drive "+
			"resolved the same object name %q for this one — adopting it would hand this member another person's storage; "+
			"give this drive a home template that names one directory per person (%s), or set a home_override for this principal",
			name, labelDriveSubject, drive.HomeName, types.HomeTemplateHash)
	}
	return nil
}

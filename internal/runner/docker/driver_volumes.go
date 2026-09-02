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
	// labelDrive carries the DRIVE ROW's uuid (types.DriveMount.DriveID), the
	// value DESIGN §3.2 specifies (`wardyn.drive=<id>`) — deliberately NOT the
	// volume's own name, which the volume already answers to and which a label
	// would only restate. The id is the discriminator that answers the question
	// a reclaim or offboarding sweep actually asks — "every object THIS drive
	// allocated" — across principals whose object names have nothing in common;
	// and it is the one value that can tell two managed drives apart when their
	// home templates collide onto a single volume name, which is exactly the
	// case the inspect-hit arm below refuses.
	labelDrive     = "wardyn.drive"
	labelDriveHome = "wardyn.home"

	// labelDriveSubject fingerprints the PRINCIPAL the object was allocated to
	// (types.DriveSubjectHash — sha256 of the sign-in subject, first 20 hex).
	//
	// It closes the one gap labelDrive cannot: a volume name is per-HOME, and a
	// home template can fold two principals onto one home (`email_local` over
	// two addresses that share a local part). Both allocations then carry the
	// SAME drive id, so the labelDrive check below MATCHES and adopts — one
	// person handed the other's storage, with write access whenever the
	// allocation is writable. The write boundary refuses that pair outright
	// (types.ValidateUserDrive) and the resolver refuses it again for a row an
	// older binary wrote; this is the third layer, at the object itself.
	//
	// A DIGEST, NEVER THE CLAIM. `docker volume inspect` echoes labels verbatim
	// to anybody who can reach the daemon, so the subject — routinely an email
	// address — must not be written here. The digest answers the one question
	// the driver asks ("was this object allocated to THIS principal") and no
	// other.
	labelDriveSubject = "wardyn.subject"
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
// AND THEN CREATE-THEN-VERIFY, because that same silent success is a RACE and
// not merely an ergonomics problem: two colliding drives whose first runs start
// together both see the 404 and both create, and the loser's create hands back
// the winner's volume — driver, options and labels included — with nothing of
// its own applied. Every adoption rule therefore runs on the CREATE's returned
// Volume too (driveVolumeAdoptable, one predicate for both arms), so the second
// drive is refused instead of adopting once, silently, on the one path the
// inspect never covered.
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
		return driveVolumeAdoptable(res.Volume, name, drive)
	} else if !isNotFound(err) {
		// Any error other than "no such volume" is fail-closed: a daemon that
		// cannot answer whether the volume exists must not have one created
		// over the top of whatever it could not read.
		return fmt.Errorf("docker: inspect user drive volume %q: %w", name, err)
	}
	created, err := cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   name,
		Driver: driveVolumeDriver,
		// NO DriverOpts. Ever. See driveVolumeDriver.
		Labels: driveVolumeLabels(drive),
	})
	if err != nil {
		return fmt.Errorf("docker: create user drive volume %q: %w", name, err)
	}
	// CREATE-THEN-VERIFY, and this is the FIRST-RUN RACE rather than a
	// belt-and-braces re-read. VolumeCreate against a name that already resolves
	// SUCCEEDS and hands back the EXISTING volume — its driver, its options, its
	// labels — with none of the options above applied. So two colliding drives
	// starting their first run at the same moment both see the 404 above and
	// both create; the second one's create quietly returns the first one's
	// volume, and every check that would have refused it lives in the inspect
	// arm it never reached.
	//
	// The SAME predicate answers both arms, which is the point of extracting it:
	// an adoption rule that held on the inspect path and not on the create path
	// would be a rule with a documented window.
	return driveVolumeAdoptable(created.Volume, name, drive)
}

// driveVolumeLabels is the label set every managed drive volume carries, in one
// place so the volume the driver CREATES and the volume it will later agree to
// adopt are described by the same three keys.
//
// A label whose value is empty is OMITTED rather than written blank: the
// adoption checks read an absent label as "this volume predates the label /
// was restored by hand" and an empty one as the same thing, so writing "" would
// mint a value that means nothing and reads in `docker volume inspect` like a
// fact.
func driveVolumeLabels(drive *types.DriveMount) map[string]string {
	labels := map[string]string{
		labelManaged:   "true",
		labelDrive:     drive.DriveID.String(),
		labelDriveHome: drive.HomeName,
	}
	if drive.SubjectHash != "" {
		labels[labelDriveSubject] = drive.SubjectHash
	}
	return labels
}

// driveVolumeAdoptable reports whether v — a volume that already answers to
// name, found either by the inspect probe or handed back by a create that lost
// a race — may be mounted as drive's storage. nil means yes.
//
// THREE REFUSALS, each of which hands a member somebody else's bytes if it is
// missing:
//
//  1. NOT WARDYN'S SHAPE. The volume was not necessarily created by this
//     driver: an operator can precreate one by hand, and `docker volume create
//     --opt type=cifs --opt o=username=…,password=…` is the documented way to do
//     it. Mounting that into a member's sandbox because the names happened to
//     match would hand the drive whatever share those options point at, on a
//     credential Wardyn never chose and cannot see — the exact outcome
//     driveVolumeDriver's no-DriverOpts rule exists to prevent, arrived at
//     through the back door. So adopt only the local driver with no options.
//
//  2. ANOTHER DRIVE'S ID. Two managed drives whose home templates collide
//     resolve to a single volume name (DriveObjectName is per-principal, not
//     per-drive), and without this the second drive would silently adopt the
//     first drive's storage.
//
//  3. ANOTHER PRINCIPAL'S SUBJECT. The case #2 cannot see, because the id
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

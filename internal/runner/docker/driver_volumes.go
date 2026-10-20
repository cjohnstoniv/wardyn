// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Docker named volumes for MANAGED user drives — the only place in this
// package that creates a volume, and the only shape it ever creates.
//
// SECURITY: MANAGED (no host path) means nothing for the bind deny-list to
// deny and no host-root ceiling to escape; host_path runs that deny matrix
// instead (driver_mounts.go). Ownership is the image's job: a named volume
// mounted over an image directory inherits its uid/gid via Docker's copy-up,
// so every agent image pre-creates /home/agent/drive owned by agent. A
// person's volume at a derived path the image has no directory for is seeded
// through that same directory first (seedDriveVolume). A TEAM volume is shared
// by a group, so nothing from a run's image is ever copied into it: its mounts
// carry NoCopy, and a writable allocation gives an empty team volume's top
// directory to the agent's uid through the daemon's archive API instead
// (ownEmptyTeamVolume). wardynd runs no privileged helper.

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

// driveSeedPrefix starts the name of the container seedDriveVolume creates;
// driveSeedName completes it with the run, so concurrent runs on one volume
// never contend for a name, and teardown can remove one a crash left behind.
const driveSeedPrefix = "wardyn-drive-seed-"

func driveSeedName(runID uuid.UUID) string { return driveSeedPrefix + runID.String() }

// componentDriveSeed labels the seed container (wardynLabels), so the orphan
// sweep (sweepDriveSeeds) finds one a crash left behind.
const componentDriveSeed = "drive-seed"

// driveSeedRemoveTimeout bounds the seed's last removal, which runs even when
// the create's context has ended: a seed left behind holds the volume.
const driveSeedRemoveTimeout = 30 * time.Second

// seedDriveVolume gives a managed volume mounted below runner.DrivesRoot the
// ownership the legacy drive gets. Docker copies an image directory's owner and
// contents onto an EMPTY volume mounted over it, and every agent image
// pre-creates runner.DriveTarget owned by the agent, but no image has a
// directory at a derived path, so a fresh volume there would start root-owned
// and unwritable by the agent.
//
// So a person's volume is mounted once at runner.DriveTarget in a container
// that is created and removed, never started, with no network: the copy
// happens at create, nothing in the image runs, nothing is chowned, and a
// volume that already holds data is left as it is (Docker copies into an empty
// volume only). One run seeds its drives one at a time under its own name, so
// two runs on one volume never collide; two copies into one empty volume write
// the same directory.
//
// A team volume is never seeded from the image: the run's image is one
// member's choice, and whatever it holds at runner.DriveTarget would reach
// every member. A read-only allocation needs no seed at all. A writable one
// mounts the volume with NoCopy at teamDriveSeedTarget in the same
// never-started container and gives it to the agent with ownEmptyTeamVolume.
func (d *Driver) seedDriveVolume(ctx context.Context, runID uuid.UUID, image string, drive *types.DriveMount) error {
	if drive.Team && drive.ReadOnly {
		return nil
	}
	seedMount := mount.Mount{Type: mount.TypeVolume, Source: drive.ObjectName, Target: runner.DriveTarget}
	if drive.Team {
		seedMount.Target, seedMount.VolumeOptions = teamDriveSeedTarget, teamDriveVolumeOptions(drive)
	}
	name := driveSeedName(runID)
	remove := func(ctx context.Context) error {
		if _, err := d.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			return fmt.Errorf("docker: seed user drive volume %q: remove: %w", drive.ObjectName, err)
		}
		return nil
	}
	if err := remove(ctx); err != nil {
		return err
	}
	// A runner's own label lets its orphan sweep tell its seeds from another's.
	extra := map[string]string{labelDrive: drive.DriveID.String()}
	if d.cfg.RunnerID != "" {
		extra[runner.LabelRunnerID] = d.cfg.RunnerID
	}
	_, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{Image: image, Cmd: []string{"true"}, NetworkDisabled: true,
			Labels: d.labels(runID, componentDriveSeed, extra)},
		HostConfig: &container.HostConfig{
			NetworkMode: "none",
			Mounts:      []mount.Mount{seedMount},
		},
	})
	if err == nil && drive.Team {
		err = d.ownEmptyTeamVolume(ctx, name)
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), driveSeedRemoveTimeout)
	defer cancel()
	if rerr := remove(rctx); err == nil {
		err = rerr
	}
	if err != nil {
		return fmt.Errorf("docker: seed user drive volume %q: %w", drive.ObjectName, err)
	}
	return nil
}

// teamDriveSeedTarget is where a writable team volume mounts in its seed
// container: a path of Wardyn's own, so neither the copy (NoCopy) nor the
// ownership (ownEmptyTeamVolume) depends on what the image has at
// runner.DriveTarget. teamDriveOwner is the uid and gid every Wardyn agent
// image runs its agent as (the image contract; Kubernetes runs it as 1000 too).
const (
	teamDriveSeedTarget = "/wardyn-drive-seed"
	teamDriveOwner      = 1000
)

// teamDriveVolumeOptions is the VolumeOptions for every mount of a team volume:
// NoCopy, so Docker never copies an image directory into it. nil for any other
// drive, which keeps Docker's default copy into an empty volume.
func teamDriveVolumeOptions(drive *types.DriveMount) *mount.VolumeOptions {
	if !drive.Team {
		return nil
	}
	return &mount.VolumeOptions{NoCopy: true}
}

// ownEmptyTeamVolume gives an EMPTY team volume's top directory to the agent
// (teamDriveOwner, mode 0755), which a NoCopy mount leaves root-owned. It runs
// on the never-started seed container: the daemon reads the volume through the
// archive API, and, only when the volume holds nothing, extracts one directory
// entry for the mount point, whose owner and mode it applies to the volume's
// existing top directory. Nothing in the image runs, nothing from it is copied,
// and a volume that already holds anything is left as it is, as Docker's own
// copy into an empty volume would. A seed path that is not a directory (an
// image with a link there) is refused rather than left unwritable.
func (d *Driver) ownEmptyTeamVolume(ctx context.Context, seed string) error {
	res, err := d.cli.CopyFromContainer(ctx, seed, client.CopyFromContainerOptions{SourcePath: teamDriveSeedTarget})
	if err != nil {
		return fmt.Errorf("read the team volume: %w", err)
	}
	tr := tar.NewReader(res.Content)
	top, err := tr.Next()
	if err == nil && top.Typeflag != tar.TypeDir {
		err = fmt.Errorf("%s is not a directory in this image", teamDriveSeedTarget)
	}
	if err == nil {
		_, err = tr.Next()
		if err == nil {
			_ = res.Content.Close()
			return nil // holds something: left as it is
		}
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	_ = res.Content.Close()
	if err != nil {
		return fmt.Errorf("read the team volume: %w", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: strings.TrimPrefix(teamDriveSeedTarget, "/") + "/", Typeflag: tar.TypeDir, Mode: 0o755,
		Uid: teamDriveOwner, Gid: teamDriveOwner, ModTime: time.Now(),
	}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// CopyUIDGID false: the header's owner is applied as written.
	if _, err := d.cli.CopyToContainer(ctx, seed, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf}); err != nil {
		return fmt.Errorf("own the team volume: %w", err)
	}
	return nil
}

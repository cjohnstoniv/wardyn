// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reclaimFake is a daemon holding exactly the volume ensureDriveVolume would
// have created for drive, plus the driver under test. Built from the drive
// rather than written out, so a case that mutates one label is unambiguously
// about that label.
func reclaimFake(t *testing.T, drive *types.DriveMount, mut func(*client.VolumeCreateOptions)) (*fakeDocker, *Driver) {
	t.Helper()
	f := newFakeDocker()
	opts := client.VolumeCreateOptions{
		Name: drive.ObjectName, Driver: driveVolumeDriver,
		Labels: map[string]string{
			labelManaged:      "true",
			labelDrive:        drive.DriveID.String(),
			labelDriveHome:    drive.HomeName,
			labelDriveSubject: drive.SubjectHash,
		},
	}
	if mut != nil {
		mut(&opts)
	}
	f.volumes[opts.Name] = opts
	return f, newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
}

// TestReclaimDrive_DeletesTheDrivesOwnVolume is the happy path, and the two
// assertions beside the outcome are the ones that matter: the remove named the
// volume the drive resolved to, and FORCE was never set — a forced remove
// would pull the storage out from under a live agent mid-write.
func TestReclaimDrive_DeletesTheDrivesOwnVolume(t *testing.T) {
	drive := dockerVolumeDrive()
	f, d := reclaimFake(t, drive, nil)

	got, err := d.ReclaimDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ReclaimDrive: %v", err)
	}
	if got != runner.DriveReclaimDeleted {
		t.Errorf("outcome = %q, want %q", got, runner.DriveReclaimDeleted)
	}
	if len(f.volumeRemoves) != 1 || f.volumeRemoves[0] != drive.ObjectName {
		t.Errorf("VolumeRemove calls = %v, want exactly [%s]", f.volumeRemoves, drive.ObjectName)
	}
	if f.lastVolumeRemoveForce {
		t.Error("the remove set Force: Docker's own refusal to remove a volume a container still uses IS " +
			"the in-use check, and overriding it deletes a running agent's storage mid-write")
	}
	if _, still := f.volumes[drive.ObjectName]; still {
		t.Error("the volume survived a reported delete")
	}
}

// TestReclaimDrive_AlreadyAbsentIsNotAnError: an operator's own
// `docker volume rm`, or a prior half-finished reclaim, reaches the same end
// state. It is never reported as `deleted` — an audit row claiming a person's
// storage was destroyed when it was already gone is the one row an operator
// must be able to trust.
func TestReclaimDrive_AlreadyAbsentIsNotAnError(t *testing.T) {
	drive := dockerVolumeDrive()
	f := newFakeDocker()
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})

	got, err := d.ReclaimDrive(context.Background(), *drive)
	if err != nil {
		t.Fatalf("ReclaimDrive against a missing volume: %v", err)
	}
	if got != runner.DriveReclaimAlreadyAbsent {
		t.Errorf("outcome = %q, want %q", got, runner.DriveReclaimAlreadyAbsent)
	}
	if len(f.volumeRemoves) != 0 {
		t.Errorf("a delete was issued for a volume the inspect did not find: %v", f.volumeRemoves)
	}
}

// TestReclaimDrive_RefusesAVolumeThatIsNotThisDrivesObject is the identity
// rail. The same predicate that refuses to MOUNT a foreign volume refuses to
// DELETE one, because the two mistakes are the same mistake: a wrong adoption
// shows one member another member's files, and a wrong reclaim deletes them.
func TestReclaimDrive_RefusesAVolumeThatIsNotThisDrivesObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*client.VolumeCreateOptions)
	}{
		{
			"another drive's object under a colliding minted name",
			func(o *client.VolumeCreateOptions) { o.Labels[labelDrive] = "99999999-8888-7777-6666-555555555555" },
		},
		{
			"another principal's object under a home template that folds two people onto one",
			func(o *client.VolumeCreateOptions) {
				o.Labels[labelDriveSubject] = types.DriveSubjectHash("mallory@corp.example")
			},
		},
		{
			"an operator's hand-made volume carrying driver options",
			func(o *client.VolumeCreateOptions) { o.DriverOpts = map[string]string{"type": "cifs"} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive := dockerVolumeDrive()
			f, d := reclaimFake(t, drive, tc.mut)

			_, err := d.ReclaimDrive(context.Background(), *drive)
			if !errors.Is(err, runner.ErrDriveNotReclaimable) {
				t.Fatalf("err = %v, want ErrDriveNotReclaimable", err)
			}
			if len(f.volumeRemoves) != 0 {
				t.Errorf("the driver issued a delete against a volume it had already judged foreign: %v", f.volumeRemoves)
			}
			if _, still := f.volumes[drive.ObjectName]; !still {
				t.Error("the foreign volume is gone")
			}
		})
	}
}

// TestReclaimDrive_RefusesWhileAContainerStillMountsIt: the daemon's own 409 is
// the "a run still holds this" check, mapped to the sentinel the API turns into
// a 409 rather than overridden with Force.
func TestReclaimDrive_RefusesWhileAContainerStillMountsIt(t *testing.T) {
	drive := dockerVolumeDrive()
	f, d := reclaimFake(t, drive, nil)
	f.volumeInUse = drive.ObjectName

	_, err := d.ReclaimDrive(context.Background(), *drive)
	if !errors.Is(err, runner.ErrDriveInUse) {
		t.Fatalf("err = %v, want ErrDriveInUse", err)
	}
	if _, still := f.volumes[drive.ObjectName]; !still {
		t.Error("the in-use volume was removed anyway")
	}
}

// TestReclaimDrive_RefusesABackendItDidNotAllocate is the no-rm-rf rail at the
// substrate: a host_path drive's object is a DIRECTORY ON THE OPERATOR'S OWN
// FILESYSTEM. Wardyn never created it and never deletes it.
func TestReclaimDrive_RefusesABackendItDidNotAllocate(t *testing.T) {
	f := newFakeDocker()
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	drive := dockerVolumeDrive()
	drive.Backend, drive.ObjectName, drive.HostRoot = types.DriveBackendHostPath, "/srv/shares/alice", "/srv/shares"

	if _, err := d.ReclaimDrive(context.Background(), *drive); !errors.Is(err, runner.ErrDriveNotReclaimable) {
		t.Fatalf("err = %v, want ErrDriveNotReclaimable", err)
	}
	if len(f.volumeRemoves) != 0 {
		t.Errorf("a host_path drive reached VolumeRemove: %v", f.volumeRemoves)
	}
}

// TestReclaimDrive_RefusesAnEmptyObjectName: an empty name on the create path
// allocates an anonymous volume; here it would be a delete against whatever the
// daemon makes of "".
func TestReclaimDrive_RefusesAnEmptyObjectName(t *testing.T) {
	f := newFakeDocker()
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	drive := dockerVolumeDrive()
	drive.ObjectName = ""

	if _, err := d.ReclaimDrive(context.Background(), *drive); !errors.Is(err, runner.ErrDriveNotReclaimable) {
		t.Fatalf("err = %v, want ErrDriveNotReclaimable", err)
	}
	if len(f.volumeRemoves) != 0 {
		t.Errorf("an unnamed object reached VolumeRemove: %v", f.volumeRemoves)
	}
}

// TestReclaimDrive_FailsClosedWhenTheDaemonCannotSayWhatTheVolumeIs: a daemon
// that cannot answer the identity question must not have the volume deleted on
// the strength of its name alone.
func TestReclaimDrive_FailsClosedWhenTheDaemonCannotSayWhatTheVolumeIs(t *testing.T) {
	drive := dockerVolumeDrive()
	f, d := reclaimFake(t, drive, nil)
	f.failVolumeInspect = true

	_, err := d.ReclaimDrive(context.Background(), *drive)
	if err == nil {
		t.Fatal("ReclaimDrive succeeded although the inspect failed")
	}
	if errors.Is(err, runner.ErrDriveNotReclaimable) || errors.Is(err, runner.ErrDriveInUse) {
		t.Errorf("a transport failure was reported as a decided refusal (%v) — the operator would read a "+
			"409 and stop retrying a reclaim the substrate never judged", err)
	}
	if len(f.volumeRemoves) != 0 {
		t.Errorf("the driver deleted on the strength of the name alone: %v", f.volumeRemoves)
	}
}

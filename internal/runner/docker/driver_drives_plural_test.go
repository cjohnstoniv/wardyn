// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secondVolumeDrive is a managed drive beside the legacy one, at its derived path.
func secondVolumeDrive() types.DriveMount {
	d := *dockerVolumeDrive()
	d.DriveID, d.ObjectName, d.Target, d.ReadOnly = uuid.New(), "wardyn-drive-team-alice", runner.DrivesRoot+"/team", false
	return d
}

func createWithDrives(t *testing.T, f *fakeDocker, drives ...types.DriveMount) ([]mount.Mount, error) {
	t.Helper()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
	spec := testSpec()
	spec.Drives = drives
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		return nil, err
	}
	return f.containers[agentContainerName(spec.RunID)].host.Mounts, nil
}

// TestCreateSandbox_MountsEveryDrive: two drives, each at its own path, each a
// volume of its own. The one below DrivesRoot is first seeded through the
// image's own drive directory by a container created and removed, never
// started; the legacy one needs no seed.
func TestCreateSandbox_MountsEveryDrive(t *testing.T) {
	f := newFakeDocker()
	second := secondVolumeDrive()
	mounts, err := createWithDrives(t, f, *dockerVolumeDrive(), second)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []types.DriveMount{*dockerVolumeDrive(), second} {
		m := findMount(mounts, d.Target)
		if m == nil || m.Type != mount.TypeVolume || m.Source != d.ObjectName || m.ReadOnly != d.ReadOnly {
			t.Errorf("drive at %s mounted as %+v, want volume %s ro=%v", d.Target, m, d.ObjectName, d.ReadOnly)
		}
	}
	seed := f.containers[driveSeedPrefix+second.ObjectName]
	if seed == nil || !seed.removed || seed.state != nil && seed.state.Running {
		t.Fatalf("seed container = %+v, want one created and removed, never started", seed)
	}
	if m := findMount(seed.host.Mounts, runner.DriveTarget); m == nil || m.Source != second.ObjectName || m.ReadOnly {
		t.Errorf("seed mounts = %+v, want %s writable at %s", seed.host.Mounts, second.ObjectName, runner.DriveTarget)
	}
	if f.containers[driveSeedPrefix+dockerVolumeDrive().ObjectName] != nil {
		t.Error("the legacy drive was seeded; the image's copy-up serves it directly")
	}
}

// TestCreateSandbox_DriveSeedFailureRefuses: a drive that could not be seeded
// refuses the sandbox rather than starting it with an unwritable drive.
func TestCreateSandbox_DriveSeedFailureRefuses(t *testing.T) {
	f := newFakeDocker()
	f.failCreateContainer = driveSeedPrefix
	if _, err := createWithDrives(t, f, secondVolumeDrive()); err == nil || !strings.Contains(err.Error(), "seed user drive volume") {
		t.Fatalf("CreateSandbox = %v, want the seed refusal", err)
	}
}

// TestCreateSandbox_DrivesAtOneOrForeignTargetRefused: two drives at one path
// would hide one; a managed drive under the shared root, or below its own
// folder, is not a path the server derives.
func TestCreateSandbox_DrivesAtOneOrForeignTargetRefused(t *testing.T) {
	twin := secondVolumeDrive()
	twin.Target = runner.DriveTarget
	cases := map[string][]types.DriveMount{
		"two at one target":      {*dockerVolumeDrive(), twin},
		"managed under shared":   {withTarget(secondVolumeDrive(), runner.SharedRoot+"/team")},
		"below its own folder":   {withTarget(secondVolumeDrive(), runner.DrivesRoot+"/team/x")},
		"the drives root itself": {withTarget(secondVolumeDrive(), runner.DrivesRoot)},
	}
	for name, drives := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := createWithDrives(t, newFakeDocker(), drives...)
			if err == nil || !strings.Contains(err.Error(), errDriveTargetInvalid.Error()) {
				t.Fatalf("CreateSandbox = %v, want %v", err, errDriveTargetInvalid)
			}
		})
	}
}

func withTarget(d types.DriveMount, target string) types.DriveMount {
	d.Target = target
	return d
}

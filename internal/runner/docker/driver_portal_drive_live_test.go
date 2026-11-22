// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

// Real-daemon check of a portal's team folder: the volume is named by the
// (portal, drive) folder alone, so two runs of the portal mount the same bytes,
// and a read-only run can read them and cannot write. Skipped unless
// WARDYN_TEST_DOCKER=1.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestDriveVolume_TeamFolderSharedAcrossRunsReadOnlyRefusesWrite_Docker(t *testing.T) {
	l := newDriveLive(t)
	d := types.UserDrive{ID: uuid.New(), Backend: types.DriveBackendDockerVolume, Team: true}
	portal := uuid.New()
	home := types.DrivePortalHome(d, portal)
	name := types.DrivePortalObjectName(d, home)
	t.Cleanup(func() { _, _ = l.cli.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })

	mountOf := func(readOnly bool) *types.DriveMount {
		return &types.DriveMount{DriveID: d.ID, Backend: d.Backend, ObjectName: name, HomeName: home,
			Target: runner.SharedRoot + "/eng", ReadOnly: readOnly, Team: true}
	}
	writable, readOnly := mountOf(false), mountOf(true)

	if code, out := l.run(l.image, "1000", l.mounts(writable), "echo FIRST > "+writable.Target+"/marker"); code != 0 {
		t.Fatalf("the first run could not write the team folder: exit %d (%s)", code, out)
	}
	code, out := l.run(l.image, "1000", l.mounts(readOnly), "cat "+readOnly.Target+"/marker")
	if code != 0 || !strings.Contains(out, "FIRST") {
		t.Errorf("a second run did not read the first run's file: exit %d (%q)", code, out)
	}
	if code, out := l.run(l.image, "1000", l.mounts(readOnly), "echo SECOND > "+readOnly.Target+"/other"); code == 0 {
		t.Errorf("a read-only run wrote the team folder (%s)", out)
	}
	if got := l.volume(name); !strings.Contains(got, "marker=FIRST") || strings.Contains(got, "other") {
		t.Errorf("team folder after both runs = %q, want only the first run's marker", got)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// removeDeadlineDocker is fakeDocker that records, for each ContainerRemove, the time left on its context's
// deadline (-1 for none) in *left.
type removeDeadlineDocker struct {
	*fakeDocker
	left *[]time.Duration
}

func (f removeDeadlineDocker) ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	*f.left = append(*f.left, left)
	return f.fakeDocker.ContainerRemove(ctx, id, opts)
}

// TestDrivePin_RemovalsHaveTheirOwnDeadline: the hold's removal after a refused start and the removal for a
// create that failed each run on a deadline of their own, driveSeedRemoveTimeout at most, when the caller's context
// has none, and the hold is removed.
func TestDrivePin_RemovalsHaveTheirOwnDeadline(t *testing.T) {
	drive := dockerVolumeDrive()
	vol := volume.Volume{Name: drive.ObjectName, Labels: driveVolumeLabels(drive)}
	for name, tc := range map[string]struct {
		held    bool  // the judged volume is still there, so the hold is made and kept
		wantErr error // pinDriveVolumes' answer
	}{
		"the hold is removed after a refused start":    {wantErr: errDriveVolumeReplaced},
		"the hold is removed for a create that failed": {held: true},
	} {
		t.Run(name, func(t *testing.T) {
			var left []time.Duration
			f := newFakeDocker()
			if tc.held {
				f.volumes[drive.ObjectName] = client.VolumeCreateOptions{Name: drive.ObjectName, Driver: driveVolumeDriver, Labels: driveVolumeLabels(drive)}
			}
			d := newWithClient(removeDeadlineDocker{f, &left}, Config{ProxyImage: "wardyn-proxy:dev"})
			run := uuid.New()
			if err := d.pinDriveVolumes(context.Background(), run, []judgedVolume{{drive: drive, vol: vol}}); !errors.Is(err, tc.wantErr) {
				t.Fatalf("pinDriveVolumes = %v, want %v", err, tc.wantErr)
			}
			if tc.held {
				d.dropDrivePin(run)
			}
			if len(left) != 1 || left[0] <= 0 || left[0] > driveSeedRemoveTimeout {
				t.Fatalf("hold removals had %v left on their deadlines; want one within (0, %v]", left, driveSeedRemoveTimeout)
			}
			if c := f.containers[drivePinName(run)]; c == nil || !c.removed {
				t.Fatalf("the hold = %+v, want made and removed", c)
			}
		})
	}
}

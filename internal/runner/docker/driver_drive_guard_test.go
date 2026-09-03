//go:build docker

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestClassesDeclaresDrivesAndMountsThem pins the DECLARATION against the
// BEHAVIOUR: this substrate says it can bind a member's drive, and it does.
//
// The two halves are one fact and drift between them is a member-visible
// failure in whichever direction it drifts. The control plane admits or refuses
// a drive-carrying run by reading Classes().UserDrives, before dispatch and
// again at preflight (api.driveMountFor). So a driver that declares FALSE while
// it can mount refuses every allocation on a deployment that would have worked;
// one that declares TRUE while it cannot is the run that previews green, answers
// 201 and then fails at dispatch — the shape the capability gate exists to
// close.
//
// This test is the D3 half of that pair, flipped. Before D3 it asserted the
// opposite — refuses, and declares false — over the errDriveUnsupported stub
// this driver no longer has. D3 landed the mount (driveMount in
// driver_mounts.go, ensureDriveVolume in driver_volumes.go) and the declaration
// moved with it. What the file pins never changed: the two must agree.
//
// The MOUNT's own behaviour — naming, labels, adoption, idempotence, the
// fail-closed arms — is driver_volumes_test.go's; this asserts only that a
// mount happens at all, because that is what the declaration claims.
func TestClassesDeclaresDrivesAndMountsThem(t *testing.T) {
	ctx := context.Background()

	cs, err := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev"}).Classes(ctx)
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !cs.UserDrives {
		t.Fatal("Classes reports UserDrives=false while this driver mounts drives — " +
			"the control plane would refuse every allocation on this deployment")
	}

	// …and the claim is true. A declaration nothing exercises is the half of
	// the pair that rots.
	drive := dockerVolumeDrive()
	_, mounts, err := createWithDrive(t, drive, nil)
	if err != nil {
		t.Fatalf("CreateSandbox with a drive: %v", err)
	}
	if m := findMount(mounts, runner.DriveTarget); m == nil {
		t.Errorf("Classes declares UserDrives, but no mount landed at %s; mounts=%+v",
			runner.DriveTarget, mounts)
	}
}

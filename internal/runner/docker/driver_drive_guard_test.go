//go:build docker

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCreateSandbox_RejectsAUserDrive pins the fail-closed half of
// seedRequestDrive's refusal matrix on this substrate: a spec carrying a
// resolved user drive is refused BEFORE any Docker call, so a member who asked
// for storage never gets a run that silently lacks it. D3 replaces this test
// with the real mount's coverage.
func TestCreateSandbox_RejectsAUserDrive(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	spec := testSpec()
	spec.Drive = &types.DriveMount{
		Backend:    types.DriveBackendDockerVolume,
		ObjectName: "wardyn-drive-d-0123456789abcdef0123",
		HomeName:   "d-0123456789abcdef0123",
		Target:     runner.DriveTarget,
		SizeMiB:    10240,
	}

	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox: want an error refusing the drive, got nil")
	}
	if !errors.Is(err, errDriveUnsupported) {
		t.Errorf("err = %v, want errors.Is(err, errDriveUnsupported)", err)
	}
	if !strings.Contains(err.Error(), "does not mount drives yet") {
		t.Errorf("err = %v, want it to name the gap", err)
	}
	if got := f.containers[agentContainerName(spec.RunID)]; got != nil {
		t.Errorf("an agent container was created despite the refusal: %+v", got)
	}

	// THE DECLARATION MUST AGREE WITH THE BEHAVIOUR. The control plane refuses a
	// drive-carrying run by reading this flag, so a stub that refuses while the
	// flag says "yes" is the run that previews green and dies here — the exact
	// shape the gate closes. D3 deletes the refusal above and sets this true in
	// the SAME change; whichever half lands alone fails this.
	cs, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if cs.UserDrives {
		t.Error("Classes reports UserDrives=true while CreateSandbox still refuses every drive")
	}
}

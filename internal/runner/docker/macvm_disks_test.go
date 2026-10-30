// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

const testDiskObject = "wardyn-drive-0123456789abcdef0123456789abcdef-d-0123456789abcdef"

func testDiskDrive(target string) types.DriveMount {
	return types.DriveMount{DriveID: uuid.MustParse("1a2b3c4d-0000-4000-8000-000000000001"), Backend: types.DriveBackendMacDisk,
		ObjectName: testDiskObject, DriveName: "Notes", Target: target, SizeMiB: 1024}
}

func TestMacDisksAreAttachedInTheirOrder(t *testing.T) {
	r := newMacRig(t)
	disks, guest, err := r.d.macDisks([]types.DriveMount{testDiskDrive(runner.DriveTarget), testDiskDrive(runner.ScratchWorkPath)})
	if err != nil {
		t.Fatal(err)
	}
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	if len(disks) != 2 || disks[0] != (macvm.Disk{ID: "wardyn-disk0", Path: img, SizeBytes: 1 << 30}) || disks[1].ID != "wardyn-disk1" {
		t.Fatalf("disks = %+v", disks)
	}
	want := guestagent.Disk{ID: "wardyn-disk0", Label: "Wardyn-1a2b3c4d", Target: guestagent.MacAgentHome + "/drive"}
	if len(guest) != 2 || guest[0] != want || guest[1].Target != guestagent.MacAgentHome+"/work" {
		t.Fatalf("guest disks = %+v", guest)
	}
}

func TestMacDisksRefuseAHeldDiskInItsSentence(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	unlock, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, _, err = r.d.macDisks([]types.DriveMount{testDiskDrive(runner.DriveTarget)})
	if err == nil || err.Error() != runner.MacVMDiskLocked("Notes") {
		t.Fatalf("err = %v, want %q", err, runner.MacVMDiskLocked("Notes"))
	}
	if gone, err := r.d.ReclaimDrive(t.Context(), testDiskDrive(runner.DriveTarget)); gone != "" || !errors.Is(err, runner.ErrDriveInUse) {
		t.Fatalf("reclaim of a held disk = %q, %v", gone, err)
	}
}

func TestMacDisksRefusals(t *testing.T) {
	r := newMacRig(t)
	for name, mod := range map[string]func(*types.DriveMount){
		"a name Wardyn does not mint": func(d *types.DriveMount) { d.ObjectName = "../x" },
		"a target outside the drives": func(d *types.DriveMount) { d.Target = "/home/agent/other" },
		"no size":                     func(d *types.DriveMount) { d.SizeMiB = 0 },
	} {
		drive := testDiskDrive(runner.DriveTarget)
		mod(&drive)
		if _, _, err := r.d.macDisks([]types.DriveMount{drive}); err == nil {
			t.Errorf("%s: the disk was attached", name)
		}
	}
}

func TestReclaimMacDiskRemovesOnlyItsImage(t *testing.T) {
	r := newMacRig(t)
	drive := testDiskDrive(runner.DriveTarget)
	if out, err := r.d.ReclaimDrive(t.Context(), drive); out != runner.DriveReclaimAlreadyAbsent || err != nil {
		t.Fatalf("no image: %q %v", out, err)
	}
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	if err := macvm.MakeDiskImage(img, 1<<20); err != nil {
		t.Fatal(err)
	}
	if out, err := r.d.ReclaimDrive(t.Context(), drive); out != runner.DriveReclaimDeleted || err != nil {
		t.Fatalf("reclaim: %q %v", out, err)
	}
	if _, err := os.Stat(img); !os.IsNotExist(err) {
		t.Fatal("the image is still there")
	}
	drive.ObjectName = "../../x"
	if _, err := r.d.ReclaimDrive(t.Context(), drive); !errors.Is(err, runner.ErrDriveNotReclaimable) {
		t.Fatalf("a name outside the drives folder: %v", err)
	}
}

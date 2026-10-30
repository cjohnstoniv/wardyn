// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A person's disk drive, or a kept work folder, is a disk image on this Mac (types.DriveBackendMacDisk)
// that the run's VM attaches as a disk; every other drive is a shared folder (macShares).

// splitMacDisks is drives as the disk drives and the rest, each in its order.
func splitMacDisks(drives []types.DriveMount) (disks, rest []types.DriveMount) {
	for _, drive := range drives {
		if drive.Backend == types.DriveBackendMacDisk {
			disks = append(disks, drive)
		} else {
			rest = append(rest, drive)
		}
	}
	return disks, rest
}

// macDrives is a macOS VM run's folders and drives as the helper and the guest agent take them: its
// folders and share drives as shared folders (macShares), its disk drives as disks (macDiskDrives).
func (d *Driver) macDrives(ctx context.Context, spec runner.SandboxSpec) ([]macvm.Share, []guestagent.Mount, []macvm.Disk, []guestagent.Disk, error) {
	diskDrives, shareDrives := splitMacDisks(spec.Drives)
	spec.Drives = shareDrives
	shares, mounts, err := d.macShares(ctx, spec)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	disks, guest, err := d.macDiskDrives(diskDrives)
	return shares, mounts, disks, guest, err
}

// macDiskDrives is each disk drive of a macOS VM run as the helper attaches it and the guest agent mounts it.
// A disk another run on this Mac holds refuses the run here, before anything exists; the helper takes
// the lock for the VM's life (macvm.LockDisk), so this check is the early answer, not the guard.
func (d *Driver) macDiskDrives(drives []types.DriveMount) ([]macvm.Disk, []guestagent.Disk, error) {
	c := d.macVMConfig()
	var disks []macvm.Disk
	var guest []guestagent.Disk
	for i := range drives {
		drive := &drives[i]
		if err := runner.ValidateDriveTarget(drive); err != nil {
			return nil, nil, fmt.Errorf("docker: denied user drive (%s) -> %q: %w (%v)",
				runner.DriveSubject(drive), drive.Target, errDriveTargetInvalid, err)
		}
		target, err := macShareTarget(drive.Target)
		if err != nil {
			return nil, nil, err
		}
		img, err := c.Layout.DriveImage(drive.ObjectName)
		if err != nil {
			return nil, nil, fmt.Errorf("docker: denied user drive (%s): %w", runner.DriveSubject(drive), err)
		}
		if !drive.ReadOnly && drive.SizeMiB <= 0 {
			return nil, nil, fmt.Errorf("docker: denied user drive (%s): a disk drive needs a size", runner.DriveSubject(drive))
		}
		unlock, err := macvm.LockDisk(img)
		if errors.Is(err, macvm.ErrDiskInUse) {
			//lint:ignore ST1005 the sentence the person reads, as written in the approved copy
			return nil, nil, errors.New(runner.MacVMDiskLocked(cmp.Or(drive.DriveName, runner.DriveSubject(drive))))
		}
		if err != nil {
			return nil, nil, fmt.Errorf("docker: user drive (%s): %w", runner.DriveSubject(drive), err)
		}
		unlock()
		id := macvm.DiskID(i)
		disks = append(disks, macvm.Disk{ID: id, Path: img, ReadOnly: drive.ReadOnly, SizeBytes: int64(drive.SizeMiB) << 20})
		guest = append(guest, guestagent.Disk{ID: id, Label: macDiskLabel(drive.DriveID.String()), Target: target, ReadOnly: drive.ReadOnly})
	}
	return disks, guest, nil
}

// macDiskLabel is the name of a disk drive's APFS volume: the same on every run, and different for every
// drive, since macOS mounts a volume at /Volumes/<name> before the guest agent moves it.
func macDiskLabel(driveID string) string { return "Wardyn-" + driveID[:8] }

// nonDiskDrives is a run's drives other than its disk drives.
func nonDiskDrives(drives []types.DriveMount) []types.DriveMount {
	_, rest := splitMacDisks(drives)
	return rest
}

// reclaimMacDisk deletes a disk drive's image from this Mac, under its lock: runner.ErrDriveInUse while
// a run here has it attached. Only a name Wardyn mints is ever removed, and only in the drives folder.
func (d *Driver) reclaimMacDisk(drive types.DriveMount) (runner.DriveReclaimOutcome, error) {
	c := d.macVMConfig()
	if c == nil {
		return "", fmt.Errorf("docker: drive: this runner keeps no disk drives: %w", runner.ErrDriveNotReclaimable)
	}
	img, err := c.Layout.DriveImage(drive.ObjectName)
	if err != nil {
		return "", fmt.Errorf("docker: drive: %v: %w", err, runner.ErrDriveNotReclaimable)
	}
	gone, err := macvm.RemoveDiskImage(img)
	switch {
	case errors.Is(err, macvm.ErrDiskInUse):
		return "", fmt.Errorf("docker: drive: %s is attached to a run: %w", drive.ObjectName, runner.ErrDriveInUse)
	case err != nil:
		return "", fmt.Errorf("docker: drive: %w", err)
	case !gone:
		return runner.DriveReclaimAlreadyAbsent, nil
	}
	return runner.DriveReclaimDeleted, nil
}

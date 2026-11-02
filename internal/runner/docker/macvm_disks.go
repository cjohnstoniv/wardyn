// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"time"

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
	disks, guest, err := d.macDiskDrives(ctx, diskDrives)
	return shares, mounts, disks, guest, err
}

// macDiskDrives is each disk drive of a macOS VM run as the helper attaches it and the guest agent mounts it.
// A disk a live run of this runner holds refuses the run here at once, before anything exists; one held by
// anything else (a run that has just ended and is still releasing it, or another login's VM) is waited for
// (lockMacDiskDrive). The helper takes the lock for the VM's life (macvm.LockDisk), so this check is the
// early answer, not the guard.
func (d *Driver) macDiskDrives(ctx context.Context, drives []types.DriveMount) ([]macvm.Disk, []guestagent.Disk, error) {
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
		unlock, err := d.lockMacDiskDrive(ctx, img, drive)
		if err != nil {
			return nil, nil, err
		}
		size, err := macDiskSize(img, drive)
		unlock()
		if err != nil {
			return nil, nil, err
		}
		// `wardyn-runnerd drive open <name>` finds the drive by this file, which holds its name and volume
		// label (the runner's driveOpenNameFile). Best effort: a run never fails for it, and drive open
		// then says it cannot find the name.
		if drive.DriveName != "" {
			record, _ := json.Marshal(map[string]string{"name": drive.DriveName, "label": macDiskLabel(drive.DriveID.String())})
			nameFile := img + ".name"
			if old, rerr := os.ReadFile(nameFile); rerr != nil || !bytes.Equal(old, record) {
				tmp := nameFile + "." + strconv.Itoa(os.Getpid()) + ".tmp"
				werr := os.WriteFile(tmp, record, 0o600)
				if werr == nil {
					werr = os.Rename(tmp, nameFile)
				}
				if werr != nil {
					_ = os.Remove(tmp)
					slog.Warn("docker: could not record a disk drive's name for drive open", slog.String("drive", runner.DriveSubject(drive)), slog.Any("err", werr))
				}
			}
		}
		id := macvm.DiskID(i)
		disks = append(disks, macvm.Disk{ID: id, Path: img, ReadOnly: drive.ReadOnly, SizeBytes: size})
		guest = append(guest, guestagent.Disk{ID: id, Label: macDiskLabel(drive.DriveID.String()), Target: target, ReadOnly: drive.ReadOnly,
			SizeBytes: size})
	}
	return disks, guest, nil
}

// lockMacDiskDrive takes a disk drive's lock for the size check. A disk held when it asks is waited for,
// polling every three seconds for at most ninety (the VM of a run that has just ended holds it for up to
// about eighty after the run shows as ended), unless a live run of this runner has it: that one stays, so
// it is refused at once. The wait ends with the caller's ctx. Past the ninety seconds the refusal is the
// same sentence (runner.MacVMDiskLocked) with the wait said before it. The lock is a flock the kernel frees
// when its holder dies, so waiting can never be on a stale lock.
func (d *Driver) lockMacDiskDrive(ctx context.Context, img string, drive *types.DriveMount) (func(), error) {
	const (
		poll = 3 * time.Second
		wait = 90 * time.Second
	)
	refusal := runner.MacVMDiskLocked(cmp.Or(drive.DriveName, runner.DriveSubject(drive)))
	unlock, err := macvm.LockDisk(img)
	if !errors.Is(err, macvm.ErrDiskInUse) {
		return lockedMacDisk(unlock, err, drive)
	}
	if d.liveMacRunHoldsDisk(img) {
		return nil, errors.New(refusal)
	}
	log := slog.With(slog.String("drive", runner.DriveSubject(drive)))
	log.Info("docker: a macOS VM disk drive is held; waiting for its release", slog.Duration("up_to", wait))
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-waitCtx.Done():
			log.Info("docker: the macOS VM disk drive wait ended; it was not released", slog.Any("err", waitCtx.Err()))
			if ctx.Err() != nil {
				return nil, fmt.Errorf("docker: user drive (%s): %w", runner.DriveSubject(drive), ctx.Err())
			}
			return nil, errors.New("Waited " + strconv.Itoa(int(wait/time.Second)) + " seconds for the drive to be released. " + refusal)
		case <-tick.C:
		}
		unlock, err = macvm.LockDisk(img)
		switch {
		case !errors.Is(err, macvm.ErrDiskInUse):
			log.Info("docker: the macOS VM disk drive was released; the run goes on")
			return lockedMacDisk(unlock, err, drive)
		case d.liveMacRunHoldsDisk(img):
			log.Info("docker: a live run took the macOS VM disk drive while this one waited")
			return nil, errors.New(refusal)
		}
	}
}

// lockedMacDisk is LockDisk's answer as lockMacDiskDrive gives it: the unlock, or the error said of the drive.
func lockedMacDisk(unlock func(), err error, drive *types.DriveMount) (func(), error) {
	if err != nil {
		return nil, fmt.Errorf("docker: user drive (%s): %w", runner.DriveSubject(drive), err)
	}
	return unlock, nil
}

// liveMacRunHoldsDisk reports whether a macOS VM run this runner keeps has the disk image img in its run
// config. A run's entry leaves the driver's VMs as its teardown begins (teardownVM), before its helper lets
// the disk go, so the run that has just ended is not live here, and that is the one the wait is for.
func (d *Driver) liveMacRunHoldsDisk(img string) bool {
	held := false
	d.vms.Range(func(_, value any) bool {
		v, ok := value.(*vmRun)
		if !ok || !v.mac {
			return true
		}
		cfg, err := readMacRun(v.dir, v.runID)
		if err != nil {
			return true
		}
		held = slices.ContainsFunc(cfg.Disks, func(disk macvm.Disk) bool { return disk.Path == img })
		return !held
	})
	return held
}

// macDiskSize is the size the VM and the guest take for a disk drive: the image's own length when it
// exists, since it is made once and never resized, and the run's size only for a new one.
func macDiskSize(img string, drive *types.DriveMount) (int64, error) {
	fi, err := os.Stat(img)
	switch {
	case err == nil && fi.Mode().IsRegular():
		return fi.Size(), nil
	case err == nil || !errors.Is(err, os.ErrNotExist):
		return 0, fmt.Errorf("docker: user drive (%s): disk image: %w", runner.DriveSubject(drive), cmp.Or(err, errors.New("not a file")))
	}
	return int64(drive.SizeMiB) << 20, nil
}

// macDiskLabel is the name of a disk drive's volume: the same on every run, and different for every
// drive, since macOS mounts a volume at /Volumes/<name> before the guest agent moves it.
func macDiskLabel(driveID string) string { return "Wardyn-" + driveID[:8] }

// nonDiskDrives is a run's drives other than its disk drives.
func nonDiskDrives(drives []types.DriveMount) []types.DriveMount {
	_, rest := splitMacDisks(drives)
	return rest
}

// reclaimMacDisk deletes a disk drive's image and its lock file from this Mac, under the lock:
// runner.ErrDriveInUse while a run here has it attached. Only a name Wardyn mints is ever removed, and only in the drives folder.
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

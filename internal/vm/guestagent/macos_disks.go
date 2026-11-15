// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MacDisks attaches a run's disk drives in a macOS guest: Server.Disk there. Like MacShares it is
// plain file and command work, tested off a Mac with a fake runner. Every command it runs, and the
// ioreg key it finds a disk by, is below, so a Mac can check them.
type MacDisks struct {
	Run CmdRunner
	// Home is the agent's home: every target is strictly inside it.
	Home string
	// UID and GID are the agent user's, who owns each folder made for a target and a writable
	// volume's root.
	UID, GID int
	// Dev is the device folder, "" for /dev (tests).
	Dev string
	// Wait is how long to wait between the reads of a volume macOS is still mounting; 0 is a second.
	Wait time.Duration
}

// The guest's block devices are listed by ioreg: each one's whole disk is the first "BSD Name" under
// it and its length the first "Size". A macOS guest shows no trace of the VZ block device identifier,
// and every block device sits at the same registry location, so a disk drive is the one disk that is
// not the boot disk and has the drive's size. The boot disk is the APFS physical store of /.
var (
	macDiskBSDRe    = regexp.MustCompile(`"BSD Name" ?= ?"(disk[0-9]+)"`)
	macDiskSizeRe   = regexp.MustCompile(`"Size" ?= ?([0-9]+)`)
	macDiskBootRe   = regexp.MustCompile(`<key>APFSPhysicalStore</key>\s*<string>(disk[0-9]+)s[0-9]+</string>`)
	macDiskVolumeRe = regexp.MustCompile(`(?m)^\s*Device Identifier:\s*(disk[0-9]+s[0-9]+)\s*$`)
	macDiskIDRe     = regexp.MustCompile(`^wardyn-disk[0-9]+$`)
	macDiskLabelRe  = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)
)

// macDiskVolumeReads bounds the wait for macOS to mount a disk's volume after it attached.
const macDiskVolumeReads = 10

// Attach mounts disk d at its target. A disk that holds nothing yet (a new drive's first run) is
// given a Mac OS Extended volume named d.Label first; a read-only one never is. The volume macOS mounted under
// /Volumes is unmounted and mounted again at the target, nosuid and nodev, read-only when asked, with
// its root the agent's when writable.
func (m MacDisks) Attach(ctx context.Context, d Disk) error {
	if !macDiskIDRe.MatchString(d.ID) || !macDiskLabelRe.MatchString(d.Label) {
		return fmt.Errorf("disk %q is not a Wardyn disk drive", d.ID)
	}
	if err := checkMacShareTarget(m.Home, d.Target); err != nil {
		return err
	}
	dev, err := m.device(ctx, d)
	if err != nil {
		return err
	}
	blank, err := m.blank(dev)
	if err != nil {
		return err
	}
	if blank {
		if d.ReadOnly {
			return fmt.Errorf("disk %s has no volume yet, and a read-only drive is never formatted", d.ID)
		}
		if _, err := m.Run.Run(ctx, "/usr/sbin/diskutil", "eraseDisk", "JHFS+", d.Label, "GPT", dev); err != nil {
			return fmt.Errorf("disk %s: format: %w", d.ID, err)
		}
	}
	vol, err := m.volume(ctx, d.Label)
	if err != nil {
		return fmt.Errorf("disk %s: %w", d.ID, err)
	}
	if _, err := m.Run.Run(ctx, "/usr/sbin/diskutil", "unmount", vol); err != nil {
		return fmt.Errorf("disk %s: unmount %s: %w", d.ID, vol, err)
	}
	if err := (MacShares{Home: m.Home, UID: m.UID, GID: m.GID}).makeTarget(d.Target); err != nil {
		return err
	}
	if _, err := m.Run.Run(ctx, "/usr/sbin/diskutil", macDiskMountArgs(d, vol)...); err != nil {
		return fmt.Errorf("disk %s: mount at %s: %w", d.ID, d.Target, err)
	}
	if !d.ReadOnly {
		return os.Lchown(d.Target, m.UID, m.GID)
	}
	return nil
}

// macDiskMountArgs is diskutil's arguments to mount disk d's volume vol at its target.
func macDiskMountArgs(d Disk, vol string) []string {
	args := []string{"mount"}
	if d.ReadOnly {
		args = append(args, "readOnly")
	}
	return append(args, "-mountOptions", "nosuid,nodev", "-mountPoint", d.Target, vol)
}

// device is the whole disk (diskN) of disk drive d: the one block device that is not the boot disk
// and is d.SizeBytes long. Two such disks are refused rather than told apart by a guess.
func (m MacDisks) device(ctx context.Context, d Disk) (string, error) {
	out, err := m.Run.Run(ctx, "/usr/sbin/ioreg", "-r", "-c", "IOBlockStorageDevice", "-l", "-w", "0")
	if err != nil {
		return "", fmt.Errorf("disk %s: list the block devices: %w", d.ID, err)
	}
	info, err := m.Run.Run(ctx, "/usr/sbin/diskutil", "info", "-plist", "/")
	if err != nil {
		return "", fmt.Errorf("disk %s: find the boot disk: %w", d.ID, err)
	}
	boot := macDiskBootRe.FindSubmatch(info)
	if boot == nil {
		return "", fmt.Errorf("disk %s: find the boot disk: no APFS physical store for /", d.ID)
	}
	if d.SizeBytes <= 0 {
		return "", fmt.Errorf("disk %s is not attached to this VM: the runner sent no size to find it by", d.ID)
	}
	var found, seen []string
	// With -r each device's subtree starts at a "+-o" line in the first column.
	for _, dev := range strings.Split("\n"+string(out), "\n+-o ")[1:] {
		b, n := macDiskBSDRe.FindStringSubmatch(dev), macDiskSizeRe.FindStringSubmatch(dev)
		if b == nil || n == nil || b[1] == string(boot[1]) {
			continue
		}
		seen = append(seen, b[1]+" "+n[1])
		if n[1] == strconv.FormatInt(d.SizeBytes, 10) {
			found = append(found, b[1])
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("disk %s is not attached to this VM: no disk of %d bytes beside the boot disk (attached: %s)",
			d.ID, d.SizeBytes, cmp.Or(strings.Join(seen, ", "), "none"))
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("disk %s: %d disks of its size are attached to this VM, and a run cannot tell them apart; "+
		"a macOS VM run attaches one disk drive, and a kept work folder counts as one", d.ID, len(found))
}

// blank reports whether the disk's first 4 KiB are all zero: an image the runner just made, which
// holds no partition map yet. A disk whose first 4 KiB cannot all be read is an error, never blank: a
// short read, or none, proves nothing about the rest.
func (m MacDisks) blank(dev string) (bool, error) {
	f, err := os.Open(filepath.Join(cmp.Or(m.Dev, "/dev"), "r"+dev))
	if err != nil {
		return false, fmt.Errorf("disk %s: %w", dev, err)
	}
	defer f.Close()
	head := make([]byte, 4096)
	if _, err := io.ReadFull(f, head); err != nil {
		return false, fmt.Errorf("disk %s: its first 4 KiB could not be read, so it was not formatted: %w", dev, err)
	}
	return bytes.Count(head, []byte{0}) == len(head), nil
}

// volume is the device identifier (diskNsM) of the volume macOS mounted at /Volumes/<label>, read
// until it is there.
func (m MacDisks) volume(ctx context.Context, label string) (string, error) {
	wait := m.Wait
	if wait == 0 {
		wait = time.Second
	}
	for i := 0; ; i++ {
		out, err := m.Run.Run(ctx, "/usr/sbin/diskutil", "info", "/Volumes/"+label)
		if err == nil {
			if v := macDiskVolumeRe.FindSubmatch(out); v != nil {
				return string(v[1]), nil
			}
		}
		if i+1 == macDiskVolumeReads {
			return "", fmt.Errorf("its volume %s was not mounted", label)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
}

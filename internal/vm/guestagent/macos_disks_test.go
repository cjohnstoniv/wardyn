// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// macDiskIoreg is ioreg -r -c IOBlockStorageDevice -l's shape for the boot disk and two disk drives.
const macDiskIoreg = `+-o AppleVirtIOBlockStorageDevice  <class AppleVirtIOBlockStorageDevice>
    {
      "Device Characteristics" = {"Product Name"="VirtIO Block Device","Serial Number"=""}
    }
    +-o IOMedia  <class IOMedia>
        {
          "BSD Name" = "disk0"
        }
+-o AppleVirtIOBlockStorageDevice  <class AppleVirtIOBlockStorageDevice>
    {
      "Device Characteristics" = {"Product Name"="VirtIO Block Device","Serial Number"="wardyn-disk1"}
    }
    +-o IOMedia  <class IOMedia>
        {
          "BSD Name" = "disk4"
        }
+-o AppleVirtIOBlockStorageDevice  <class AppleVirtIOBlockStorageDevice>
    {
      "Device Characteristics" = {"Product Name"="VirtIO Block Device","Serial Number"="wardyn-disk2"}
    }
    +-o IOMedia  <class IOMedia>
        {
          "BSD Name" = "disk5"
        }
`

// macDiskGuest is a guest whose disk4 holds head, with diskutil answering that /Volumes/<label> is disk7s1.
func macDiskGuest(t *testing.T, head []byte) (MacDisks, *fakeRunner) {
	t.Helper()
	dev := t.TempDir()
	if err := os.WriteFile(filepath.Join(dev, "rdisk4"), head, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{out: func(argv []string) ([]byte, error) {
		switch {
		case argv[0] == "/usr/sbin/ioreg":
			return []byte(macDiskIoreg), nil
		case len(argv) > 2 && argv[1] == "info":
			return []byte("   Device Identifier:         disk7s1\n   Volume Name:               Wardyn-1a2b3c4d\n"), nil
		}
		return nil, nil
	}}
	return MacDisks{Run: r, Home: t.TempDir(), UID: os.Getuid(), GID: os.Getgid(), Dev: dev, Wait: 1}, r
}

func TestMacDisksFormatsANewDiskThenMountsItAtItsTarget(t *testing.T) {
	m, r := macDiskGuest(t, make([]byte, 8192))
	target := filepath.Join(m.Home, "work")
	if err := m.Attach(context.Background(), Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: target}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/usr/sbin/ioreg", "-r", "-c", "IOBlockStorageDevice", "-l", "-w", "0"},
		{"/usr/sbin/diskutil", "eraseDisk", "APFS", "Wardyn-1a2b3c4d", "GPT", "disk4"},
		{"/usr/sbin/diskutil", "info", "/Volumes/Wardyn-1a2b3c4d"},
		{"/usr/sbin/diskutil", "unmount", "disk7s1"},
		{"/usr/sbin/diskutil", "mount", "-mountOptions", "nosuid,nodev", "-mountPoint", target, "disk7s1"},
	}
	if !slices.EqualFunc(r.calls, want, slices.Equal) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("target not made: %v", err)
	}
}

func TestMacDisksNeverFormatsADiskThatHoldsAVolume(t *testing.T) {
	head := make([]byte, 4096)
	copy(head[512:], "EFI PART")
	m, r := macDiskGuest(t, head)
	target := filepath.Join(m.Home, "drive")
	if err := m.Attach(context.Background(), Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: target, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if slices.Contains(c, "eraseDisk") {
			t.Fatalf("a disk with a partition map was formatted: %q", r.calls)
		}
	}
	if last := r.calls[len(r.calls)-1]; !slices.Equal(last, []string{"/usr/sbin/diskutil", "mount", "readOnly", "-mountOptions", "nosuid,nodev", "-mountPoint", target, "disk7s1"}) {
		t.Fatalf("mount = %q", last)
	}
}

func TestMacDisksRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		head []byte
		disk func(home string) Disk
		want string
	}{
		"a disk this VM does not have": {make([]byte, 4096), func(h string) Disk {
			return Disk{ID: "wardyn-disk3", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x")}
		}, "not attached"},
		"an identifier that is not a drive's": {make([]byte, 4096), func(h string) Disk {
			return Disk{ID: "disk0", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x")}
		}, "not a Wardyn disk drive"},
		"a label with a path in it": {make([]byte, 4096), func(h string) Disk {
			return Disk{ID: "wardyn-disk1", Label: "../x", Target: filepath.Join(h, "x")}
		}, "not a Wardyn disk drive"},
		"a target outside the home": {make([]byte, 4096), func(string) Disk {
			return Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: "/tmp/x"}
		}, "not inside"},
		"a new disk asked for read-only": {make([]byte, 4096), func(h string) Disk {
			return Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x"), ReadOnly: true}
		}, "never formatted"},
	} {
		t.Run(name, func(t *testing.T) {
			m, r := macDiskGuest(t, tc.head)
			err := m.Attach(context.Background(), tc.disk(m.Home))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, c := range r.calls {
				if slices.Contains(c, "eraseDisk") || slices.Contains(c, "mount") {
					t.Fatalf("a refused disk was touched: %q", r.calls)
				}
			}
		})
	}
}

func TestMacDisksWaitsForTheVolumeThenGivesUp(t *testing.T) {
	m, r := macDiskGuest(t, []byte("not blank"))
	r.out = func(argv []string) ([]byte, error) {
		if argv[0] == "/usr/sbin/ioreg" {
			return []byte(macDiskIoreg), nil
		}
		return nil, errors.New("Could not find disk")
	}
	err := m.Attach(context.Background(), Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(m.Home, "x")})
	if err == nil || !strings.Contains(err.Error(), "was not mounted") {
		t.Fatalf("err = %v", err)
	}
	reads := 0
	for _, c := range r.calls {
		if slices.Contains(c, "info") {
			reads++
		}
	}
	if reads != macDiskVolumeReads {
		t.Fatalf("read the volume %d times, want %d", reads, macDiskVolumeReads)
	}
}

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

// macDiskIoreg is ioreg -r -c IOBlockStorageDevice -l -w 0 on a macOS 26 guest with one disk drive
// attached, trimmed from a capture: both devices sit at @1, the boot disk is disk1 there and the
// 2 GiB drive disk0, and neither carries the VZ block device identifier.
const macDiskIoreg = `+-o AppleVirtIODiskStorageDevice@1  <class AppleVirtIODiskStorageDevice, id 0x1000001eb, registered, matched, active, busy 0 (187 ms), retain 11>
    {
      "IOUnit" = 1
      "device-type" = "Generic"
      "Device Characteristics" = {"Medium Type"="Solid State"}
      "AvpVirtioBlockDeviceRole" = 1
    }
    +-o IOBlockStorageDriver  <class IOBlockStorageDriver, id 0x1000001ec, registered, matched, active, busy 0 (187 ms), retain 8>
      +-o Apple Inc. VirtIO Block Media  <class IOMedia, id 0x1000001ed, registered, matched, active, busy 0 (187 ms), retain 12>
        |   "Whole" = Yes
        |   "BSD Name" = "disk1"
        |   "Size" = 53687091200
        +-o IOGUIDPartitionScheme  <class IOGUIDPartitionScheme, id 0x1000001f6, !registered, !matched, active, busy 0 (180 ms), retain 8>
          +-o iBootSystemContainer@1  <class IOMedia, id 0x1000001f7, registered, matched, active, busy 0 (16 ms), retain 12>
            |   "Size" = 524288000
            |   "BSD Name" = "disk1s1"
+-o AppleVirtIODiskStorageDevice@1  <class AppleVirtIODiskStorageDevice, id 0x1000001fa, registered, matched, active, busy 0 (12 ms), retain 11>
    {
      "IOUnit" = 1
      "device-type" = "Generic"
      "Device Characteristics" = {"Medium Type"="Solid State"}
      "AvpVirtioBlockDeviceRole" = 1
    }
    +-o IOBlockStorageDriver  <class IOBlockStorageDriver, id 0x1000001fb, registered, matched, active, busy 0 (12 ms), retain 8>
      +-o Apple Inc. VirtIO Block Media  <class IOMedia, id 0x1000001fc, registered, matched, active, busy 0 (12 ms), retain 12>
        |   "Whole" = Yes
        |   "BSD Name" = "disk0"
        |   "Size" = 2147483648
`

// macDiskBootPlist is diskutil info -plist / on that guest, trimmed: / is on disk1's APFS container.
const macDiskBootPlist = `<dict>
	<key>APFSPhysicalStores</key>
	<array>
		<dict>
			<key>APFSPhysicalStore</key>
			<string>disk1s2</string>
		</dict>
	</array>
</dict>
`

// macDiskSize is the attached drive's length in macDiskIoreg.
const macDiskSize = 2147483648

// macDiskGuest is a guest whose disk4 holds head, with diskutil answering that /Volumes/<label> is disk7s1.
func macDiskGuest(t *testing.T, head []byte) (MacDisks, *fakeRunner) {
	t.Helper()
	dev := t.TempDir()
	if err := os.WriteFile(filepath.Join(dev, "rdisk0"), head, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{out: func(argv []string) ([]byte, error) {
		switch {
		case argv[0] == "/usr/sbin/ioreg":
			return []byte(macDiskIoreg), nil
		case slices.Equal(argv, []string{"/usr/sbin/diskutil", "info", "-plist", "/"}):
			return []byte(macDiskBootPlist), nil
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
	if err := m.Attach(context.Background(), Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: target, SizeBytes: macDiskSize}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/usr/sbin/ioreg", "-r", "-c", "IOBlockStorageDevice", "-l", "-w", "0"},
		{"/usr/sbin/diskutil", "info", "-plist", "/"},
		{"/usr/sbin/diskutil", "eraseDisk", "JHFS+", "Wardyn-1a2b3c4d", "GPT", "disk0"},
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
	if err := m.Attach(context.Background(), Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: target, ReadOnly: true, SizeBytes: macDiskSize}); err != nil {
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
			return Disk{ID: "wardyn-disk3", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x"), SizeBytes: 1 << 30}
		}, "no disk of 1073741824 bytes beside the boot disk (attached: disk0 2147483648)"},
		"a disk whose size an older runner did not send": {make([]byte, 4096), func(h string) Disk {
			return Disk{ID: "wardyn-disk0", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x")}
		}, "the runner sent no size"},
		"the boot disk's size, which only the boot disk has": {make([]byte, 4096), func(h string) Disk {
			return Disk{ID: "wardyn-disk0", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x"), SizeBytes: 53687091200}
		}, "no disk of 53687091200 bytes beside the boot disk (attached: disk0 2147483648)"},
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
			return Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x"), ReadOnly: true, SizeBytes: macDiskSize}
		}, "never formatted"},
		// A short read, or none, is not a blank disk: its first 4 KiB were never seen.
		"a disk that gives less than 4 KiB": {make([]byte, 100), func(h string) Disk {
			return Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x"), SizeBytes: macDiskSize}
		}, "its first 4 KiB could not be read, so it was not formatted"},
		"a disk that gives nothing": {nil, func(h string) Disk {
			return Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(h, "x"), SizeBytes: macDiskSize}
		}, "its first 4 KiB could not be read, so it was not formatted"},
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
	m, r := macDiskGuest(t, append([]byte("not blank"), make([]byte, 4096)...))
	r.out = func(argv []string) ([]byte, error) {
		switch {
		case argv[0] == "/usr/sbin/ioreg":
			return []byte(macDiskIoreg), nil
		case slices.Equal(argv, []string{"/usr/sbin/diskutil", "info", "-plist", "/"}):
			return []byte(macDiskBootPlist), nil
		}
		return nil, errors.New("Could not find disk")
	}
	err := m.Attach(context.Background(), Disk{ID: "wardyn-disk1", Label: "Wardyn-1a2b3c4d", Target: filepath.Join(m.Home, "x"), SizeBytes: macDiskSize})
	if err == nil || !strings.Contains(err.Error(), "was not mounted") {
		t.Fatalf("err = %v", err)
	}
	reads := 0
	for _, c := range r.calls {
		if slices.Contains(c, "info") && !slices.Contains(c, "-plist") {
			reads++
		}
	}
	if reads != macDiskVolumeReads {
		t.Fatalf("read the volume %d times, want %d", reads, macDiskVolumeReads)
	}
}

// TestMacDisksFindTheDriveByItsSizeApartFromTheBootDisk covers the lookup itself: the boot disk is never
// a drive even at the drive's size, and two disks a run cannot tell apart are refused, not guessed, with
// the whole sentence: the control plane refuses the pair at create, and this is its backstop.
func TestMacDisksFindTheDriveByItsSizeApartFromTheBootDisk(t *testing.T) {
	bootSized := strings.Replace(macDiskIoreg, `"Size" = 53687091200`, `"Size" = 2147483648`, 1)
	twoDrives := macDiskIoreg + strings.ReplaceAll(macDiskIoreg[strings.LastIndex(macDiskIoreg, "+-o "):], `"disk0"`, `"disk4"`)
	for name, tc := range map[string]struct {
		ioreg, boot, dev, err string
	}{
		"a drive numbered below the boot disk": {macDiskIoreg, macDiskBootPlist, "disk0", ""},
		"a boot disk of the drive's size":      {bootSized, macDiskBootPlist, "disk0", ""},
		"two disks of the drive's size": {twoDrives, macDiskBootPlist, "", "disk wardyn-disk0: 2 disks of its size are attached to this VM, " +
			"and a run cannot tell them apart; a macOS VM run attaches one disk drive, and a kept work folder counts as one"},
		"a boot store that is not a partition": {macDiskIoreg, strings.Replace(macDiskBootPlist, "disk1s2", "disk1", 1), "", "find the boot disk"},
		"no boot store at all":                 {macDiskIoreg, "<dict/>", "", "find the boot disk"},
	} {
		t.Run(name, func(t *testing.T) {
			m := MacDisks{Run: &fakeRunner{out: func(argv []string) ([]byte, error) {
				if argv[0] == "/usr/sbin/ioreg" {
					return []byte(tc.ioreg), nil
				}
				return []byte(tc.boot), nil
			}}}
			dev, err := m.device(context.Background(), Disk{ID: "wardyn-disk0", SizeBytes: macDiskSize})
			if dev != tc.dev || (err == nil) != (tc.err == "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("device = %q, %v; want %q, %q", dev, err, tc.dev, tc.err)
			}
		})
	}
}

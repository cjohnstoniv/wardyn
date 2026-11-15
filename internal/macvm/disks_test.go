// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

const testDriveObject = "wardyn-drive-0123456789abcdef0123456789abcdef-d-0123456789abcdef"

func TestDriveImageIsAMintedNameUnderDrives(t *testing.T) {
	l := macvm.Layout{Root: "/r"}
	got, err := l.DriveImage(testDriveObject)
	if err != nil || got != "/r/drives/"+testDriveObject+".img" {
		t.Fatalf("DriveImage = %q, %v", got, err)
	}
	for _, bad := range []string{"", "disk", "wardyn-drive-", "wardyn-drive-../x", "wardyn-drive-a/b", "wardyn-drive-A", "../wardyn-drive-a"} {
		if _, err := l.DriveImage(bad); err == nil {
			t.Errorf("DriveImage(%q) was not refused", bad)
		}
	}
}

func TestDiskLockIsOneRunAtATime(t *testing.T) {
	img := filepath.Join(t.TempDir(), "d.img")
	unlock, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.LockDisk(img); !errors.Is(err, macvm.ErrDiskInUse) {
		t.Fatalf("second lock: %v, want ErrDiskInUse", err)
	}
	if _, err := macvm.RemoveDiskImage(img); !errors.Is(err, macvm.ErrDiskInUse) {
		t.Fatalf("remove under a run's lock: %v, want ErrDiskInUse", err)
	}
	unlock()
	again, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatalf("lock after the run ended: %v", err)
	}
	again()
}

func TestMakeDiskImageIsSparseAndKeepsAnExistingOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drives")
	img := filepath.Join(dir, "d.img")
	if err := macvm.MakeDiskImage(img, 1<<30); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(img)
	if err != nil || fi.Size() != 1<<30 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("image: %v %v", fi, err)
	}
	if d, err := os.Stat(dir); err != nil || d.Mode().Perm() != 0o700 {
		t.Fatalf("drives folder: %v %v", d, err)
	}
	if err := os.WriteFile(img, []byte("files"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := macvm.MakeDiskImage(img, 1<<30); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(img); string(b) != "files" {
		t.Fatal("an existing image was made again")
	}
	link := filepath.Join(dir, "link.img")
	if err := os.Symlink(img, link); err != nil {
		t.Fatal(err)
	}
	if err := macvm.MakeDiskImage(link, 1<<30); err == nil {
		t.Fatal("a link was taken for an image")
	}
	if err := macvm.MakeDiskImage(filepath.Join(dir, "unsized.img"), 0); err == nil {
		t.Fatal("an unsized image was made")
	}
	if _, err := os.Stat(filepath.Join(dir, "unsized.img")); !os.IsNotExist(err) {
		t.Fatal("an unsized image was left behind")
	}
}

func TestRemoveDiskImage(t *testing.T) {
	img := filepath.Join(t.TempDir(), "d.img")
	if gone, err := macvm.RemoveDiskImage(img); gone || err != nil {
		t.Fatalf("absent image: %v %v", gone, err)
	}
	if err := macvm.MakeDiskImage(img, 1<<20); err != nil {
		t.Fatal(err)
	}
	if gone, err := macvm.RemoveDiskImage(img); !gone || err != nil {
		t.Fatalf("remove: %v %v", gone, err)
	}
	if _, err := os.Stat(img); !os.IsNotExist(err) {
		t.Fatal("the image is still there")
	}
	if _, err := os.Stat(img + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the lock file is still there: %v", err)
	}
	if _, err := os.Stat(img + ".new"); !os.IsNotExist(err) {
		t.Fatalf("the new image's mark is still there: %v", err)
	}
	// A lock file left by an earlier run of a drive whose image is gone goes too, and the lock comes back.
	again, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatal(err)
	}
	again()
	if gone, err := macvm.RemoveDiskImage(img); gone || err != nil {
		t.Fatalf("absent image: %v %v", gone, err)
	}
	if _, err := os.Stat(img + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the lock file of an absent image is still there: %v", err)
	}
}

// A guest formats a disk that starts with 4 KiB of zeros, so CheckDiskImage passes such an image only with the
// mark MakeDiskImage left beside that very file. Once the drive is formatted (it no longer starts with zeros,
// or a guest had it mounted) the mark is used up, and the drive zeroed there later is refused, as are an image
// put in a new image's place and one shorter than 4 KiB. A refused image is not written to.
func TestCheckDiskImagePassesABlankImageOnlyWithItsNewMark(t *testing.T) {
	const refused = "Wardyn did not format this disk drive, so any files it holds are still in its image. The image on this Mac "
	const restart = ". To start again with an empty drive, delete this drive and create a new one; deleting it asks this Mac to delete the image."
	const zeros = refused + "starts with 4 KiB of zeros, where a formatted drive keeps its partition map, and this Mac has no record that the runner made it new" + restart
	dir := filepath.Join(t.TempDir(), "drives")
	img := filepath.Join(dir, "d.img")
	if err := macvm.CheckDiskImage(img); err != nil {
		t.Fatalf("no image yet: %v", err)
	}
	if err := macvm.MakeDiskImage(img, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := macvm.CheckDiskImage(img); err != nil {
		t.Fatalf("a new image: %v", err)
	}
	writeAt := func(b []byte, off int64) {
		t.Helper()
		f, err := os.OpenFile(img, os.O_WRONLY, 0)
		if err == nil {
			_, err = f.WriteAt(b, off)
			_ = f.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	writeAt([]byte("EFI PART"), 512) // what a format leaves at the head
	if err := macvm.CheckDiskImage(img); err != nil {
		t.Fatalf("a formatted image: %v", err)
	}
	if _, err := os.Stat(img + ".new"); !os.IsNotExist(err) {
		t.Fatalf("the mark outlived the format: %v", err)
	}
	writeAt(make([]byte, 8), 512) // zeroed again: damaged, not new
	if err := macvm.CheckDiskImage(img); err == nil || err.Error() != zeros {
		t.Fatalf("a formatted image zeroed at its head: %v, want %q", err, zeros)
	}

	mounted := filepath.Join(dir, "m.img")
	if err := macvm.MakeDiskImage(mounted, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := macvm.UseUpNewDisk(mounted); err != nil {
		t.Fatal(err)
	}
	if err := macvm.CheckDiskImage(mounted); err == nil || err.Error() != zeros {
		t.Fatalf("an image a guest had mounted, still blank: %v, want %q", err, zeros)
	}

	// A file put in a new image's place (made first, so it cannot reuse the inode) is not new by its mark.
	swapped := filepath.Join(dir, "s.img")
	if err := macvm.MakeDiskImage(swapped, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(swapped+".tmp", make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swapped+".tmp", swapped); err != nil {
		t.Fatal(err)
	}
	if err := macvm.CheckDiskImage(swapped); err == nil || err.Error() != zeros {
		t.Fatalf("a blank image in a new image's place: %v, want %q", err, zeros)
	}

	// A symlink at an image's path is never followed, even to a formatted image.
	formatted := filepath.Join(dir, "f.img")
	if err := os.WriteFile(formatted, bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l.img")
	if err := os.Symlink(formatted, link); err != nil {
		t.Fatal(err)
	}
	if err := macvm.CheckDiskImage(link); err == nil {
		t.Fatal("a symlink at the image's path passed")
	}

	short := filepath.Join(dir, "short.img")
	if err := os.WriteFile(short, []byte("files"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := refused + "is 5 bytes long, shorter than the 4 KiB a formatted drive starts with" + restart
	if err := macvm.CheckDiskImage(short); err == nil || err.Error() != want {
		t.Fatalf("a short image: %v, want %q", err, want)
	}
	if b, _ := os.ReadFile(short); string(b) != "files" {
		t.Fatal("a refused image was written to")
	}
}

// The helper never builds a VM with an image the guest would format but the runner did not make new: an
// existing image that starts with 4 KiB of zeros and has no mark, or one shorter than 4 KiB. It leaves the
// image as it was and the disk's lock free.
func TestRunRefusesABlankDiskItDidNotMakeNew(t *testing.T) {
	for name, body := range map[string][]byte{
		"4 KiB of zeros and no mark": make([]byte, 1<<20),
		"shorter than 4 KiB":         []byte("files"),
	} {
		t.Run(name, func(t *testing.T) {
			eng := &macvmtest.Engine{}
			img := filepath.Join(t.TempDir(), "drives", "d.img")
			if err := os.MkdirAll(filepath.Dir(img), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(img, body, 0o600); err != nil {
				t.Fatal(err)
			}
			r := startRun(t, eng, func(c *macvm.RunConfig, _ *macvm.RunOptions) {
				c.Disks = []macvm.Disk{{ID: macvm.DiskID(0), Path: img, SizeBytes: int64(len(body))}}
			})
			if n := len(eng.Machines()); n != 0 {
				t.Fatalf("%d VMs built with the disk", n)
			}
			select {
			case err := <-r.done:
				if err == nil || !strings.Contains(err.Error(), "Wardyn did not format this disk drive") {
					t.Fatalf("run = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the run did not end")
			}
			if b, err := os.ReadFile(img); err != nil || !bytes.Equal(b, body) {
				t.Fatalf("the image changed: %v", err)
			}
			unlock, err := macvm.LockDisk(img)
			if err != nil {
				t.Fatalf("the refused run kept the disk's lock: %v", err)
			}
			unlock()
		})
	}
}

// diskSpec is a run's machine spec, with no disk drive yet.
func diskSpec(t *testing.T) macvm.MachineSpec {
	t.Helper()
	g, h, err := macvm.NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = h.Close() })
	dir := t.TempDir()
	return macvm.MachineSpec{CPUs: 4, MemoryBytes: 8 << 30, HardwareModel: []byte("hw"), MachineIdentifier: []byte("id"),
		DiskPath: filepath.Join(dir, "run", macvm.DiskFile), AuxPath: filepath.Join(dir, "run", macvm.AuxFile),
		BaseDir: filepath.Join(dir, "base"), Net: g}
}

func TestPlanAttachesDisksAfterTheClone(t *testing.T) {
	spec := diskSpec(t)
	spec.Disks = []macvm.Disk{{ID: macvm.DiskID(0), Path: "/r/drives/a.img"}, {ID: macvm.DiskID(1), Path: "/r/drives/b.img", ReadOnly: true}}
	p, err := macvm.BuildPlan(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []macvm.StorageDevice{{Path: spec.DiskPath}, {Path: "/r/drives/a.img", Identifier: "wardyn-disk0"},
		{Path: "/r/drives/b.img", ReadOnly: true, Identifier: "wardyn-disk1"}}
	if len(p.Storage) != len(want) {
		t.Fatalf("storage = %+v", p.Storage)
	}
	for i := range want {
		if p.Storage[i] != want[i] {
			t.Fatalf("storage[%d] = %+v, want %+v", i, p.Storage[i], want[i])
		}
	}
	if err := macvm.CheckPlan(p); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*macvm.MachineSpec){
		"out of order":    func(s *macvm.MachineSpec) { s.Disks[0].ID = macvm.DiskID(1) },
		"relative":        func(s *macvm.MachineSpec) { s.Disks[0].Path = "a.img" },
		"inside the base": func(s *macvm.MachineSpec) { s.Disks[0].Path = filepath.Join(s.BaseDir, "disk.img") },
	} {
		s := diskSpec(t)
		s.Disks = []macvm.Disk{{ID: macvm.DiskID(0), Path: "/r/drives/a.img"}}
		mod(&s)
		if _, err := macvm.BuildPlan(s); err == nil {
			t.Errorf("%s: a disk was planned", name)
		}
	}
	p.Storage[0].Identifier = "wardyn-disk9"
	if err := macvm.CheckPlan(p); err == nil {
		t.Fatal("a plan whose clone carries an identifier was checked")
	}
}

func TestRunLocksItsDiskForTheVMsLife(t *testing.T) {
	eng := &macvmtest.Engine{}
	var img string
	r := startRun(t, eng, func(c *macvm.RunConfig, _ *macvm.RunOptions) {
		img = filepath.Join(filepath.Dir(c.BaseDir), "..", "drives", "d.img")
		img = filepath.Clean(img)
		c.Disks = []macvm.Disk{{ID: macvm.DiskID(0), Path: img, SizeBytes: 1 << 20}}
	})
	if _, err := macvm.LockDisk(img); !errors.Is(err, macvm.ErrDiskInUse) {
		t.Fatalf("the helper does not hold its disk: %v", err)
	}
	if fi, err := os.Stat(img); err != nil || fi.Size() != 1<<20 {
		t.Fatalf("the disk was not made: %v %v", fi, err)
	}
	if ms := eng.Machines(); len(ms) != 1 || len(ms[0].Plan.Storage) != 2 || ms[0].Plan.Storage[1].Identifier != macvm.DiskID(0) {
		t.Fatalf("the VM was built without its disk")
	}
	held := startRun(t, &macvmtest.Engine{}, func(c *macvm.RunConfig, _ *macvm.RunOptions) {
		c.Disks = []macvm.Disk{{ID: macvm.DiskID(0), Path: img, SizeBytes: 1 << 20}}
	})
	if err := <-held.done; err == nil || !strings.Contains(err.Error(), macvm.ErrDiskInUse.Error()) {
		t.Fatalf("a second run took a held disk: %v", err)
	}
	r.cancel()
	<-r.done
	unlock, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatalf("the disk stayed locked after the VM ended: %v", err)
	}
	unlock()
}

// A tools-update or prepare boot attaches no disk drive, as it shares no folder.
func TestStage2BootRefusesDisks(t *testing.T) {
	cfg := macvm.Stage2BootConfig{Engine: &macvmtest.Engine{}, SockRoot: macvmtest.SockRoot(t), SockKey: macvm.JobSockKey("0123456789abcdef01234567"),
		SockFstat: macvmtest.SockFstat}
	spec := macvm.MachineSpec{Disks: []macvm.Disk{{ID: macvm.DiskID(0), Path: "/a.img"}}}
	if _, _, err := cfg.ToolsBoot()(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "shares none") {
		t.Fatalf("ToolsBoot with a disk = %v", err)
	}
}

// The mark goes before the image. A folder that holds a file stands in for an image that cannot be removed:
// the remove fails, and no mark is left that could vouch for a later file at that path.
func TestRemoveDiskImageRemovesTheMarkBeforeTheImage(t *testing.T) {
	img := filepath.Join(t.TempDir(), "d.img")
	if err := os.MkdirAll(filepath.Join(img, "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img+".new", []byte("1 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if gone, err := macvm.RemoveDiskImage(img); gone || err == nil {
		t.Fatalf("an image that cannot be removed: %v %v", gone, err)
	}
	if _, err := os.Stat(img + ".new"); !os.IsNotExist(err) {
		t.Fatalf("the mark outlived an image that could not be removed: %v", err)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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
	disks, guest, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget), testDiskDrive(runner.ScratchWorkPath)})
	if err != nil {
		t.Fatal(err)
	}
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	if len(disks) != 2 || disks[0] != (macvm.Disk{ID: "wardyn-disk0", Path: img, SizeBytes: 1 << 30}) || disks[1].ID != "wardyn-disk1" {
		t.Fatalf("disks = %+v", disks)
	}
	want := guestagent.Disk{ID: "wardyn-disk0", Label: "Wardyn-1a2b3c4d", Target: guestagent.MacAgentHome + "/drive", SizeBytes: 1 << 30}
	if len(guest) != 2 || guest[0] != want || guest[1].Target != guestagent.MacAgentHome+"/work" {
		t.Fatalf("guest disks = %+v", guest)
	}
}

// The image is made once, at the size the run had then; a later run, at another size, sends the
// image's own length, so the guest finds the disk by it.
func TestMacDiskSizeIsTheExistingImagesAndTheRunsOnlyForANewOne(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	if err := macvm.MakeDiskImage(img, 2<<20); err != nil {
		t.Fatal(err)
	}
	disks, guest, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)}) // asks for 1 GiB
	if err != nil {
		t.Fatal(err)
	}
	if disks[0].SizeBytes != 2<<20 || guest[0].SizeBytes != 2<<20 {
		t.Fatalf("sizes = %d, %d, want the image's %d", disks[0].SizeBytes, guest[0].SizeBytes, 2<<20)
	}
}

// holdingMacRun stores a live run of this driver whose run config names the image img, as a started run's
// does, and takes the image's lock for it; the unlock is safe to call twice.
func holdingMacRun(t *testing.T, r *macRig, img string) func() {
	t.Helper()
	runID := uuid.MustParse("5a5a5a5a-0000-4000-8000-0000000000aa")
	dir := r.c.runDir(runID)
	if err := macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: runID.String(), BaseDir: "/base", Disks: []macvm.Disk{{ID: "wardyn-disk0", Path: img}}}); err != nil {
		t.Fatal(err)
	}
	r.d.vms.Store("live", &vmRun{runID: runID, dir: dir, mac: true})
	unlock, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatal(err)
	}
	once := sync.OnceFunc(unlock)
	t.Cleanup(once)
	return once
}

// A disk a live run of this driver holds (its entry is in the driver's VMs, as a run that has just ended
// has it until its helper exits) is waited for like any other: released at 20 s, the create goes on at the
// first poll after, 21 s.
func TestMacDisksWaitForADiskALiveRunHolds(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	unlock := holdingMacRun(t, r, img)
	if gone, err := r.d.ReclaimDrive(t.Context(), testDiskDrive(runner.DriveTarget)); gone != "" || !errors.Is(err, runner.ErrDriveInUse) {
		t.Fatalf("reclaim of a held disk = %q, %v", gone, err)
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		go func() {
			time.Sleep(20 * time.Second)
			unlock()
		}()
		disks, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)})
		if err != nil || len(disks) != 1 {
			t.Fatalf("disks = %+v, err = %v, want the disk once the live run released it", disks, err)
		}
		if waited := time.Since(start); waited != 21*time.Second {
			t.Fatalf("waited %v, want 21s: the first poll after the release at 20s", waited)
		}
	})
}

// A live run's disk that is never released fails at exactly ninety seconds, in the wait-stated sentence.
func TestMacDisksALiveRunNeverReleasingFailsAtNinetySeconds(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	holdingMacRun(t, r, img)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)})
		want := "Waited 90 seconds for the drive to be released. " + runner.MacVMDiskLocked("Notes")
		if err == nil || err.Error() != want || !runner.IsMacVMDiskLocked(err.Error()) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if waited := time.Since(start); waited != 90*time.Second {
			t.Fatalf("waited %v, want 90s", waited)
		}
	})
}

// A disk held by anything (the run that has just ended, still being released, or another login's VM) is
// waited for: three seconds at a time, for ninety.
func TestMacDisksWaitForADiskReleasedWithinNinetySeconds(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	held, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatal(err)
	}
	unlock := sync.OnceFunc(held)
	defer unlock()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		go func() {
			time.Sleep(10 * time.Second)
			unlock()
		}()
		disks, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)})
		if err != nil || len(disks) != 1 {
			t.Fatalf("disks = %+v, err = %v, want the disk once it was released", disks, err)
		}
		if waited := time.Since(start); waited != 12*time.Second {
			t.Fatalf("waited %v, want 12s: the first poll after the release at 10s", waited)
		}
	})
}

func TestMacDisksNeverReleasedFailAtNinetySecondsInTheSameSentence(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	unlock, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)})
		want := "Waited 90 seconds for the drive to be released. " + runner.MacVMDiskLocked("Notes")
		if err == nil || err.Error() != want || !runner.IsMacVMDiskLocked(err.Error()) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if waited := time.Since(start); waited != 90*time.Second {
			t.Fatalf("waited %v, want 90s", waited)
		}
	})
}

// The ninety seconds are the run's, not each drive's: a first drive released after 40 s leaves a second,
// never released, 50 s, so the run fails at 90 s in all and "Waited 90 seconds" is what the run waited.
func TestMacDisksShareOneNinetySecondWaitAcrossDrives(t *testing.T) {
	r := newMacRig(t)
	second := testDiskDrive(runner.ScratchWorkPath)
	second.ObjectName = "wardyn-drive-0123456789abcdef0123456789abcdef-d-fedcba9876543210"
	second.DriveName = "Other"
	var held [2]func()
	for i, d := range []types.DriveMount{testDiskDrive(runner.DriveTarget), second} {
		img, err := r.c.Layout.DriveImage(d.ObjectName)
		if err != nil {
			t.Fatal(err)
		}
		unlock, err := macvm.LockDisk(img)
		if err != nil {
			t.Fatal(err)
		}
		held[i] = sync.OnceFunc(unlock)
		defer held[i]()
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		go func() {
			time.Sleep(40 * time.Second)
			held[0]()
		}()
		_, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget), second})
		want := "Waited 90 seconds for the drive to be released. " + runner.MacVMDiskLocked("Other")
		if err == nil || err.Error() != want || !runner.IsMacVMDiskLocked(err.Error()) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if waited := time.Since(start); waited != 90*time.Second {
			t.Fatalf("waited %v in all, want 90s: the second drive gets what the first left, not a fresh 90s", waited)
		}
	})
}

func TestMacDisksWaitEndsWithTheCreate(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	unlock, err := macvm.LockDisk(img)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		time.AfterFunc(10*time.Second, cancel)
		_, _, err := r.d.macDiskDrives(ctx, []types.DriveMount{testDiskDrive(runner.DriveTarget)})
		if !errors.Is(err, context.Canceled) || runner.IsMacVMDiskLocked(err.Error()) {
			t.Fatalf("err = %v, want the create's cancel", err)
		}
		if waited := time.Since(start); waited != 10*time.Second {
			t.Fatalf("waited %v, want 10s", waited)
		}
	})
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
		if _, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{drive}); err == nil {
			t.Errorf("%s: the disk was attached", name)
		}
	}
}

// An existing image the guest would format but this Mac did not make new (it starts with 4 KiB of zeros and
// has no mark, or it is shorter than 4 KiB) is refused before the run starts, in one sentence that names the
// drive, says it was not formatted, and says how to start again. The image is not written to.
func TestMacDiskDrivesRefuseABlankImageThisMacDidNotMakeNew(t *testing.T) {
	const refused = "Notes: Wardyn did not format this disk drive, so any files it holds are still in its image. The image on this Mac "
	const restart = ". To start again with an empty drive, delete this drive and create a new one; deleting it asks this Mac to delete the image."
	for name, c := range map[string]struct {
		body []byte
		want string
	}{
		"4 KiB of zeros and no mark": {make([]byte, 2<<20), refused +
			"starts with 4 KiB of zeros, where a formatted drive keeps its partition map, and this Mac has no record that the runner made it new" + restart},
		"shorter than 4 KiB": {[]byte("files"), refused + "is 5 bytes long, shorter than the 4 KiB a formatted drive starts with" + restart},
	} {
		t.Run(name, func(t *testing.T) {
			r := newMacRig(t)
			img, _ := r.c.Layout.DriveImage(testDiskObject)
			if err := os.MkdirAll(filepath.Dir(img), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(img, c.body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)}); err == nil || err.Error() != c.want {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if b, err := os.ReadFile(img); err != nil || string(b) != string(c.body) {
				t.Fatalf("the refused image changed: %v", err)
			}
		})
	}
}

// A run's new disk drive: the helper makes the image with its mark, the guest has the drive mounted, and the
// run uses the mark up. So after that run, the image still starting with 4 KiB of zeros (this fake guest never
// formats it, which is how a drive damaged there looks) is refused for the next run, not formatted.
func TestAMacRunUsesUpItsNewDrivesMark(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	spec := macSpec()
	spec.Drives = []types.DriveMount{testDiskDrive(runner.DriveTarget)}
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(img); err != nil {
		t.Fatalf("the run made no image: %v", err)
	}
	if _, err := os.Stat(img + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the new drive's mark outlived the guest's mount: %v", err)
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	_, _, err = r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)})
	if err == nil || !strings.HasPrefix(err.Error(), "Notes: Wardyn did not format this disk drive") {
		t.Fatalf("the next run of the blank image = %v", err)
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
	if _, err := os.Stat(img + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the lock file is still there: %v", err)
	}
	drive.ObjectName = "../../x"
	if _, err := r.d.ReclaimDrive(t.Context(), drive); !errors.Is(err, runner.ErrDriveNotReclaimable) {
		t.Fatalf("a name outside the drives folder: %v", err)
	}
}

// A run whose disk drive another VM on this Mac holds is refused at its start: no helper starts, the
// ledger holds nothing for it, and the run that holds the disk keeps its lock. Whoever holds it, a live run
// of this runner included, is waited for first, and the same sentence says so (lockMacDiskDrive).
func TestARunRefusedForAHeldDiskAttachesNothing(t *testing.T) {
	for name, c := range map[string]struct {
		liveRun bool
		want    string
		waited  time.Duration
	}{
		"held by a live run of this runner": {true, "Waited 90 seconds for the drive to be released. " + runner.MacVMDiskLocked("Notes"), 90 * time.Second},
		"held by anything else":             {false, "Waited 90 seconds for the drive to be released. " + runner.MacVMDiskLocked("Notes"), 90 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			r := newMacRig(t)
			img, _ := r.c.Layout.DriveImage(testDiskObject)
			if c.liveRun {
				runID := uuid.MustParse("5a5a5a5a-0000-4000-8000-0000000000aa")
				dir := r.c.runDir(runID)
				if err := macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: runID.String(), BaseDir: "/base", Disks: []macvm.Disk{{ID: "wardyn-disk0", Path: img}}}); err != nil {
					t.Fatal(err)
				}
				r.d.vms.Store("live", &vmRun{runID: runID, dir: dir, mac: true})
			}
			unlock, err := macvm.LockDisk(img)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			spawned := 0
			r.c.spawn = func(string) error { spawned++; return nil }
			spec := macSpec()
			spec.Drives = []types.DriveMount{testDiskDrive(runner.DriveTarget)}
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				if _, err := r.d.CreateSandbox(t.Context(), spec); err == nil || err.Error() != c.want {
					t.Fatalf("err = %v, want %q", err, c.want)
				}
				if waited := time.Since(start); waited != c.waited {
					t.Fatalf("waited %v, want %v", waited, c.waited)
				}
			})
			if spawned != 0 || len(r.held(t)) != 0 {
				t.Fatalf("the refused run started %d helpers and holds %d ledger entries", spawned, len(r.held(t)))
			}
			if again, err := macvm.LockDisk(img); !errors.Is(err, macvm.ErrDiskInUse) {
				if again != nil {
					again()
				}
				t.Fatalf("the holder lost the disk's lock: %v", err)
			}
		})
	}
}

// A disk drive with a name leaves a name file beside its image for `wardyn-runnerd drive open`: JSON with the
// drive's name and its volume label, byte for byte what that command's tests read. A drive with no name
// leaves none, and a file that cannot be written never fails the run.
func TestMacDiskDrivesRecordTheDrivesNameForDriveOpen(t *testing.T) {
	r := newMacRig(t)
	img, _ := r.c.Layout.DriveImage(testDiskObject)
	nameFile := img + ".name"
	if _, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(nameFile); err != nil || string(b) != `{"label":"Wardyn-1a2b3c4d","name":"Notes"}` {
		t.Fatalf("name file = %q, %v", b, err)
	}
	if _, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)}); err != nil { // unchanged: not rewritten
		t.Fatal(err)
	}
	if err := os.Remove(nameFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nameFile, 0o700); err != nil { // a directory where the file goes: the write fails
		t.Fatal(err)
	}
	if _, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{testDiskDrive(runner.DriveTarget)}); err != nil {
		t.Fatalf("a name file that could not be written failed the run: %v", err)
	}
	if err := os.Remove(nameFile); err != nil {
		t.Fatal(err)
	}
	unnamed := testDiskDrive(runner.DriveTarget)
	unnamed.DriveName = ""
	if _, _, err := r.d.macDiskDrives(t.Context(), []types.DriveMount{unnamed}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(nameFile); !os.IsNotExist(err) {
		t.Fatalf("an unnamed drive left a name file: %v", err)
	}
}

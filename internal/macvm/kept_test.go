// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

func TestKeptMachineDirIsBesideTheRuns(t *testing.T) {
	l := macvm.Layout{Root: "/r"}
	id := uuid.NewString()
	if got, want := l.KeptMachineDir(id), filepath.Join("/r", "kept", macvm.RunKey(id)); got != want {
		t.Fatalf("KeptMachineDir = %s, want %s", got, want)
	}
	if filepath.Dir(l.KeptMachineDir(id)) != l.KeptDir() || filepath.Dir(l.RunDir(id)) == l.KeptDir() {
		t.Fatal("a kept machine's folder is not under runs/")
	}
}

func TestKeptRecordRoundTripChecksTheFullID(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	rec := macvm.KeptRecord{ID: id.String(), OwnerHash: "h", RunnerID: uuid.NewString(), Base: "0",
		CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := macvm.WriteKept(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := macvm.ReadKept(dir, id.String())
	if err != nil || got != rec {
		t.Fatalf("ReadKept = %+v, %v; want %+v", got, err, rec)
	}
	// Another id with the same 12-digit key reads the same folder, and is refused.
	other := id
	other[15] ^= 1
	if macvm.RunKey(other.String()) != macvm.RunKey(id.String()) {
		t.Fatal("test ids should share a key")
	}
	if _, err := macvm.ReadKept(dir, other.String()); !errors.Is(err, macvm.ErrKeptIDMismatch) {
		t.Fatalf("ReadKept(other id) = %v, want ErrKeptIDMismatch", err)
	}
	if _, err := macvm.ReadKept(filepath.Join(l.KeptDir(), "none"), id.String()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing record = %v, want ErrNotExist", err)
	}
	if err := macvm.WriteKept(dir, macvm.KeptRecord{ID: "nope"}); err == nil {
		t.Fatal("WriteKept took a non-UUID id")
	}
}

func TestKeptLockedProbe(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	dir := l.KeptMachineDir(uuid.NewString())
	if locked, err := macvm.KeptLocked(dir); locked || err != nil {
		t.Fatalf("never-booted machine: locked=%v err=%v", locked, err)
	}
	unlock, err := macvm.LockDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := macvm.KeptLocked(dir); !locked || err != nil {
		t.Fatalf("held: locked=%v err=%v", locked, err)
	}
	unlock()
	if locked, err := macvm.KeptLocked(dir); locked || err != nil {
		t.Fatalf("released: locked=%v err=%v", locked, err)
	}
}

func TestKeptAllocatedBytesCountsWrittenBlocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, macvm.DiskFile), make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := macvm.KeptAllocatedBytes(dir)
	if err != nil || n < 64<<10 {
		t.Fatalf("KeptAllocatedBytes = %d, %v; want at least 64 KiB", n, err)
	}
}

func TestKeptRunConfigRefusesABadKeptBlock(t *testing.T) {
	dir := macvm.Layout{Root: t.TempDir()}.RunDir(testRunID)
	for _, k := range []*macvm.KeptRun{{ID: "x", Mode: macvm.KeptNew}, {ID: uuid.NewString(), Mode: "revive"}} {
		cfg := macvm.RunConfig{RunID: testRunID, BaseDir: "/b", Kept: k}
		if err := macvm.WriteRunConfig(dir, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := macvm.ReadRunConfig(dir); err == nil {
			t.Fatalf("ReadRunConfig took kept block %+v", k)
		}
	}
	cfg := macvm.RunConfig{RunID: testRunID, BaseDir: "/b", Kept: &macvm.KeptRun{ID: uuid.NewString(), Mode: macvm.KeptResume}}
	if err := macvm.WriteRunConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := macvm.ReadRunConfig(dir); err != nil || got.Kept == nil || got.Kept.Mode != macvm.KeptResume {
		t.Fatalf("ReadRunConfig = %+v, %v", got, err)
	}
}

const keptOwner = "owner-hash"

func keptCfg(base, mode string, id uuid.UUID) macvm.RunConfig {
	return macvm.RunConfig{RunID: testRunID, BaseDir: base, Identity: macvm.IdentityTravel,
		Kept: &macvm.KeptRun{ID: id.String(), Mode: mode, OwnerHash: keptOwner, RunnerID: "runner-1", Base: "0", IdleDays: 30}}
}

// openKept opens the machine and releases its lock at once, as a stopped helper would.
func openKept(t *testing.T, cfg macvm.RunConfig, dir string, now time.Time) error {
	t.Helper()
	_, _, release, err := macvm.OpenKept(cfg, dir, now)
	if err == nil {
		release()
	}
	return err
}

func TestKeptNewClonesIntoTheKeptFolderAndResumeKeepsTheDisk(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	now := time.Now().UTC().Truncate(time.Second)

	man, mid, release, err := macvm.OpenKept(keptCfg(base, macvm.KeptNew, id), dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if string(mid.MachineIdentifier) != "base-machine-id" || man.MinCPUs != 2 {
		t.Fatalf("new: identity %q manifest %+v", mid.MachineIdentifier, man)
	}
	for f, want := range map[string]string{macvm.DiskFile: "base-disk", macvm.MachineIDFile: "base-machine-id"} {
		if got, _ := os.ReadFile(filepath.Join(dir, f)); string(got) != want {
			t.Fatalf("%s = %q", f, got)
		}
	}
	rec, err := macvm.ReadKept(dir, id.String())
	if err != nil || rec.OwnerHash != keptOwner || rec.RunnerID != "runner-1" || !rec.CreatedAt.Equal(now) || rec.Scrubbed ||
		!rec.ExpiresAt.Equal(now.AddDate(0, 0, 30)) {
		t.Fatalf("record %+v, %v", rec, err)
	}
	// One boot at a time: the lock is held until release.
	if err := openKept(t, keptCfg(base, macvm.KeptResume, id), dir, now); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("second boot = %v, want ErrKeptLocked", err)
	}
	if locked, _ := macvm.KeptLocked(dir); !locked {
		t.Fatal("KeptLocked is false while booted")
	}
	release()

	// The person's work, and a base that has moved on, change nothing about the resume.
	if err := os.WriteFile(filepath.Join(dir, macvm.DiskFile), []byte("persons-work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, macvm.DiskFile), []byte("new-base-disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec.Scrubbed = true
	if err := macvm.WriteKept(dir, rec); err != nil {
		t.Fatal(err)
	}
	_, mid, release, err = macvm.OpenKept(keptCfg(base, macvm.KeptResume, id), dir, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got, _ := os.ReadFile(filepath.Join(dir, macvm.DiskFile)); string(got) != "persons-work" || string(mid.MachineIdentifier) != "base-machine-id" {
		t.Fatalf("resume: disk %q, machine id %q", got, mid.MachineIdentifier)
	}
	if rec, _ = macvm.ReadKept(dir, id.String()); rec.Scrubbed {
		t.Fatal("a boot left scrubbed true")
	}
}

func TestKeptResumeRefusals(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	now := time.Now().UTC()
	if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, now); err != nil {
		t.Fatal(err)
	}
	other := keptCfg(base, macvm.KeptResume, id)
	other.Kept.OwnerHash = "someone-else"
	otherMac := keptCfg(base, macvm.KeptFresh, id)
	otherMac.Kept.RunnerID = "runner-2"
	noOwner := keptCfg(base, macvm.KeptResume, id)
	noOwner.Kept.OwnerHash = ""
	for name, c := range map[string]macvm.RunConfig{"owner": other, "runner": otherMac, "no owner": noOwner} {
		want := macvm.ErrKeptOwner
		if name == "no owner" {
			want = nil
		}
		if err := openKept(t, c, dir, now); err == nil || (want != nil && !errors.Is(err, want)) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := openKept(t, keptCfg(base, macvm.KeptResume, id), dir, now.AddDate(0, 0, 31)); !errors.Is(err, macvm.ErrKeptExpired) {
		t.Fatalf("expired = %v", err)
	}
	if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, now); err == nil {
		t.Fatal("new took a folder that is there")
	}
	if err := openKept(t, keptCfg(base, macvm.KeptResume, uuid.New()), l.KeptMachineDir(uuid.NewString()), now); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no such machine = %v", err)
	}
	if err := os.Remove(filepath.Join(dir, macvm.AuxFile)); err != nil {
		t.Fatal(err)
	}
	if err := openKept(t, keptCfg(base, macvm.KeptResume, id), dir, now); !errors.Is(err, macvm.ErrKeptIncomplete) {
		t.Fatalf("missing aux = %v", err)
	}
	// Nothing above touched the person's disk.
	if got, _ := os.ReadFile(filepath.Join(dir, macvm.DiskFile)); string(got) != "base-disk" {
		t.Fatalf("disk %q", got)
	}
}

func TestKeptFreshReplacesTheDiskUnderTheSameID(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	t0 := time.Now().UTC().Truncate(time.Second)
	if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, t0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, macvm.DiskFile), []byte("persons-work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, macvm.DiskFile), []byte("new-base-disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(48 * time.Hour)
	if err := openKept(t, keptCfg(base, macvm.KeptFresh, id), dir, t1); err != nil {
		t.Fatal(err)
	}
	rec, err := macvm.ReadKept(dir, id.String())
	if got, _ := os.ReadFile(filepath.Join(dir, macvm.DiskFile)); string(got) != "new-base-disk" || err != nil ||
		!rec.CreatedAt.Equal(t0) || !rec.ExpiresAt.Equal(t1.AddDate(0, 0, 30)) || rec.ID != id.String() {
		t.Fatalf("after reset: disk %q record %+v %v", got, rec, err)
	}
	if err := openKept(t, keptCfg(base, macvm.KeptResume, id), dir, t1); err != nil {
		t.Fatalf("the reset machine does not resume: %v", err)
	}
}

func TestKeptRunBootsFromTheKeptFolderAndRecordsTheStop(t *testing.T) {
	id := uuid.New()
	var dir string
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, func(c *macvm.RunConfig, _ *macvm.RunOptions) {
		*c = keptCfg(c.BaseDir, macvm.KeptNew, id)
		c.CPUs, c.MemoryMiB, c.SockRoot = 4, 8192, macvmtest.SockRoot(t)
	})
	dir = r.layout.KeptMachineDir(id.String())
	r.attach(t)
	waitState(t, r.cc, macvm.StateRunning)
	if p := eng.Machines()[0].Plan; p.Storage[0].Path != filepath.Join(dir, macvm.DiskFile) || p.AuxPath != filepath.Join(dir, macvm.AuxFile) {
		t.Fatalf("the VM boots %s and %s, not the kept folder", p.Storage[0].Path, p.AuxPath)
	}
	if _, err := os.Stat(filepath.Join(r.dir, macvm.DiskFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run folder holds a disk: %v", err)
	}
	if locked, _ := macvm.KeptLocked(dir); !locked {
		t.Fatal("the machine is not locked while its VM runs")
	}
	if err := r.cc.Stop(context.Background(), true, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	rec, err := macvm.ReadKept(dir, id.String())
	if err != nil || rec.Scrubbed || rec.OwnerHash != keptOwner {
		t.Fatalf("record after the run: %+v %v", rec, err)
	}
	if locked, _ := macvm.KeptLocked(dir); locked {
		t.Fatal("the lock outlived the helper's run")
	}
	// Ending the run deletes the run folder and keeps the machine.
	if err := os.RemoveAll(r.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, macvm.DiskFile)); err != nil {
		t.Fatalf("the kept disk went with the run: %v", err)
	}
}

func TestKeptLockAndRemove(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.LockKept(dir); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("second LockKept = %v, want ErrKeptLocked", err)
	}
	if _, err := macvm.RemoveKept(dir); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("RemoveKept under a lock = %v", err)
	}
	if err := openKept(t, keptCfg(base, macvm.KeptResume, id), dir, time.Now()); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("a boot beside the runner's lock = %v", err)
	}
	unlock()
	if gone, err := macvm.RemoveKept(dir); !gone || err != nil {
		t.Fatalf("RemoveKept = %v, %v", gone, err)
	}
	for _, p := range []string{dir, dir + ".lock"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived the delete: %v", p, err)
		}
	}
	if gone, err := macvm.RemoveKept(dir); gone || err != nil {
		t.Fatalf("RemoveKept of nothing = %v, %v", gone, err)
	}
}

func TestKeptWindowStripSaysTheSignInsStay(t *testing.T) {
	scr := &fakeScreen{}
	eng := &windowEngine{Engine: &macvmtest.Engine{}}
	t.Cleanup(macvm.SetWindowRecheck(20 * time.Millisecond))
	r := startRun(t, eng.Engine, func(c *macvm.RunConfig, o *macvm.RunOptions) {
		*c = keptCfg(c.BaseDir, macvm.KeptNew, uuid.New())
		c.CPUs, c.MemoryMiB, c.SockRoot = 4, 8192, macvmtest.SockRoot(t)
		o.Engine, o.Window = eng, scr
	})
	r.attach(t)
	waitState(t, r.cc, macvm.StateRunning)
	if _, err := r.cc.Window(context.Background(), "grant-1", ""); err != nil {
		t.Fatal(err)
	}
	if scr.opens() != 1 || scr.texts[0].Strip != macvm.KeptWindowStrip || scr.texts[0].Strip == macvm.WindowStrip {
		t.Fatalf("the kept window opened as %+v", scr.texts)
	}
}

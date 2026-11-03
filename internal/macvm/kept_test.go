// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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

func TestKeptRefusedBootWritesItsCode(t *testing.T) {
	t.Cleanup(macvm.SetKeptBootLockWait(50 * time.Millisecond))
	id := uuid.New()
	for _, tc := range []struct {
		name, code string
		err        error
		setup      func(l macvm.Layout, base, dir string) func()
	}{
		{"locked", macvm.KeptRefusalLocked, macvm.ErrKeptLocked, func(l macvm.Layout, base, dir string) func() {
			unlock, err := macvm.LockKept(dir)
			if err != nil {
				t.Fatal(err)
			}
			return unlock
		}},
		{"owner", macvm.KeptRefusalOwner, macvm.ErrKeptOwner, func(l macvm.Layout, base, dir string) func() {
			rec, _ := macvm.ReadKept(dir, id.String())
			rec.OwnerHash = "someone-else"
			if err := macvm.WriteKept(dir, rec); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}},
		{"incomplete", macvm.KeptRefusalIncomplete, macvm.ErrKeptIncomplete, func(l macvm.Layout, base, dir string) func() {
			if err := os.Remove(filepath.Join(dir, macvm.DiskFile)); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}},
		{"expired", macvm.KeptRefusalExpired, macvm.ErrKeptExpired, func(l macvm.Layout, base, dir string) func() {
			rec, _ := macvm.ReadKept(dir, id.String())
			rec.ExpiresAt = time.Now().Add(-time.Hour)
			if err := macvm.WriteKept(dir, rec); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}},
		{"id_mismatch", macvm.KeptRefusalIDMismatch, macvm.ErrKeptIDMismatch, func(l macvm.Layout, base, dir string) func() {
			rec, _ := macvm.ReadKept(dir, id.String())
			rec.ID = uuid.NewString()
			if err := macvm.WriteKept(dir, rec); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startRun(t, &macvmtest.Engine{}, func(c *macvm.RunConfig, _ *macvm.RunOptions) {
				l := macvm.Layout{Root: filepath.Dir(filepath.Dir(c.BaseDir))}
				dir := l.KeptMachineDir(id.String())
				if err := openKept(t, keptCfg(c.BaseDir, macvm.KeptNew, id), dir, time.Now()); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(tc.setup(l, c.BaseDir, dir))
				*c = keptCfg(c.BaseDir, macvm.KeptResume, id)
				c.CPUs, c.MemoryMiB, c.SockRoot = 4, 8192, macvmtest.SockRoot(t)
			})
			err := <-r.done
			if !errors.Is(err, tc.err) {
				t.Fatalf("Run = %v, want %v", err, tc.err)
			}
			if got, rerr := macvm.ReadKeptRefusal(r.dir); got != tc.code || rerr != nil || macvm.KeptRefusalCode(err) != tc.code {
				t.Fatalf("kept_refusal = %q, %v; want %q", got, rerr, tc.code)
			}
		})
	}
	// A boot that is not refused leaves no code.
	r := startRun(t, &macvmtest.Engine{}, func(c *macvm.RunConfig, _ *macvm.RunOptions) {
		*c = keptCfg(c.BaseDir, macvm.KeptNew, uuid.New())
		c.CPUs, c.MemoryMiB, c.SockRoot = 4, 8192, macvmtest.SockRoot(t)
	})
	if got, err := macvm.ReadKeptRefusal(r.dir); got != "" || err != nil {
		t.Fatalf("a boot that was not refused left kept_refusal %q, %v", got, err)
	}
}

func TestKeptResumeBeforeExpiryRefreshesItAndMarksTheRun(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	t0 := time.Now().UTC().Truncate(time.Second)
	if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, t0); err != nil {
		t.Fatal(err)
	}
	rec, err := macvm.ReadKept(dir, id.String())
	if err != nil || rec.ActiveRun != testRunID || !rec.LastUsed.Equal(t0) {
		t.Fatalf("after new: %+v %v", rec, err)
	}
	expiry := rec.ExpiresAt
	resume := keptCfg(base, macvm.KeptResume, id)
	resume.RunID = "22222222-2222-2222-2222-222222222222"
	at := expiry.Add(-time.Minute)
	if err := openKept(t, resume, dir, at); err != nil {
		t.Fatal(err)
	}
	rec, _ = macvm.ReadKept(dir, id.String())
	if !rec.ExpiresAt.Equal(at.AddDate(0, 0, 30)) || !rec.LastUsed.Equal(at) || rec.ActiveRun != resume.RunID || rec.Scrubbed {
		t.Fatalf("resume at expiry-1m: %+v", rec)
	}
	// A zero IdleDays reads as the default 30.
	resume.Kept.IdleDays = 0
	if err := openKept(t, resume, dir, at); err != nil {
		t.Fatal(err)
	}
	if rec, _ = macvm.ReadKept(dir, id.String()); !rec.ExpiresAt.Equal(at.AddDate(0, 0, macvm.KeptDefaultIdleDays)) {
		t.Fatalf("zero IdleDays gave expiry %v", rec.ExpiresAt)
	}
	// The same fresh boot marks the run and the allowance too.
	fresh := keptCfg(base, macvm.KeptFresh, id)
	fresh.RunID = "33333333-3333-3333-3333-333333333333"
	if err := openKept(t, fresh, dir, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec, _ = macvm.ReadKept(dir, id.String()); rec.ActiveRun != fresh.RunID || !rec.ExpiresAt.Equal(at.Add(time.Hour).AddDate(0, 0, 30)) {
		t.Fatalf("after fresh: %+v", rec)
	}
}

func TestKeptStopUpdateAfterTheOldExpiry(t *testing.T) {
	boot := time.Now().UTC().Truncate(time.Minute)
	oldExpiry := boot.Add(time.Minute)
	stop := boot.Add(31 * time.Minute)
	// The boot refreshed the expiry, so the record held the boot's date; model the worst case where it is
	// the old one, 10:00, and already past at the stop.
	rec := macvm.KeptRecord{ActiveRun: "run-a", LastUsed: boot, ExpiresAt: oldExpiry}
	other := rec
	if macvm.KeptStopUpdate(&other, "run-b", stop, 30) || other != rec {
		t.Fatalf("a stop for another run changed the record: %+v", other)
	}
	if macvm.KeptStopUpdate(&other, "", stop, 30) || other != rec {
		t.Fatal("a stop with no run id changed the record")
	}
	if !macvm.KeptStopUpdate(&rec, "run-a", stop, 30) {
		t.Fatal("the matching run's stop was refused")
	}
	if rec.ActiveRun != "" || !rec.LastUsed.Equal(stop) || !rec.ExpiresAt.Equal(stop.AddDate(0, 0, 30)) {
		t.Fatalf("after the stop: %+v", rec)
	}
	if macvm.KeptStopUpdate(&rec, "run-a", stop, 30) {
		t.Fatal("a second stop for the same run was taken")
	}
}

func TestKeptExpiredRecordIsStillRefusedAtBootAndNotRefreshed(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	t0 := time.Now().UTC().Truncate(time.Second)
	if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, t0); err != nil {
		t.Fatal(err)
	}
	before, _ := macvm.ReadKept(dir, id.String())
	late := before.ExpiresAt.Add(time.Second)
	for _, mode := range []string{macvm.KeptResume, macvm.KeptFresh} {
		if err := openKept(t, keptCfg(base, mode, id), dir, late); !errors.Is(err, macvm.ErrKeptExpired) {
			t.Fatalf("%s past expiry = %v", mode, err)
		}
	}
	if after, _ := macvm.ReadKept(dir, id.String()); after != before {
		t.Fatalf("a refused boot rewrote the record: %+v", after)
	}
}

func TestKeptBaseDigestPinsNewAndFreshAndResumeIgnoresIt(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	now := time.Now().UTC()
	digest, err := macvm.BaseDigest(base)
	if err != nil || digest == "" {
		t.Fatalf("BaseDigest = %q, %v", digest, err)
	}
	pinned := func(mode, d string) macvm.RunConfig {
		c := keptCfg(base, mode, id)
		c.Kept.BaseDigest = d
		return c
	}
	if err := openKept(t, pinned(macvm.KeptNew, "not-the-base"), dir, now); !errors.Is(err, macvm.ErrKeptBaseChanged) {
		t.Fatalf("new against another base = %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused new left a folder: %v", err)
	}
	if err := openKept(t, pinned(macvm.KeptNew, digest), dir, now); err != nil {
		t.Fatalf("new with the matching digest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, macvm.DiskFile), []byte("persons-work"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, f := range []string{macvm.DiskFile, macvm.AuxFile, macvm.MachineIDFile, macvm.PairKeyFile, macvm.ManifestFile, macvm.KeptFile} {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			out[f] = string(b)
		}
		return out
	}
	before := snapshot()
	// The base moves on (a tools update rewrites its record): fresh is refused with the old disk untouched.
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), []byte(`{"format":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := openKept(t, pinned(macvm.KeptFresh, digest), dir, now); !errors.Is(err, macvm.ErrKeptBaseChanged) {
		t.Fatalf("fresh against a moved base = %v", err)
	}
	if after := snapshot(); !maps.Equal(before, after) {
		t.Fatal("a refused fresh changed the kept folder")
	}
	// Resume boots the machine's own disk and ignores the pin.
	if err := openKept(t, pinned(macvm.KeptResume, digest), dir, now); err != nil {
		t.Fatalf("resume with a stale digest: %v", err)
	}
	moved, _ := macvm.BaseDigest(base)
	if err := openKept(t, pinned(macvm.KeptFresh, moved), dir, now); err != nil {
		t.Fatalf("fresh with the current digest: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, macvm.DiskFile)); string(got) != "base-disk" {
		t.Fatalf("fresh left disk %q", got)
	}
}

func TestKeptBaseChangedWritesItsCode(t *testing.T) {
	id := uuid.New()
	r := startRun(t, &macvmtest.Engine{}, func(c *macvm.RunConfig, _ *macvm.RunOptions) {
		dir := macvm.Layout{Root: filepath.Dir(filepath.Dir(c.BaseDir))}.KeptMachineDir(id.String())
		if err := openKept(t, keptCfg(c.BaseDir, macvm.KeptNew, id), dir, time.Now()); err != nil {
			t.Fatal(err)
		}
		*c = keptCfg(c.BaseDir, macvm.KeptFresh, id)
		c.Kept.BaseDigest = "not-the-base"
		c.CPUs, c.MemoryMiB, c.SockRoot = 4, 8192, macvmtest.SockRoot(t)
	})
	if err := <-r.done; !errors.Is(err, macvm.ErrKeptBaseChanged) {
		t.Fatalf("Run = %v", err)
	}
	if got, err := macvm.ReadKeptRefusal(r.dir); got != "base_changed" || err != nil {
		t.Fatalf("kept_refusal = %q, %v", got, err)
	}
}

func TestKeptIdleDaysAreStoredAtBootAndStopAndDerivedWithoutThem(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	now := time.Now().UTC().Truncate(time.Second)
	for i, days := range []int{1, 90, 0} {
		mode := macvm.KeptResume
		if i == 0 {
			mode = macvm.KeptNew
		}
		cfg := keptCfg(base, mode, id)
		cfg.Kept.IdleDays = days
		if err := openKept(t, cfg, dir, now); err != nil {
			t.Fatal(err)
		}
		rec, _ := macvm.ReadKept(dir, id.String())
		want := days
		if days == 0 {
			want = macvm.KeptDefaultIdleDays
		}
		if rec.IdleDays != want || macvm.KeptRecordIdleDays(rec) != want || !rec.ExpiresAt.Equal(now.AddDate(0, 0, want)) {
			t.Fatalf("boot with %d idle days: %+v", days, rec)
		}
		for _, stopDays := range []int{1, 90, -5} {
			s := rec
			if !macvm.KeptStopUpdate(&s, rec.ActiveRun, now, stopDays) {
				t.Fatal("stop refused")
			}
			wantStop := stopDays
			if stopDays <= 0 {
				wantStop = macvm.KeptDefaultIdleDays
			}
			if s.IdleDays != wantStop || !s.ExpiresAt.Equal(now.AddDate(0, 0, wantStop)) {
				t.Fatalf("stop with %d idle days: %+v", stopDays, s)
			}
		}
	}
	// A record from before the field: the span from last use to expiry, whole days, clamped to 1-90.
	for _, tc := range []struct {
		name string
		rec  macvm.KeptRecord
		want int
	}{
		{"span", macvm.KeptRecord{LastUsed: now, ExpiresAt: now.AddDate(0, 0, 14)}, 14},
		{"rounds", macvm.KeptRecord{LastUsed: now, ExpiresAt: now.Add(36 * time.Hour)}, 2},
		{"floor", macvm.KeptRecord{LastUsed: now, ExpiresAt: now.Add(time.Hour)}, 1},
		{"ceiling", macvm.KeptRecord{LastUsed: now, ExpiresAt: now.AddDate(0, 0, 400)}, 90},
		{"backwards", macvm.KeptRecord{LastUsed: now, ExpiresAt: now.Add(-time.Hour)}, 1},
		{"no last use", macvm.KeptRecord{ExpiresAt: now.AddDate(0, 0, 9)}, macvm.KeptDefaultIdleDays},
		{"no expiry", macvm.KeptRecord{LastUsed: now}, macvm.KeptDefaultIdleDays},
		{"stored wins", macvm.KeptRecord{IdleDays: 7, LastUsed: now, ExpiresAt: now.AddDate(0, 0, 14)}, 7},
	} {
		if got := macvm.KeptRecordIdleDays(tc.rec); got != tc.want {
			t.Errorf("%s: KeptRecordIdleDays = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestKeptBootWaitsBrieflyForTheLock(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	now := time.Now().UTC()
	if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, now); err != nil {
		t.Fatal(err)
	}
	hold := func(d time.Duration) {
		unlock, err := macvm.LockKept(dir)
		if err != nil {
			t.Fatal(err)
		}
		go func() { time.Sleep(d); unlock() }()
	}
	t.Cleanup(macvm.SetKeptBootLockWait(2 * time.Second))
	hold(150 * time.Millisecond)
	if err := openKept(t, keptCfg(base, macvm.KeptResume, id), dir, now); err != nil {
		t.Fatalf("a lock released within the bound refused the boot: %v", err)
	}
	macvm.SetKeptBootLockWait(50 * time.Millisecond)
	hold(time.Second)
	start := time.Now()
	if err := openKept(t, keptCfg(base, macvm.KeptResume, id), dir, now); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("a lock held past the bound = %v, want ErrKeptLocked", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("the refusal did not wait")
	}
	// LockKeptWait itself: a wait of 0 is the one try.
	if _, err := macvm.LockKeptWait(dir, 0); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("LockKeptWait(0) while held = %v", err)
	}
	time.Sleep(time.Second)
	unlock, err := macvm.LockKeptWait(dir, 0)
	if err != nil {
		t.Fatalf("LockKeptWait(0) once free: %v", err)
	}
	unlock()
}

func TestKeptResumeViewOnlyComesFromTheMachinesOwnManifest(t *testing.T) {
	setDesktop := func(dir, mode string) {
		t.Helper()
		m, err := macvm.ReadManifest(dir)
		if err != nil {
			t.Fatal(err)
		}
		m.Desktop = mode
		b, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, macvm.ManifestFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, kept, base string
		removeBase       bool
		want             bool
	}{
		{"base swapped to view only, machine has control", macvm.DesktopControl, macvm.DesktopObserve, false, false},
		{"base replaced by one with control, machine is view only", macvm.DesktopObserve, macvm.DesktopControl, false, true},
		{"base gone, machine has control", macvm.DesktopControl, macvm.DesktopControl, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := macvm.Layout{Root: t.TempDir()}
			base := makeBase(t, l)
			id := uuid.New()
			dir := l.KeptMachineDir(id.String())
			if err := openKept(t, keptCfg(base, macvm.KeptNew, id), dir, time.Now()); err != nil {
				t.Fatal(err)
			}
			setDesktop(dir, tc.kept)
			setDesktop(base, tc.base)
			if tc.removeBase {
				if err := os.RemoveAll(base); err != nil {
					t.Fatal(err)
				}
			}
			cfg := keptCfg(base, macvm.KeptResume, id)
			run := l.RunDir(cfg.RunID)
			if err := macvm.WriteRunConfig(run, cfg); err != nil {
				t.Fatal(err)
			}
			if got := macvm.BaseViewOnly(run); got != tc.want {
				t.Fatalf("view only = %v, want %v", got, tc.want)
			}
			// A new or fresh boot clones the current base, so it reads that.
			cfg.Kept.Mode = macvm.KeptFresh
			if err := macvm.WriteRunConfig(run, cfg); err != nil {
				t.Fatal(err)
			}
			if got, want := macvm.BaseViewOnly(run), tc.removeBase || tc.base != macvm.DesktopControl; got != want {
				t.Fatalf("fresh: view only = %v, want %v", got, want)
			}
		})
	}
}

// A machine's name is recorded when it is made, kept by a stop's rewrite and a boot that names none, and
// replaced by a boot that names it otherwise (a rename).
func TestKeptNameIsRecordedKeptAndRenamed(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	id := uuid.New()
	dir := l.KeptMachineDir(id.String())
	now := time.Now().UTC().Truncate(time.Second)
	boot := func(mode, name string) macvm.KeptRecord {
		t.Helper()
		cfg := keptCfg(base, mode, id)
		cfg.Kept.Name = name
		if err := openKept(t, cfg, dir, now); err != nil {
			t.Fatalf("%s %q: %v", mode, name, err)
		}
		rec, err := macvm.ReadKept(dir, id.String())
		if err != nil {
			t.Fatal(err)
		}
		if !macvm.KeptStopUpdate(&rec, testRunID, now, 30) {
			t.Fatalf("%s: the stop is not this run's", mode)
		}
		if err := macvm.WriteKept(dir, rec); err != nil {
			t.Fatal(err)
		}
		if rec, err = macvm.ReadKept(dir, id.String()); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	for _, step := range []struct{ mode, name, want string }{
		{macvm.KeptNew, "build box", "build box"},
		{macvm.KeptResume, "", "build box"},
		{macvm.KeptResume, "release box", "release box"},
		{macvm.KeptFresh, "", "release box"},
	} {
		if rec := boot(step.mode, step.name); rec.Name != step.want {
			t.Fatalf("%s %q: the record names %q, want %q", step.mode, step.name, rec.Name, step.want)
		}
	}
}

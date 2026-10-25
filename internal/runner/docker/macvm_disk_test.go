// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// createMac creates a macOS VM sandbox on r.
func createMac(t *testing.T, r *macRig) (runner.Sandbox, runner.SandboxSpec) {
	t.Helper()
	spec := macSpec()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return sb, spec
}

func wantDisk(t *testing.T, d *Driver, ref string, state, reason string) runner.MacOSVMDisk {
	t.Helper()
	got, ok := d.MacOSVMDisk(ref)
	if !ok || got.State != state || got.Reason != reason {
		t.Fatalf("report = %+v (%v), want %s/%q", got, ok, state, reason)
	}
	return got
}

// A teardown records removed, and the report stays where every later read finds it.
func TestMacVMDiskTeardownRecordsRemoved(t *testing.T) {
	r := newMacRig(t)
	sb, spec := createMac(t, r)
	before := time.Now()
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	got := wantDisk(t, r.d, sb.Ref, runner.MacOSVMDiskRemoved, "")
	if got.RunID != spec.RunID || got.At.Before(before) || got.At.After(after) {
		t.Fatalf("report %+v, want run %s at a time in [%v, %v]", got, spec.RunID, before, after)
	}
	if again, _ := r.d.MacOSVMDisk(sb.Ref); again != got {
		t.Fatalf("a second read = %+v, want %+v", again, got)
	}
	if all := r.d.MacOSVMDisks(); len(all) != 1 || all[0] != got {
		t.Fatalf("held reports %+v, want exactly %+v", all, got)
	}
}

// A helper that outlives its kill keeps the disk: kept, helper_alive, with the run directory in place. The
// record precedes the timeout-or-not split, so this case covers a destroy that ran out of time too.
func TestMacVMDiskHelperOutlivesItsKill(t *testing.T) {
	r := newMacRig(t)
	r.c.helperWait = 100 * time.Millisecond
	sb, spec := createMac(t, r)
	r.host.mu.Lock()
	r.host.pinned = map[int]bool{macHelperPID: true}
	r.host.mu.Unlock()
	_ = r.d.StopSandbox(context.Background(), sb.Ref)
	wantDisk(t, r.d, sb.Ref, runner.MacOSVMDiskKept, runner.MacOSVMDiskHelperAlive)
	if _, err := os.Lstat(r.c.runDir(spec.RunID)); err != nil {
		t.Fatalf("run directory gone with the helper alive: %v", err)
	}
}

// A delete that fails keeps the disk (remove_failed); the orphan sweep that then deletes it turns the report
// to removed, through recordMacOSVMDisk's kept-to-removed move.
func TestMacVMDiskKeptThenTheSweepRemovesIt(t *testing.T) {
	r := newMacRig(t)
	sb, spec := createMac(t, r)
	r.c.removeAll = func(string) error { return errors.New("operation not permitted") }
	_ = r.d.StopSandbox(context.Background(), sb.Ref)
	wantDisk(t, r.d, sb.Ref, runner.MacOSVMDiskKept, runner.MacOSVMDiskRemoveFailed)
	r.c.removeAll = os.RemoveAll
	if _, err := r.d.SweepOrphanedSandboxes(context.Background(), 0, func(uuid.UUID) bool { return true }); err != nil {
		t.Fatal(err)
	}
	wantDisk(t, r.d, sb.Ref, runner.MacOSVMDiskRemoved, "")
	if _, err := os.Lstat(r.c.runDir(spec.RunID)); !os.IsNotExist(err) {
		t.Fatalf("run directory left after the sweep: %v", err)
	}
}

// The offline queue's path calls KillSandbox directly; it records removed the same way.
func TestMacVMDiskKillRecordsRemoved(t *testing.T) {
	r := newMacRig(t)
	sb, _ := createMac(t, r)
	if err := r.d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	wantDisk(t, r.d, sb.Ref, runner.MacOSVMDiskRemoved, "")
}

// A Linux VM run and a container run record no report.
func TestMacVMDiskNoneForOtherKinds(t *testing.T) {
	ctx := context.Background()
	vm := newVMRig(t)
	vsb, err := vm.d.CreateSandbox(ctx, vmSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.d.StopSandbox(ctx, vsb.Ref); err != nil {
		t.Fatal(err)
	}
	d := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev"})
	csb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StopSandbox(ctx, csb.Ref); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		d   *Driver
		ref string
	}{"linux_vm": {vm.d, vsb.Ref}, "container": {d, csb.Ref}} {
		if all := c.d.MacOSVMDisks(); all != nil {
			t.Errorf("%s: held reports %+v, want none", name, all)
		}
		if got, ok := c.d.MacOSVMDisk(c.ref); ok {
			t.Errorf("%s: report %+v, want none", name, got)
		}
	}
}

// A report older than macOSVMDiskKeep is left out, and forgotten.
func TestMacVMDiskPrunesOldReports(t *testing.T) {
	d := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev"})
	id := uuid.New()
	d.macDisks.Store(id, runner.MacOSVMDisk{RunID: id, State: runner.MacOSVMDiskRemoved, At: time.Now().Add(-7 * time.Hour)})
	if all := d.MacOSVMDisks(); len(all) != 0 {
		t.Fatalf("held reports %+v, want the 7-hour-old one left out", all)
	}
	n := 0
	d.macDisks.Range(func(any, any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("%d reports still in the map, want it forgotten", n)
	}
}

// Removed is final: a later kept never replaces it, kept turns into removed, and racing writers end on removed.
func TestMacVMDiskRemovedIsFinal(t *testing.T) {
	d := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev"})
	read := func(id uuid.UUID) runner.MacOSVMDisk {
		v, ok := d.macDisks.Load(id)
		if !ok {
			t.Fatalf("no report for %s", id)
		}
		return v.(runner.MacOSVMDisk)
	}

	id := uuid.New()
	d.recordMacOSVMDisk(id, runner.MacOSVMDiskRemoved, "")
	first := read(id)
	d.recordMacOSVMDisk(id, runner.MacOSVMDiskKept, runner.MacOSVMDiskHelperAlive)
	if got := read(id); got != first {
		t.Fatalf("after a later kept: %+v, want the first removed %+v", got, first)
	}

	id2 := uuid.New()
	d.recordMacOSVMDisk(id2, runner.MacOSVMDiskKept, runner.MacOSVMDiskRemoveFailed)
	d.recordMacOSVMDisk(id2, runner.MacOSVMDiskRemoved, "")
	if got := read(id2); got.State != runner.MacOSVMDiskRemoved || got.Reason != "" {
		t.Fatalf("kept then removed: %+v, want removed", got)
	}

	id3 := uuid.New()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				d.recordMacOSVMDisk(id3, runner.MacOSVMDiskRemoved, "")
			} else {
				d.recordMacOSVMDisk(id3, runner.MacOSVMDiskKept, runner.MacOSVMDiskHelperAlive)
			}
		}()
	}
	wg.Wait()
	if got := read(id3); got.State != runner.MacOSVMDiskRemoved {
		t.Fatalf("after racing writers: %+v, want removed", got)
	}
}

// The prune read an expired report, then a sweep stored a fresh one for the same run: forgetting what the prune
// saw leaves the fresh report in place.
func TestMacVMDiskPruneKeepsAFreshReportStoredAfterItsRead(t *testing.T) {
	d := newWithClient(newFakeDocker(), Config{ProxyImage: "wardyn-proxy:dev"})
	id := uuid.New()
	d.macDisks.Store(id, runner.MacOSVMDisk{RunID: id, State: runner.MacOSVMDiskKept, At: time.Now().Add(-7 * time.Hour)})
	seen, _ := d.macDisks.Load(id)
	d.macDisks.Store(id, runner.MacOSVMDisk{RunID: id, State: runner.MacOSVMDiskRemoved, At: time.Now()})
	d.forgetMacOSVMDisk(id, seen)
	v, ok := d.macDisks.Load(id)
	if !ok || v.(runner.MacOSVMDisk).State != runner.MacOSVMDiskRemoved {
		t.Fatalf("report = %+v (%v), want the fresh removed one kept", v, ok)
	}
}

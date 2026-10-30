// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// offer is the rig runner's offer at the base's size.
func (r *macRig) offer(t *testing.T) admission.Offer {
	t.Helper()
	o, err := r.c.Ledger.Offer(4096<<20, admission.RunDiskReservation(macvm.BaseDiskBytes))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// held is every entry the rig's ledger still counts or holds as a disk debt.
func (r *macRig) held(t *testing.T) []admission.Entry {
	t.Helper()
	counted, err := r.c.Ledger.Counted()
	if err != nil {
		t.Fatal(err)
	}
	debts, _ := r.c.Ledger.Debts()
	return append(counted, debts...)
}

// A create that fails before the helper claims its entry (the helper never started, or never opened its
// socket) releases the entry at once, though the runnerd that took it is alive: two such failures leave
// nothing counted and the offer as it was.
func TestMacVMFailedCreateBeforeClaimReleasesItsSlot(t *testing.T) {
	for name, spawn := range map[string]func(string) error{
		"spawn fails":         func(string) error { return errors.New("exec: no such file") },
		"helper never starts": func(string) error { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := newMacRig(t)
			r.c.spawn = spawn
			before := r.offer(t)
			for range 2 {
				spec := macSpec()
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				_, err := r.d.CreateSandbox(ctx, spec)
				cancel()
				if err == nil {
					t.Fatal("the create succeeded with no helper")
				}
				if _, err := os.Lstat(r.c.runDir(spec.RunID)); !os.IsNotExist(err) {
					t.Fatalf("run directory left: %v", err)
				}
			}
			if held := r.held(t); len(held) != 0 {
				t.Fatalf("entries left after failed creates: %+v", held)
			}
			if after := r.offer(t); after != before {
				t.Fatalf("offer %+v after failed creates, %+v before", after, before)
			}
		})
	}
}

// A guest that shuts itself down, with no stop asked by the runner, ends the run, and its teardown
// releases the slot: nothing stays counted and the offer is back to what it was.
func TestMacVMGuestInitiatedStopEndsRunAndReleasesSlot(t *testing.T) {
	r := newMacRig(t)
	before := r.offer(t)
	sb, err := r.d.CreateSandbox(context.Background(), macSpec())
	if err != nil {
		t.Fatal(err)
	}
	if during := r.offer(t); during.Headroom != before.Headroom-1 {
		t.Fatalf("offer %+v with a run, %+v before", during, before)
	}
	ms := r.eng.Machines()
	if len(ms) != 1 {
		t.Fatalf("machines %+v", ms)
	}
	_ = ms[0].Stop() // the guest's own shutdown
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := r.d.Status(context.Background(), sb.Ref)
		if err == nil && st.State == types.RunStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run did not end after the guest stopped: %+v %v", st, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := r.d.Wait(context.Background(), sb.Ref); err == nil {
		t.Fatal("wait reported a clean exit for a VM that stopped")
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if held := r.held(t); len(held) != 0 {
		t.Fatalf("entries left after the guest stopped: %+v", held)
	}
	if after := r.offer(t); after != before {
		t.Fatalf("offer %+v after the run, %+v before", after, before)
	}
}

// A run directory is created new: one already there is refused (and kept), and a directory whose
// run.json names another run that shares the 12-digit prefix is never opened as this run's.
func TestMacVMRunDirNeverReusedAndCheckedByFullRunID(t *testing.T) {
	r := newMacRig(t)
	spec := macSpec()
	dir := r.c.runDir(spec.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "already at") {
		t.Fatalf("create over an existing run directory: %v", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("the existing directory was removed: %v", err)
	}
	if held := r.held(t); len(held) != 0 {
		t.Fatalf("entries left: %+v", held)
	}
	other := spec.RunID
	other[15] ^= 0xff // same first 12 hex digits, another run
	if err := macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: other.String(), BaseDir: r.c.Layout.BaseDir(0)}); err != nil {
		t.Fatal(err)
	}
	if v := r.d.macVMLookup(agentContainerName(spec.RunID)); v != nil {
		t.Fatal("the lookup opened another run's directory")
	}
	if r.d.teardownMacVM(context.Background(), spec.RunID) {
		t.Fatal("the teardown destroyed another run's directory")
	}
	e, err := r.c.Ledger.Admit(admission.Request{Kind: admission.KindRun, RunID: spec.RunID.String(), GuestCPUs: 2, GuestMemory: 4 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 1})
	if err != nil {
		t.Fatal(err)
	}
	if run, err := r.d.EndNewestMacVM(context.Background(), 1<<30); run != "" || err != nil {
		t.Fatalf("the low-disk check ended %q (%v) through another run's directory", run, err)
	}
	_ = r.c.Ledger.Release(e.ID, admission.Destroyed{Stopped: true})
}

// The low-disk check ends the newest run that has a VM: a newer entry still reserved, whose create has
// not written its run directory, is passed over.
func TestMacVMEndNewestSkipsAReservedEntryWithNoRunDir(t *testing.T) {
	r := newMacRig(t)
	spec := macSpec()
	if _, err := r.d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // a later Created
	if _, err := r.c.Ledger.Admit(admission.Request{Kind: admission.KindRun, RunID: uuid.NewString(), GuestCPUs: 2, GuestMemory: 4 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 1}); err != nil {
		t.Fatal(err)
	}
	if run, err := r.d.EndNewestMacVM(context.Background(), 1<<30); run != spec.RunID.String() || err != nil {
		t.Fatalf("ended %q, %v; want the running %s", run, err, spec.RunID)
	}
}

// With only macOS VMs on, a container's ref is looked up once: the miss is remembered, so later calls
// open no run directory for it.
func TestMacVMLookupRemembersAContainer(t *testing.T) {
	r := newMacRig(t)
	ref := agentContainerName(uuid.New())
	if v, err := r.d.vmLookup(context.Background(), ref); v != nil || err != nil {
		t.Fatalf("lookup of a container: %v %v", v, err)
	}
	if _, ok := r.d.notVMs.Load(ref); !ok {
		t.Fatal("the container's miss was not remembered")
	}
}

// The run's agent may not update the pinned harness CLIs: a macOS guest's Init always turns the
// harnesses' own auto-updater off, whatever the spec's env says.
func TestMacGuestInitTurnsTheAutoUpdaterOff(t *testing.T) {
	spec := macSpec()
	spec.Env["DISABLE_AUTOUPDATER"] = "0"
	if v := macGuestInit(spec, "").Env["DISABLE_AUTOUPDATER"]; v != "1" {
		t.Fatalf("DISABLE_AUTOUPDATER = %q", v)
	}
}

// A run's boot never asks for tools mode (vsock 1026): run.json has no field that could carry it, and
// the run.json a create writes holds only the run boot's own keys. A field added to RunConfig fails
// this test, so tools mode can reach a run boot only by a reviewed change here.
func TestMacVMRunBootCarriesNoToolsMode(t *testing.T) {
	want := []string{"base_dir", "cpus", "identity", "ledger_dir", "ledger_entry", "memory_mib", "run_id", "runner_id", "shares", "sock_root"}
	var got []string
	rt := reflect.TypeFor[macvm.RunConfig]()
	for i := range rt.NumField() {
		got = append(got, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("run.json fields %v, want exactly %v", got, want)
	}
	r := newMacRig(t)
	spec := macSpec()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.c.runDir(spec.RunID), macvm.RunConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]any
	if err := json.Unmarshal(b, &written); err != nil {
		t.Fatal(err)
	}
	for k := range written {
		if !slices.Contains(want, k) || strings.Contains(strings.ToLower(k), "tool") {
			t.Fatalf("run.json carries %q", k)
		}
	}
	_ = r.d.StopSandbox(context.Background(), sb.Ref)
}

// A macOS VM's start says each phase as it enters it, and a failure says the phase it stopped in: the
// clone (the helper never opened its socket), and the disk short of the run's reservation before the
// clone began, with its figures.
func TestMacVMStartReportsItsPhases(t *testing.T) {
	detail := func(reason string) string { return runner.MacOSStartDetail(reason) }
	run := func(t *testing.T, r *macRig) ([]string, error) {
		t.Helper()
		var said []string
		spec := macSpec()
		spec.OnWaiting = func(d string) { said = append(said, d) }
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := r.d.CreateSandbox(ctx, spec)
		return said, err
	}

	r := newMacRig(t)
	said, err := run(t, r)
	if want := []string{detail(runner.MacOSCloning), detail(runner.MacOSStarting), detail(runner.MacOSConnecting)}; err != nil || !slices.Equal(said, want) {
		t.Fatalf("a good start said %q (%v), want %q", said, err, want)
	}

	r = newMacRig(t)
	r.c.spawn = func(string) error { return nil } // the helper never starts
	said, err = run(t, r)
	if want := []string{detail(runner.MacOSCloning), detail(runner.MacOSCloneFailed)}; err == nil || !slices.Equal(said, want) {
		t.Fatalf("a helper that never starts said %q (%v), want %q", said, err, want)
	}

	r = newMacRig(t)
	r.host.freeDisk = 30 << 30
	said, err = run(t, r)
	need := admission.RunDiskReservation(macvm.BaseDiskBytes) + admission.DiskFloorBytes
	if want := []string{detail(runner.MacOSCloning), runner.MacOSDiskShortMessage(need, 30<<30)}; err == nil || !slices.Equal(said, want) {
		t.Fatalf("a Mac short of disk said %q (%v), want %q", said, err, want)
	}

	// The run's network and proxy are the start's, not the clone's: their failure is said as the start's,
	// before any clone began.
	r = newMacRig(t)
	r.f.networkCreateErr = errors.New("no network")
	said, err = run(t, r)
	if want := []string{detail(runner.MacOSStartFailed)}; err == nil || !slices.Equal(said, want) {
		t.Fatalf("a run whose network failed said %q (%v), want %q", said, err, want)
	}
}

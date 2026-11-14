// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// keptMachine writes a kept machine's folder under m's layout: its kept.json (named when name is given),
// and a manifest naming the disk's logical size. It returns the folder.
func keptMachine(t *testing.T, m *macOSVMs, id uuid.UUID, name string, mutate func(*macvm.KeptRecord)) string {
	t.Helper()
	dir := m.layout.KeptMachineDir(id.String())
	rec := macvm.KeptRecord{ID: id.String(), Name: name, OwnerHash: "owner", RunnerID: m.runnerID, Base: "Standard",
		CreatedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 12, 1, 12, 0, 0, 0, time.Local), Scrubbed: true}
	if mutate != nil {
		mutate(&rec)
	}
	if err := macvm.WriteKept(dir, rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, macvm.ManifestFile), []byte(`{"format":2,"state":"ready","disk_bytes":`+"53687091200"+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func keptRig(t *testing.T) *macOSVMs {
	t.Helper()
	was := keptLockWait
	keptLockWait = 20 * time.Millisecond
	t.Cleanup(func() { keptLockWait = was })
	return newMacLedgerRig(t).open(t)
}

// The list shows each machine's name, and the key of one whose record holds none; the key is its folder.
func TestMachinesListShowsTheNameElseTheKey(t *testing.T) {
	m := keptRig(t)
	named, bare := uuid.New(), uuid.New()
	keptMachine(t, m, named, "dev", func(r *macvm.KeptRecord) {
		r.LastStop, r.StopKind = time.Date(2026, 11, 1, 18, 2, 0, 0, time.Local), macvm.KeptStopGraceful
	})
	keptMachine(t, m, bare, "", func(r *macvm.KeptRecord) { r.Scrubbed = false })
	rows, err := m.keptRows()
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %d, err %v", len(rows), err)
	}
	var out bytes.Buffer
	writeKeptList(&out, rows)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || strings.Fields(lines[0])[0] != "NAME" || !strings.HasSuffix(lines[0], "IN USE") {
		t.Fatalf("list:\n%s", out.String())
	}
	var dev, unnamed []string
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if f[0] == "dev" {
			dev = f
		} else {
			unnamed = f
		}
	}
	if dev == nil || unnamed == nil {
		t.Fatalf("no row for the named machine or for the key:\n%s", out.String())
	}
	if want := macvm.RunKey(named.String()); dev[1] != want {
		t.Errorf("dev's ID = %q, want its key %q", dev[1], want)
	}
	if want := macvm.RunKey(bare.String()); unnamed[0] != want || unnamed[1] != want {
		t.Errorf("a machine with no name lists %q, %q; want its key %q for both", unnamed[0], unnamed[1], want)
	}
	// From the right: IN USE, "GB", the written number, EXPIRES, SCRUBBED. The files are sparse, so 0 GB.
	for name, c := range map[string]struct {
		f        []string
		scrubbed string
	}{"dev": {dev, "yes"}, "unnamed": {unnamed, "no"}} {
		n := len(c.f)
		if c.f[n-1] != "no" || c.f[n-2] != "GB" || c.f[n-4] != "2026-12-01" || c.f[n-5] != c.scrubbed || c.f[2] != "Standard" {
			t.Errorf("%s's row = %v; want base Standard, scrubbed %s, expires 2026-12-01, not in use", name, c.f, c.scrubbed)
		}
	}
	if got := strings.Join(dev[3:5], " "); got != "2026-11-01 18:02" || dev[5] != "graceful" {
		t.Errorf("dev's last stop = %v", dev[3:6])
	}
	if unnamed[3] != "-" || unnamed[4] != "-" {
		t.Errorf("a machine that never stopped lists last stop and stop %v", unnamed[3:5])
	}
}

// bootKept records a live VM of m booting the machine id: an admission entry whose owner is this process,
// and the run folder whose run.json names the machine and that entry.
func bootKept(t *testing.T, r *macLedgerRig, m *macOSVMs, id uuid.UUID) (runID string) {
	t.Helper()
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindRun, GuestCPUs: 4, GuestMemory: 8 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3})
	if err != nil {
		t.Fatal(err)
	}
	run := uuid.NewString()
	cfg := macvm.RunConfig{RunID: run, BaseDir: m.layout.BaseDir(0), RunnerID: m.runnerID, LedgerEntry: e.ID,
		Kept: &macvm.KeptRun{ID: id.String(), Mode: macvm.KeptResume}}
	if err := macvm.WriteRunConfig(m.layout.RunDir(run), cfg); err != nil {
		t.Fatal(err)
	}
	return run
}

// A machine a live VM boots lists as in use, with no scrubbed answer, and the run is the one the run folder
// names; a machine whose VM is gone does not.
func TestMachinesListMarksAMachineALiveVMBoots(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	busy, idle := uuid.New(), uuid.New()
	keptMachine(t, m, busy, "busy", nil)
	keptMachine(t, m, idle, "idle", nil)
	run := bootKept(t, r, m, busy)
	if got := m.keptRunsByMachine(); len(got) != 1 || got[busy.String()] != run {
		t.Fatalf("runs by machine = %v, want %s -> %s", got, busy, run)
	}
	rows, _ := m.keptRows()
	var out bytes.Buffer
	writeKeptList(&out, rows)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	byName := map[string][]string{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		byName[f[0]] = f
	}
	if f := byName["busy"]; f[len(f)-1] != "yes" || f[len(f)-5] != "-" {
		t.Errorf("busy lists as %v; want in use yes and scrubbed -", f)
	}
	if f := byName["idle"]; f[len(f)-1] != "no" || f[len(f)-5] != "yes" {
		t.Errorf("idle lists as %v", f)
	}
	r.host.setProcs() // the VM is gone
	if got := m.keptRunsByMachine(); len(got) != 0 {
		t.Fatalf("a gone VM still boots %v", got)
	}
}

// The list says a machine whose record still names a session is in use, even with its run folder gone,
// as the caps do (keptFromFolder): its expiry is the boot's until the sweep's recovery writes the new one.
func TestMachinesListMarksAnUnrecoveredSessionInUse(t *testing.T) {
	m := keptRig(t)
	held, idle := uuid.New(), uuid.New()
	keptMachine(t, m, held, "held", func(r *macvm.KeptRecord) { r.ActiveRun = uuid.NewString() })
	keptMachine(t, m, idle, "idle", nil)
	rows, err := m.keptRows()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writeKeptList(&out, rows)
	byName := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
		f := strings.Fields(l)
		byName[f[0]] = f
	}
	if f := byName["held"]; f[len(f)-1] != "yes" || f[len(f)-5] != "-" {
		t.Errorf("held lists as %v; want in use yes and scrubbed -", f)
	}
	if f := byName["idle"]; f[len(f)-1] != "no" {
		t.Errorf("idle lists as %v; want in use no", f)
	}
}

// Nothing periodic or read-only takes a machine's lock: a lock held by someone else (a boot, a stop write)
// is invisible to the caps, the status and the list, which read live VMs from the run folders. The lock
// probe (macvm.KeptLocked) is flock'd with the helper's boot and the runner's stop write, so one at the
// wrong moment can refuse a boot or drop a write.
func TestCapsStatusAndListNeverTouchTheLock(t *testing.T) {
	m := keptRig(t)
	m.useDriver(&fakeKept{kept: &runnerwire.MacVMKept{LimitBytes: 110 << 30}})
	id := uuid.New()
	dir := keptMachine(t, m, id, "dev", nil)
	unlock, err := macvm.LockKept(dir) // held, with no VM of this runner: a stop write in progress
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	rows, _ := m.keptRows()
	if len(rows) != 1 || rows[0].inUse {
		t.Fatalf("a lock held with no live VM makes the machine in use: %+v", rows)
	}
	st, err := m.status(context.Background())
	if err != nil || st.Kept == nil || st.Kept.Machines != 1 {
		t.Fatalf("status: %v, %+v", err, st.Kept)
	}
	if c := m.build(nil); !c.KeptMachines || c.Kept == nil {
		t.Fatalf("caps: %+v", c)
	}
	// Nothing in the runner names the probe: what must know a machine is free waits for its lock instead
	// (macvm.LockKeptWait).
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "macvm.KeptLocked(") {
			t.Errorf("%s probes a machine's lock", f)
		}
	}
}

func TestMachinesListWithNoneSaysSo(t *testing.T) {
	m := keptRig(t)
	rows, err := m.keptRows()
	var out bytes.Buffer
	writeKeptList(&out, rows)
	if err != nil || out.String() != "no kept machines\n" {
		t.Fatalf("err %v, list %q", err, out.String())
	}
}

// Only a folder whose kept.json names it is a machine: a lock, a staging folder and a stray record are not.
func TestMachinesIgnoresWhatIsNotAMachine(t *testing.T) {
	m := keptRig(t)
	id := uuid.New()
	keptMachine(t, m, id, "dev", nil)
	kd := m.layout.KeptDir()
	_ = os.MkdirAll(filepath.Join(kd, macvm.RunKey(id.String())+".update"), 0o700)
	// A folder keyed for another id, and one with no record.
	other := uuid.New()
	_ = os.MkdirAll(filepath.Join(kd, macvm.RunKey(other.String())), 0o700)
	wrong := filepath.Join(kd, "0123456789ab")
	if err := macvm.WriteKept(wrong, macvm.KeptRecord{ID: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	rows, err := m.keptRows()
	if err != nil || len(rows) != 1 || rows[0].name != "dev" {
		t.Fatalf("rows %+v, err %v", rows, err)
	}
}

// A machine is named by its name, its id, its key or the start of its id; an arg that matches two or none
// is refused.
func TestMachinesResolveEveryWayToNameOne(t *testing.T) {
	m := keptRig(t)
	a, _ := uuid.Parse("6a0f2c9e-81d4-4000-8000-000000000001")
	b, _ := uuid.Parse("6a0f2c9e-91d4-4000-8000-000000000002")
	keptMachine(t, m, a, "dev", nil)
	keptMachine(t, m, b, "", nil)
	rows, _ := m.keptRows()
	for arg, want := range map[string]string{"dev": a.String(), a.String(): a.String(), macvm.RunKey(a.String()): a.String(),
		"6a0f2c9e8": a.String(), macvm.RunKey(b.String()): b.String(), "6A0F2C9E9": b.String()} {
		r, err := resolveKept(rows, arg)
		if err != nil || r.rec.ID != want {
			t.Errorf("%q -> %v, %v; want %s", arg, r.rec.ID, err, want)
		}
	}
	for _, arg := range []string{"nope", "6a0f", "abc", ""} {
		if r, err := resolveKept(rows, arg); err == nil {
			t.Errorf("%q resolved to %s", arg, r.rec.ID)
		}
	}
}

// A name two machines share is refused, listing their keys; a key names one of them, and wins over a name
// that is the same text.
func TestMachinesResolveRefusesASharedName(t *testing.T) {
	m := keptRig(t)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	keptMachine(t, m, a, "dev", nil)
	keptMachine(t, m, b, "dev", nil)
	keptMachine(t, m, c, macvm.RunKey(a.String()), nil) // named like a's key
	rows, _ := m.keptRows()
	_, err := resolveKept(rows, "dev")
	ka, kb := macvm.RunKey(a.String()), macvm.RunKey(b.String())
	if err == nil || !strings.HasPrefix(err.Error(), "More than one kept machine is named dev (") || !strings.HasSuffix(err.Error(), "). Use the key.") ||
		!strings.Contains(err.Error(), ka) || !strings.Contains(err.Error(), kb) || strings.Contains(err.Error(), macvm.RunKey(c.String())) {
		t.Fatalf("a shared name: %v", err)
	}
	for _, id := range []uuid.UUID{a, b} {
		if r, err := resolveKept(rows, macvm.RunKey(id.String())); err != nil || r.rec.ID != id.String() {
			t.Errorf("key %s -> %v, %v", macvm.RunKey(id.String()), r.rec.ID, err)
		}
	}
	// c's name is a's key: the key is the exact match first.
	if r, err := resolveKept(rows, ka); err != nil || r.rec.ID != a.String() {
		t.Errorf("a name that is another machine's key took %v, %v", r.rec.ID, err)
	}
}

// A delete removes the machine's folder and its lock file, through macvm.RemoveKept.
func TestMachinesDeleteRemovesFolderAndLock(t *testing.T) {
	m := keptRig(t)
	id := uuid.New()
	dir := keptMachine(t, m, id, "dev", nil)
	other := keptMachine(t, m, uuid.New(), "other", nil)
	rows, _ := m.keptRows()
	r, err := resolveKept(rows, "dev")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := deleteKept(&out, r); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, dir + ".lock"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survives the delete: %v", p, err)
		}
	}
	if _, err := os.Lstat(other); err != nil {
		t.Errorf("another machine went with it: %v", err)
	}
	if want := "deleted kept machine dev (" + macvm.RunKey(id.String()) + "); the organisation's record goes on the next report\n"; out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

// A machine a boot holds is not deleted: the delete takes the machine's lock like every other, so it never
// runs beside a boot.
func TestMachinesDeleteRefusesAMachineABootHolds(t *testing.T) {
	m := keptRig(t)
	dir := keptMachine(t, m, uuid.New(), "dev", nil)
	rows, _ := m.keptRows()
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = deleteKept(&out, rows[0])
	if err == nil || !strings.Contains(err.Error(), "machine dev is running on this Mac") || out.Len() != 0 {
		t.Fatalf("err %v, printed %q", err, out.String())
	}
	if _, serr := os.Stat(filepath.Join(dir, macvm.KeptFile)); serr != nil {
		t.Fatalf("the held machine lost its record: %v", serr)
	}
	unlock()
	if err := deleteKept(&out, rows[0]); err != nil {
		t.Fatalf("after the boot: %v", err)
	}
}

// A holder that lets go within the wait does not refuse a delete or an update; one that does not is a boot.
func TestMachinesWaitBrieflyForTheLock(t *testing.T) {
	m := keptRig(t)
	keptLockWait = 2 * time.Second
	dir := keptMachine(t, m, uuid.New(), "dev", nil)
	rows, _ := m.keptRows()
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The goroutine gets its own copy: the test assigns unlock again below, and the release is not an ordering
	// the race detector sees.
	release := unlock
	go func() { time.Sleep(100 * time.Millisecond); release() }()
	var out bytes.Buffer
	if err := deleteKept(&out, rows[0]); err != nil {
		t.Fatalf("a brief holder refused the delete: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the machine survives: %v", err)
	}
	// A holder that stays is a boot: the refusal comes after the wait, not before.
	dir = keptMachine(t, m, uuid.New(), "boot", nil)
	rows, _ = m.keptRows()
	keptLockWait = 300 * time.Millisecond
	unlock, err = macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	if err := deleteKept(&out, rows[0]); err == nil || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("a held machine: err %v after %v", err, time.Since(start))
	}
}

// An update of a machine that is current says so and starts nothing; --force updates it anyway, naming the
// folder, and a machine a boot holds is refused in the person's words.
func TestMachinesUpdateRunsTheKeptUpdater(t *testing.T) {
	was := keptLockWait
	keptLockWait = 20 * time.Millisecond
	defer func() { keptLockWait = was }()
	k := newKeptToolsRig(t)
	id := uuid.New()
	// Re-home the rig's folder under a machine record that names it.
	if err := os.RemoveAll(k.dir); err != nil {
		t.Fatal(err)
	}
	k.dir = keptMachine(t, k.m, id, "dev", nil)
	man := `{"format":2,"state":"ready","guest_os":"macOS 26.1","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"],"disk_bytes":53687091200}`
	if err := os.WriteFile(filepath.Join(k.dir, macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	k.writeRecord(t, k.m.version)
	rows, _ := k.m.keptRows()
	ctx := context.Background()

	var out bytes.Buffer
	if err := updateKept(ctx, &out, k.u, rows[0], false); err != nil || out.String() != "machine dev: tools are current\n" || len(k.jobs) != 0 {
		t.Fatalf("current: err %v, printed %q, jobs %d", err, out.String(), len(k.jobs))
	}
	out.Reset()
	if err := updateKept(ctx, &out, k.u, rows[0], true); err != nil || len(k.jobs) != 1 {
		t.Fatalf("forced: err %v, jobs %d", err, len(k.jobs))
	}
	if !strings.Contains(out.String(), "Updating this machine's tools") || !strings.HasSuffix(out.String(), "machine dev: tools updated\n") {
		t.Errorf("forced update printed %q", out.String())
	}
	if !containsArg(k.jobs[0].extra, "--"+macvm.FlagForce) || !containsArg(k.jobs[0].extra, k.dir) {
		t.Errorf("forced job = %v, want --force and the machine's folder", k.jobs[0].extra)
	}
	// Behind: an update without --force runs, and carries no --force.
	k.writeRecord(t, "0.9.9")
	out.Reset()
	if err := updateKept(ctx, &out, k.u, rows[0], false); err != nil || len(k.jobs) != 2 || containsArg(k.jobs[1].extra, "--"+macvm.FlagForce) {
		t.Fatalf("behind: err %v, jobs %d", err, len(k.jobs))
	}
	// A boot holds the machine.
	unlock, err := macvm.LockKept(k.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := updateKept(ctx, &out, k.u, rows[0], true); err == nil || !strings.Contains(err.Error(), "machine dev is running on this Mac") || len(k.jobs) != 2 {
		t.Fatalf("held: err %v, jobs %d", err, len(k.jobs))
	}
}

func containsArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// The status line is the design's: count, the most the machines could take, the limit, what is written.
func TestStatusKeptLine(t *testing.T) {
	var out bytes.Buffer
	macOSVMKeptStatus{Machines: 1, WorstBytes: 51 << 30, LimitBytes: 110 << 30, AllocatedBytes: 12<<30 + 5}.writeText(&out)
	if want := "kept machines: 1, up to 51 GB of 110 GB (12 GB written)\n"; out.String() != want {
		t.Fatalf("line %q, want %q", out.String(), want)
	}
	out.Reset()
	macOSVMKeptStatus{Machines: 1, WorstBytes: 51 << 30, AllocatedBytes: 1}.writeText(&out)
	if !strings.HasPrefix(out.String(), "kept machines: off on this Mac (macos_vm_kept_disk_limit_gb is 0); 1 held") {
		t.Fatalf("a limit of 0 prints %q", out.String())
	}
}

// The status counts the machines on disk at their worst case (the disk's size plus a run's reserve) against
// the Mac's limit, and prints the line.
func TestStatusCountsKeptMachinesAtTheirWorstCase(t *testing.T) {
	m := keptRig(t)
	m.kept.limit = 110 << 30
	keptMachine(t, m, uuid.New(), "a", nil)
	keptMachine(t, m, uuid.New(), "b", nil)
	st, err := m.status(context.Background())
	if err != nil || st.Kept == nil {
		t.Fatalf("status: err %v, kept %+v", err, st.Kept)
	}
	want := 2 * admission.RunDiskReservation(50<<30)
	if st.Kept.Machines != 2 || st.Kept.WorstBytes != want || st.Kept.LimitBytes != 110<<30 {
		t.Fatalf("kept = %+v, want 2 machines, worst %d, limit %d", st.Kept, want, int64(110<<30))
	}
	var text bytes.Buffer
	st.writeText(&text)
	if !strings.Contains(text.String(), "kept machines: 2, up to 102 GB of 110 GB (0 GB written)\n") {
		t.Fatalf("status text:\n%s", text.String())
	}
	j, _ := json.Marshal(st)
	if !strings.Contains(string(j), `"kept":{"machines":2,"worst_bytes":`) {
		t.Fatalf("status JSON lacks kept: %s", j)
	}
}

// The kept limit setting: unset is 110 GB, 0 is off, a whole number is that many GB, and anything else is
// refused naming the setting.
func TestKeptDiskLimitSetting(t *testing.T) {
	for in, want := range map[string]int64{"": 110 << 30, " ": 110 << 30, "0": 0, "60": 60 << 30, " 110 ": 110 << 30} {
		if got, err := parseKeptDiskLimit(in); err != nil || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"-1", "abc", "1.5", "110GB", "99999999999999"} {
		if _, err := parseKeptDiskLimit(in); err == nil || !strings.Contains(err.Error(), macOSVMKeptDiskLimitKey) {
			t.Errorf("%q = %v; want a refusal naming the setting", in, err)
		}
	}
}

// openKeptRig opens the rig's runner with the kept limit setting as its caller resolved it.
func openKeptRig(t *testing.T, r *macLedgerRig, setting string) *macOSVMs {
	t.Helper()
	return openSettingsRig(t, r, macOSVMSettings{keptLimit: setting})
}

// openSettingsRig opens the rig's runner with the settings as its caller resolved them.
func openSettingsRig(t *testing.T, r *macLedgerRig, set macOSVMSettings) *macOSVMs {
	t.Helper()
	m := openMacOSVMs(macOSVMSetup{ledgerDir: r.dir, root: r.root, runnerID: "runner-a", version: "1.0.0", uid: 501,
		dockerMemory: 8 * gib, host: r.host, owner: r.owner, fstat: r.fstat, settings: set})
	if m.ledger == nil {
		t.Fatalf("ledger refused: %s", m.refusal)
	}
	return m
}

// The handle's limit is the setting its caller resolved: unset is 110 GB, 0 is off, and one that cannot be
// read keeps no machines on the Mac.
func TestOpenTakesTheKeptLimitItIsGiven(t *testing.T) {
	for setting, want := range map[string]int64{"": 110 << 30, "0": 0, "60": 60 << 30} {
		if m := openKeptRig(t, newMacLedgerRig(t), setting); m.kept.limit != want || m.kept.limitErr != nil {
			t.Errorf("%q: limit %d, err %v; want %d", setting, m.kept.limit, m.kept.limitErr, want)
		}
	}
	if m := openKeptRig(t, newMacLedgerRig(t), "lots"); m.kept.limit != 0 || m.kept.limitErr == nil {
		t.Fatalf("a bad setting: limit %d, err %v", m.kept.limit, m.kept.limitErr)
	}
}

// fakeKept is a driver with kept machines.
type fakeKept struct {
	noDriver
	kept     *runnerwire.MacVMKept
	keptErr  error
	sweeps   int
	erases   int
	eraseErr error
}

func (f *fakeKept) KeptMachines() (*runnerwire.MacVMKept, error) { return f.kept, f.keptErr }
func (f *fakeKept) SweepKeptMachines() error                     { f.sweeps++; return nil }
func (f *fakeKept) EraseKeptMachines(context.Context) error      { f.erases++; return f.eraseErr }

// The caps say a runner whose driver keeps machines keeps them, with the driver's machines; a driver that
// does not leaves both out. A Mac that keeps none (limit 0) reports a limit of 0; a count that fails keeps
// the last one.
func TestCapsCarryKeptMachines(t *testing.T) {
	m := keptRig(t)
	m.useDriver(noDriver{})
	if c := m.build(nil); c.KeptMachines || c.Kept != nil {
		t.Fatalf("a driver with no kept machines reports %v, %+v", c.KeptMachines, c.Kept)
	}
	want := &runnerwire.MacVMKept{LimitBytes: 110 << 30, WorstBytes: 51 << 30, AllocatedBytes: 12 << 30,
		Machines: []runnerwire.MacVMKeptMachine{{ID: uuid.NewString(), Bytes: 12 << 30, Scrubbed: true}}}
	f := &fakeKept{kept: want}
	m.useDriver(f)
	c := m.build(nil)
	if !c.KeptMachines || c.Kept == nil || c.Kept.LimitBytes != want.LimitBytes || len(c.Kept.Machines) != 1 || c.Kept.Machines[0].ID != want.Machines[0].ID {
		t.Fatalf("caps kept = %v, %+v", c.KeptMachines, c.Kept)
	}
	// The kept caps are there before the base is ready too.
	if err := os.RemoveAll(m.layout.BaseDir(0)); err != nil {
		t.Fatal(err)
	}
	if c := m.build(nil); !c.KeptMachines || c.Kept == nil || c.Base.State == runnerwire.MacVMBaseReady {
		t.Fatalf("with no base: kept %v, %+v, base %s", c.KeptMachines, c.Kept, c.Base.State)
	}
	f.kept, m.kept.limit = nil, 0 // the driver reports nothing at a limit of 0
	if c := m.build(nil); !c.KeptMachines || c.Kept == nil || c.Kept.LimitBytes != 0 || len(c.Kept.Machines) != 0 {
		t.Fatalf("a Mac that keeps none reports %v, %+v", c.KeptMachines, c.Kept)
	}
	f.kept, f.keptErr = want, errors.New("read failed")
	if c := m.build(&runnerwire.MacVMCaps{Kept: want}); !c.KeptMachines || c.Kept != want {
		t.Fatalf("a failed count drops the last caps: %+v", c.Kept)
	}
	if c := m.build(nil); !c.KeptMachines || c.Kept != nil {
		t.Fatalf("a failed count with no last caps reports %+v", c.Kept)
	}
}

// A change in the kept machines reaches the organisation as a change of caps.
func TestCapsKeptChangeIsAChange(t *testing.T) {
	m := keptRig(t)
	f := &fakeKept{kept: &runnerwire.MacVMKept{LimitBytes: 110 << 30}}
	m.useDriver(f)
	if !m.recompute() || m.recompute() {
		t.Fatal("the first recompute must change the caps and the second must not")
	}
	f.kept = &runnerwire.MacVMKept{LimitBytes: 110 << 30, Machines: []runnerwire.MacVMKeptMachine{{ID: uuid.NewString()}}}
	if !m.recompute() {
		t.Fatal("a new kept machine was not a change")
	}
}

// The sweep runs once at start, then on every keptSweepEvery-th tick and on no other.
func TestKeptSweepRunsAtStartAndOnItsCadence(t *testing.T) {
	m := keptRig(t)
	f := &fakeKept{}
	m.useDriver(f)
	m.kept.sweepEvery = 3
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run loop returns at once
	m.start(ctx, func() {})
	if f.sweeps != 1 {
		t.Fatalf("%d sweeps at start, want 1", f.sweeps)
	}
	for i := 0; i < 7; i++ {
		m.tick(ctx)
	}
	if f.sweeps != 3 { // ticks 3 and 6
		t.Fatalf("%d sweeps after 7 ticks, want 3 in all", f.sweeps)
	}
}

// With no cadence set the sweep waits the default number of ticks.
func TestKeptSweepDefaultCadence(t *testing.T) {
	m := keptRig(t)
	f := &fakeKept{}
	m.useDriver(f)
	for i := 0; i < macOSVMKeptSweepEvery-1; i++ {
		m.tick(context.Background())
	}
	if f.sweeps != 0 {
		t.Fatalf("%d sweeps before the cadence", f.sweeps)
	}
	m.tick(context.Background())
	if f.sweeps != 1 {
		t.Fatalf("%d sweeps at the cadence, want 1", f.sweeps)
	}
}

// A runner the organisation revoked erases its kept machines; a failed erase is retried by the sweep
// instead of the sweep's own work, until it works. A handle or driver without kept machines erases none.
func TestEraseKeptOnRevokeRetriesOnTheSweep(t *testing.T) {
	var nilHandle *macOSVMs
	if err := nilHandle.eraseKept(context.Background()); err != nil {
		t.Fatal(err)
	}
	nilHandle.sweepKept(context.Background())
	m := keptRig(t)
	m.useDriver(noDriver{})
	if err := m.eraseKept(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &fakeKept{eraseErr: errors.New("busy")}
	m.useDriver(f)
	if err := m.eraseKept(context.Background()); err == nil || f.erases != 1 {
		t.Fatalf("erase err %v after %d calls", err, f.erases)
	}
	m.sweepKept(context.Background())
	if f.erases != 2 || f.sweeps != 0 {
		t.Fatalf("a failed erase: %d erases and %d sweeps on the next sweep, want 2 and 0", f.erases, f.sweeps)
	}
	f.eraseErr = nil
	m.sweepKept(context.Background())
	m.sweepKept(context.Background())
	if f.erases != 3 || f.sweeps != 1 {
		t.Fatalf("after the erase worked: %d erases and %d sweeps, want 3 and 1", f.erases, f.sweeps)
	}
}

// macos_vm_kept_disk_limit_gb is a runner setting: runnerd.json accepts it (and refuses a bad value), the
// managed profile maps MacOSVMKeptDiskLimit, the managed value outranks the file, and "0" is a value (off),
// never unset. No flag or environment variable sets it.
func TestKeptDiskLimitIsARunnerSetting(t *testing.T) {
	limit := func(in runnerInputs) (string, int64) {
		f := resolveRunnerConfig(in).field(macOSVMKeptDiskLimitKey)
		n, err := parseKeptDiskLimit(f.Value)
		if err != nil {
			t.Fatalf("%q: %v", f.Value, err)
		}
		return f.Source, n
	}
	if src, n := limit(inputs(nil, nil, nil, runnerFile{})); n != 110<<30 || src != sourceDefault {
		t.Errorf("unset = %d from %q, want 110 GB from the default", n, src)
	}
	if src, n := limit(inputs(nil, nil, nil, runnerFile{MacOSVMKeptDiskLimit: "60"})); n != 60<<30 || !strings.HasPrefix(src, sourceFile) {
		t.Errorf("file 60 = %d from %q", n, src)
	}
	if src, n := limit(inputs(nil, nil, nil, runnerFile{MacOSVMKeptDiskLimit: "0"})); n != 0 || !strings.HasPrefix(src, sourceFile) {
		t.Errorf("file 0 = %d from %q: 0 must be off, not unset", n, src)
	}
	if src, n := limit(inputs(nil, nil, map[string]string{"MacOSVMKeptDiskLimit": "0"}, runnerFile{MacOSVMKeptDiskLimit: "60"})); n != 0 || !strings.HasPrefix(src, sourceManaged) {
		t.Errorf("managed 0 over file 60 = %d from %q", n, src)
	}
	if src, n := limit(inputs(nil, nil, map[string]string{"MacOSVMKeptDiskLimit": "40"}, runnerFile{})); n != 40<<30 || !strings.HasPrefix(src, sourceManaged) {
		t.Errorf("managed 40 = %d from %q", n, src)
	}
	// Through the file decode: the key is known to the strict decoder, and a bad value is refused naming it.
	write := func(body string) (runnerFile, error) {
		p := filepath.Join(t.TempDir(), "runnerd.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadRunnerFile(p, true)
	}
	for body, want := range map[string]string{`{"macos_vm_kept_disk_limit_gb":"0"}`: "0", `{"macos_vm_kept_disk_limit_gb":"60"}`: "60", `{}`: ""} {
		f, err := write(body)
		if err != nil || f.MacOSVMKeptDiskLimit != want {
			t.Errorf("%s: %q, %v", body, f.MacOSVMKeptDiskLimit, err)
		}
	}
	for _, body := range []string{`{"macos_vm_kept_disk_limit_gb":"-5"}`, `{"macos_vm_kept_disk_limit_gb":"lots"}`, `{"macos_vm_kept_disk_limit_gb":"1.5"}`} {
		if _, err := write(body); err == nil || !strings.Contains(err.Error(), macOSVMKeptDiskLimitKey) {
			t.Errorf("%s: err %v, want a refusal naming the setting", body, err)
		}
	}
}

// The limit serve gives the handle is the one in the file serve was told to read (--config), not the default
// file's: an explicit file's 0 (off) or smaller budget beats a default file's, and an explicit file with
// none leaves the default (110), whatever the default file says.
func TestKeptDiskLimitFollowsTheConfigFileServeReads(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("HOME", cfgHome)
	defPath, err := defaultRunnerConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	explicit := filepath.Join(t.TempDir(), "runner.json")
	openServe := func() *macOSVMs {
		t.Helper()
		cfg, err := serveConfig(newServeCommand(nil), explicit)
		if err != nil {
			t.Fatal(err)
		}
		var org, state string
		var opts serveOptions
		cfg.applyTo(&org, &state, &opts)
		return openSettingsRig(t, newMacLedgerRig(t), opts.macOSVMSettings)
	}
	limitOf := func() int64 { return openServe().kept.limit }
	body := func(v string) string { return `{"macos_vm_kept_disk_limit_gb":"` + v + `"}` }
	write(defPath, body("60"))
	write(explicit, body("0"))
	if n := limitOf(); n != 0 {
		t.Errorf("explicit 0 over a default file's 60 = %d", n)
	}
	write(defPath, body("0"))
	write(explicit, body("40"))
	if n := limitOf(); n != 40<<30 {
		t.Errorf("explicit 40 over a default file's 0 = %d", n)
	}
	write(explicit, `{}`)
	if n := limitOf(); n != 110<<30 {
		t.Errorf("explicit file with none over a default file's 0 = %d, want 110 GB", n)
	}
}

// The install list (macos_vm_base_file) and the other bases' folder (macos_vm_bases_dir) are the ones in
// the file serve was told to read, not the default file's.
func TestBaseSettingsFollowTheConfigFileServeReads(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("HOME", cfgHome)
	defPath, err := defaultRunnerConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	explicit := filepath.Join(t.TempDir(), "runner.json")
	write(defPath, `{"macos_vm_base_file":"/default/items.json","macos_vm_bases_dir":"/default/bases"}`)
	write(explicit, `{"macos_vm_base_file":"/explicit/items.json","macos_vm_bases_dir":"/explicit/bases"}`)
	open := func() *macOSVMs {
		cfg, err := serveConfig(newServeCommand(nil), explicit)
		if err != nil {
			t.Fatal(err)
		}
		var org, state string
		var opts serveOptions
		cfg.applyTo(&org, &state, &opts)
		return openSettingsRig(t, newMacLedgerRig(t), opts.macOSVMSettings)
	}
	m := open()
	if m.items.file != "/explicit/items.json" || m.items.err != nil || m.bases.dir != "/explicit/bases" || m.bases.err != nil {
		t.Fatalf("items %+v, bases dir %q: want the explicit file's, not the default's", m.items, m.bases.dir)
	}
	write(explicit, `{}`)
	if m := open(); m.items.file != "" || m.bases.dir != "" {
		t.Fatalf("an explicit file with none still has the default file's: %q, %q", m.items.file, m.bases.dir)
	}
}

// keptFolderAt writes a kept machine folder under root's VM layout, as an earlier run of macOS VMs left it.
func keptFolderAt(t *testing.T, root string) (id uuid.UUID, dir string) {
	t.Helper()
	l := macvm.Layout{Root: root}
	id = uuid.New()
	dir = l.KeptMachineDir(id.String())
	if err := macvm.WriteKept(dir, macvm.KeptRecord{ID: id.String(), OwnerHash: "o", RunnerID: "runner-a"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+".update", 0o700); err != nil {
		t.Fatal(err)
	}
	return id, dir
}

// A revoked runner erases its kept machines with macOS VMs off (no handle) and with a handle that has no
// driver; a machine a boot holds is reported as not erased, and the rest go.
func TestEraseKeptWithMacOSVMsOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root, err := macvm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	was := keptLockWait
	keptLockWait = 20 * time.Millisecond
	defer func() { keptLockWait = was }()
	var off *macOSVMs
	_, a := keptFolderAt(t, root)
	_, b := keptFolderAt(t, root)
	if err := off.eraseKept(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a, a + ".lock", a + ".update", b, b + ".lock", b + ".update"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survives the erase: %v", p, err)
		}
	}
	// A handle with no driver erases from its own layout.
	m := keptRig(t)
	_, c := keptFolderAt(t, m.layout.Root)
	if err := m.eraseKept(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(c); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a handle with no driver left %s: %v", c, err)
	}
	// A held machine is reported, kept with its lock, and does not stop the others going.
	_, held := keptFolderAt(t, root)
	_, free := keptFolderAt(t, root)
	unlock, err := macvm.LockKept(held)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = off.eraseKept(context.Background())
	if err == nil || !strings.Contains(err.Error(), filepath.Base(held)) {
		t.Fatalf("erase with a machine held: %v", err)
	}
	if _, err := os.Lstat(held); err != nil {
		t.Errorf("the held machine was removed: %v", err)
	}
	if _, err := os.Lstat(free); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a free machine survives: %v", err)
	}
}

// A Mac whose limit is 0 keeps no new machines but still reports the ones it holds, so they can be seen,
// deleted and expired: the caps list them with the limit 0, the status counts them, and a delete works.
func TestLimitZeroDoesNotHideMachines(t *testing.T) {
	r := newMacLedgerRig(t)
	m := openKeptRig(t, r, "0")
	if m.kept.limit != 0 {
		t.Fatalf("limit %d", m.kept.limit)
	}
	m.useDriver(&fakeKept{kept: nil}) // the driver reports nothing at a limit of 0
	idle, busy := uuid.New(), uuid.New()
	dir := keptMachine(t, m, idle, "idle", nil)
	keptMachine(t, m, busy, "busy", nil)
	run := bootKept(t, r, m, busy)
	c := m.build(nil)
	if !c.KeptMachines || c.Kept == nil || c.Kept.LimitBytes != 0 || len(c.Kept.Machines) != 2 {
		t.Fatalf("caps at limit 0: %v, %+v", c.KeptMachines, c.Kept)
	}
	for _, km := range c.Kept.Machines {
		switch km.ID {
		case idle.String():
			if km.InUse || km.RunID != "" {
				t.Errorf("idle machine: %+v", km)
			}
		case busy.String():
			if !km.InUse || km.RunID != run {
				t.Errorf("busy machine: %+v, want run %s", km, run)
			}
		default:
			t.Errorf("unknown machine %+v", km)
		}
	}
	if c.Kept.WorstBytes != 2*admission.RunDiskReservation(50<<30) {
		t.Errorf("worst %d", c.Kept.WorstBytes)
	}
	st, err := m.status(context.Background())
	if err != nil || st.Kept == nil || st.Kept.Machines != 2 || st.Kept.LimitBytes != 0 {
		t.Fatalf("status at limit 0: %v, %+v", err, st.Kept)
	}
	rows, _ := m.keptRows()
	var gone keptRow
	for _, r := range rows {
		if r.name == "idle" {
			gone = r
		}
	}
	var out bytes.Buffer
	if err := deleteKept(&out, gone); err != nil {
		t.Fatalf("delete at limit 0: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted machine survives: %v", err)
	}
	if c := m.build(nil); c.Kept == nil || len(c.Kept.Machines) != 1 {
		t.Errorf("caps after the delete: %+v", c.Kept)
	}
}

// The caps read from the kept folder never offer a record that still names a session as idle, even with
// its run folder gone: its expiry is the boot's until the sweep's recovery writes the new one.
func TestKeptFromFolderHoldsAnUnrecoveredSession(t *testing.T) {
	m := keptRig(t)
	held, idle := uuid.New(), uuid.New()
	run := uuid.NewString()
	keptMachine(t, m, held, "held", func(r *macvm.KeptRecord) { r.ActiveRun = run })
	keptMachine(t, m, idle, "idle", nil)
	got := map[string]runnerwire.MacVMKeptMachine{}
	for _, k := range m.keptFromFolder().Machines {
		got[k.ID] = k
	}
	if k := got[held.String()]; !k.InUse || k.RunID != run {
		t.Fatalf("a record naming an unrecovered session: %+v", k)
	}
	if k := got[idle.String()]; k.InUse || k.RunID != "" {
		t.Fatalf("a machine no session holds: %+v", k)
	}
}

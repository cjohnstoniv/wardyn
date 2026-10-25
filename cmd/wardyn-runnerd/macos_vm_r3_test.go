// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// helperFlags parses a job argv as the helper's own job flag set does (cmd/wardyn-macvm newJobFlags,
// built from the same macvm constants): a flag name runnerd made up fails here, as it would there.
func helperFlags(t *testing.T, argv []string) (sub string, set map[string]string) {
	t.Helper()
	if len(argv) == 0 {
		t.Fatal("no argv")
	}
	fs := flag.NewFlagSet(argv[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, n := range []string{macvm.FlagRoot, macvm.FlagRunnerID, macvm.FlagLedgerEntry, macvm.FlagLedgerDir, macvm.FlagSockRoot} {
		fs.String(n, "", "")
	}
	fs.Int(macvm.FlagSlot, 0, "")
	fs.Int(macvm.FlagCPUs, 0, "")
	fs.Uint64(macvm.FlagMemoryMiB, 0, "")
	switch argv[0] {
	case macvm.PrepareCommand:
		fs.Bool(macvm.FlagKeepIPSW, false, "")
		fs.Bool(macvm.FlagReprepare, false, "")
	case macvm.UpdateToolsCommand:
		fs.Bool(macvm.FlagForce, false, "")
	default:
		t.Fatalf("job command %q is not the helper's", argv[0])
	}
	if err := fs.Parse(argv[1:]); err != nil || fs.NArg() != 0 {
		t.Fatalf("the helper's flags refuse %q: %v", argv, err)
	}
	set = map[string]string{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = f.Value.String() })
	return argv[0], set
}

// R2 M1: the tools update and prepare argv are built from the helper's exported command and flag names
// and parse with its flag set; the drift update is forced (the helper knows no agent version to compare)
// and names the socket root runnerd dials.
func TestMacOSVMJobArgvIsTheHelpersFlags(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	m.sockRoot = "/tmp/wm-test/sock"
	prev, prevEx := prepareClaimWait, excludeFromBackups
	prepareClaimWait, excludeFromBackups = 2*time.Second, func(string) error { return nil }
	t.Cleanup(func() { prepareClaimWait, excludeFromBackups = prev, prevEx })
	var args string
	m.helper, args = fakeHelper(t, 0)
	man, _ := macvm.ReadManifest(m.layout.BaseDir(0))
	_ = m.startToolsUpdate(context.Background(), man)
	_ = m.startPrepare(context.Background(), prepareArgs{helper: m.helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192, keepIPSW: true, newImage: true})
	var jobs [][]string
	for _, c := range calls(t, args) {
		if f := strings.Fields(c); len(f) > 0 && f[0] != "probe" {
			jobs = append(jobs, f)
		}
	}
	if len(jobs) != 2 {
		t.Fatalf("helper jobs %q", jobs)
	}
	sub, set := helperFlags(t, jobs[0])
	if sub != macvm.UpdateToolsCommand || set[macvm.FlagForce] != "" || set[macvm.FlagLedgerDir] != r.dir || set[macvm.FlagSockRoot] != m.sockRoot ||
		set[macvm.FlagRoot] != r.root || set[macvm.FlagLedgerEntry] == "" || set[macvm.FlagMemoryMiB] != "8192" {
		t.Fatalf("tools update %s %v", sub, set)
	}
	sub, set = helperFlags(t, jobs[1])
	if sub != macvm.PrepareCommand || set[macvm.FlagReprepare] != "true" || set[macvm.FlagKeepIPSW] != "true" || set[macvm.FlagCPUs] != "4" {
		t.Fatalf("prepare %s %v", sub, set)
	}
}

// openWithBudget is the runner under test with a host budget of 11 GiB (64 GiB, less 49 for Docker and
// 4 for macOS) and a ready base whose minimum is 4 CPUs and 4 GiB.
func openWithBudget(t *testing.T, r *macLedgerRig) (*macOSVMs, macvm.Manifest) {
	t.Helper()
	m := openMacOSVMs(macOSVMSetup{ledgerDir: r.dir, root: r.root, runnerID: "runner-a", version: "1.0.0", uid: 501,
		dockerMemory: 49 * gib, host: r.host, owner: r.owner, fstat: r.fstat})
	if m.ledger == nil {
		t.Fatalf("ledger refused: %s", m.refusal)
	}
	base := m.layout.BaseDir(0)
	_ = os.MkdirAll(base, 0o700)
	man := `{"format":1,"state":"ready","guest_os":"macOS 27.0","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":4096,"harnesses":["none"]}`
	if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := macvm.ReadManifest(base)
	if err != nil {
		t.Fatal(err)
	}
	return m, got
}

// R2 L1 + KIND hook: the organisation's per-run limits (runnerwire.RunnerHooks.PerRunLimits) size each
// guest and kick a recompute; (0, 0) is no limit, back to the base's minimum, never a zero-sized guest;
// a limit below the base's minimum offers none and the log names the flag to raise (design 3.10).
func TestMacOSVMPerRunLimitsSizeTheOffer(t *testing.T) {
	r := newMacLedgerRig(t)
	m, man := openWithBudget(t, r)
	var logs strings.Builder
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	defer slog.SetDefault(prev)
	maxAt := func(cpuMillis, memMiB int64) int {
		t.Helper()
		for len(m.kick) > 0 {
			<-m.kick
		}
		m.setPerRunLimits(cpuMillis, memMiB)
		if len(m.kick) != 1 {
			t.Fatalf("limits (%d, %d) kicked no recompute", cpuMillis, memMiB)
		}
		m.recompute()
		return m.current().Max
	}
	if n := maxAt(4000, 6144); n != 1 {
		t.Fatalf("per-run 6 GiB in an 11 GiB budget: max %d, want 1", n)
	}
	if n := maxAt(0, 0); n != 2 || m.guestMemory(man) != man.MinMemoryMiB<<20 {
		t.Fatalf("no per-run limit: max %d, guest %d bytes, want 2 at the base's minimum", n, m.guestMemory(man))
	}
	if n := maxAt(4000, 2048); n != 0 || !strings.Contains(logs.String(), "--per-run-memory") {
		t.Fatalf("per-run memory below the minimum: max %d, log:\n%s", n, logs.String())
	}
	if n := maxAt(2000, 8192); n != 0 || !strings.Contains(logs.String(), "--per-run-cpu") {
		t.Fatalf("per-run CPUs below the minimum: max %d, log:\n%s", n, logs.String())
	}
	if n := maxAt(0, 0); n != 2 {
		t.Fatalf("limits cleared again: max %d, want 2", n)
	}
	var nilVMs *macOSVMs
	nilVMs.setPerRunLimits(4000, 8192) // macOS VMs off: the hook does nothing
}

// An unset per-run limit is (0, 0) on the wire and means no limit, never "no runs": no write can declare
// a zero per-run CPU or memory, AUTH_OK with none declared carries neither field and decodes to (0, 0),
// and the hook then sizes guests at the base's minimum. (The store's NULL-is-nil and the hub's nil-is-0
// are pinned in internal/runner/remote TestHubAuthOKCarriesThePerRunLimits.)
func TestMacOSVMPerRunZeroIsUnsetNotNoRuns(t *testing.T) {
	total := types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384, DiskMiB: 100 << 10}
	for _, per := range []types.RunnerAmounts{{CPUMillis: 0, MemoryMiB: 8192, DiskMiB: 25 << 10}, {CPUMillis: 4000, MemoryMiB: 0, DiskMiB: 25 << 10}} {
		if err := (types.RunnerLimits{RunsAtOnce: 1, Total: total, PerRun: per}).Validate(types.RunnerMaxRunsCeiling); err == nil {
			t.Fatalf("a zero per-run limit %+v validates", per)
		}
	}
	b, _ := json.Marshal(runnerwire.AuthOK{SessionID: "s"})
	if strings.Contains(string(b), "per_run") {
		t.Fatalf("AUTH_OK with no limits declared: %s", b)
	}
	var ok runnerwire.AuthOK
	if err := json.Unmarshal(b, &ok); err != nil || ok.PerRunCPUMillis != 0 || ok.PerRunMemoryMiB != 0 {
		t.Fatalf("decoded %+v %v", ok, err)
	}
	r := newMacLedgerRig(t)
	m, man := openWithBudget(t, r)
	m.setPerRunLimits(4000, 6144)
	m.setPerRunLimits(ok.PerRunCPUMillis, ok.PerRunMemoryMiB)
	if g := m.guestMemory(man); g != man.MinMemoryMiB<<20 {
		t.Fatalf("guest after (0, 0): %d bytes, want the base's minimum", g)
	}
	if why, _, _ := m.belowMinimum(man); why != "" {
		t.Fatalf("(0, 0) read as a limit: %s", why)
	}
}

// D:336: at start-up runnerd deletes a <slot>.update staging dir a crash left, for each slot whose update
// lock it gets without waiting; a slot whose update lock is held (a live update) keeps its dir.
func TestMacOSVMStartupSweepsStagingUnlessAnUpdateHoldsIt(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	for _, slot := range []int{0, 1} {
		if err := os.MkdirAll(filepath.Join(m.layout.BaseDir(slot)+".update", "half"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	held, err := os.OpenFile(m.layout.BaseDir(1)+".update.lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	m.sweepStaging()
	if _, err := os.Lstat(m.layout.BaseDir(0) + ".update"); !os.IsNotExist(err) {
		t.Fatalf("slot 0's leftover staging dir stayed: %v", err)
	}
	if _, err := os.Lstat(m.layout.BaseDir(1) + ".update"); err != nil {
		t.Fatalf("a live update's staging dir was deleted: %v", err)
	}
	if _, err := os.Lstat(m.layout.BaseDir(0)); err != nil {
		t.Fatalf("the base itself went: %v", err)
	}
}

// A base job's stage-2 boot gets its network from runnerd: once the helper's boot listens on the job's
// ctl.sock, the prep network comes up and its relay's descriptor reaches the boot as net_fd, which starts
// its VM; once the helper has gone, the network goes.
func TestMacOSVMHandsTheJobItsNetwork(t *testing.T) {
	r := newMacLedgerRig(t)
	const helperPID = 4343
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"},
		admission.Proc{PID: helperPID, UID: 501, Start: 9, Name: admission.HelperProcess})
	m := r.open(t)
	m.sockRoot, m.sockFstat = macvmtest.SockRoot(t), macvmtest.SockFstat
	prevPoll := jobSockPoll
	jobSockPoll = 10 * time.Millisecond
	t.Cleanup(func() { jobSockPoll = prevPoll })
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindToolsUpdate, GuestCPUs: 4, GuestMemory: 8 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ledger.ClaimHelper(e.ID, helperPID, 9); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	undone := make(chan struct{})
	m.prepNet = func(_ context.Context, id string) (*os.File, func(), error) {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			return nil, nil, err
		}
		_ = unix.Close(fds[1])
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		return os.NewFile(uintptr(fds[0]), "relay"), func() { t.Error("a handed-off network was undone") }, nil
	}
	m.prepRemove = func(_ context.Context, id string) error {
		if id != e.ID {
			t.Errorf("removed the stage-2 network of %s", id)
		}
		close(undone)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()

	eng := &macvmtest.Engine{}
	dir := t.TempDir()
	for _, f := range []string{macvm.DiskFile, macvm.AuxFile} {
		_ = os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	spec := macvm.MachineSpec{CPUs: 4, MemoryBytes: 8 << 30, MinCPUs: 2, MinMemoryBytes: 4 << 30, HardwareModel: []byte("hw"),
		MachineIdentifier: []byte("id"), DiskPath: filepath.Join(dir, macvm.DiskFile), AuxPath: filepath.Join(dir, macvm.AuxFile)}
	boot := macvm.Stage2BootConfig{Engine: eng, SockRoot: m.sockRoot, SockKey: macvm.JobSockKey(e.ID), SockFstat: macvmtest.SockFstat,
		NetWait: 10 * time.Second, GuestWait: 10 * time.Second}
	booted := make(chan error, 1)
	var stop func(context.Context) error
	go func() {
		var err error
		_, stop, err = boot.ToolsBoot()(ctx, spec)
		booted <- err
	}()
	m.recompute() // the recompute starts the job's handler
	m.recompute() // one handler per job
	select {
	case err := <-booted:
		if err != nil {
			t.Fatalf("the stage-2 boot: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stage-2 boot never got its network")
	}
	defer func() { _ = stop(context.Background()) }()
	if ms := eng.Machines(); len(ms) != 1 || ms[0].State() != macvm.StateRunning {
		t.Fatalf("machines %+v", ms)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0] != e.ID {
		t.Fatalf("prep network asked for %q, want once for %s", asked, e.ID)
	}
	mu.Unlock()
	select {
	case <-undone:
		t.Fatal("the prep network went while the helper lives")
	case <-time.After(100 * time.Millisecond):
	}
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	select {
	case <-undone:
	case <-time.After(5 * time.Second):
		t.Fatal("the prep network outlived the job's helper")
	}
}

// DR-0428: `macos-vm status` shows how the last run boot's tools-port dial ended, in both forms, and
// nothing for a record that is not one of the five classes.
func TestMacOSVMStatusShowsTheRunBootToolsDial(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.helper, _ = fakeHelper(t, 0)
	for content, want := range map[string]string{"refused\n": "refused", "listening\n": ""} {
		_ = os.WriteFile(filepath.Join(r.root, runToolsDialFile), []byte(content), 0o600)
		st, err := m.status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		st.writeText(&text)
		js, _ := json.Marshal(st)
		if st.ToolsDial != want || strings.Contains(text.String(), "tools_dial: refused") != (want != "") ||
			strings.Contains(string(js), `"tools_dial":"refused"`) != (want != "") {
			t.Fatalf("record %q: %q\n%s\n%s", content, st.ToolsDial, text.String(), js)
		}
	}
}

// runnerd's start sweeps the stage-2 networks a crash left before any job is handed one, keeping those of
// every job still in the ledger.
func TestMacOSVMStartSweepsLeftoverPrepNetsButLedgeredJobs(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindPrepare, GuestCPUs: 4, GuestMemory: 8 << 30,
		DiskReserve: admission.PrepareDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3})
	if err != nil {
		t.Fatal(err)
	}
	var kept [][]string
	var handing bool
	m.prepSweep = func(_ context.Context, keep []string) (int, error) {
		m.mu.Lock()
		handing = m.ctx != nil
		m.mu.Unlock()
		kept = append(kept, keep)
		return 3, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run loop returns at once
	m.start(ctx, func() {})
	if len(kept) != 1 || !slices.Equal(kept[0], []string{e.ID}) || handing {
		t.Fatalf("swept %d times keeping %v (jobs could be handed a network: %v); want once, keeping %s first", len(kept), kept, handing, e.ID)
	}
}

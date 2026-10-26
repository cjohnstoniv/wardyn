// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const gib = 1 << 30

// macHost is a Mac with settable numbers and process table.
type macHost struct {
	mu    sync.Mutex
	procs []admission.Proc
}

func (h *macHost) BootTime() (int64, error)              { return 1, nil }
func (h *macHost) MemoryBytes() (uint64, error)          { return 64 * gib, nil }
func (h *macHost) AvailableMemoryBytes() (uint64, error) { return 48 * gib, nil }
func (h *macHost) FreeDiskBytes(string) (int64, error)   { return 500 * gib, nil }
func (h *macHost) setProcs(ps ...admission.Proc)         { h.mu.Lock(); h.procs = ps; h.mu.Unlock() }
func (h *macHost) Processes() ([]admission.Proc, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]admission.Proc(nil), h.procs...), nil
}

// macLedgerRig is a ledger directory two simulated logins share. Tests run as one OS user, so owners
// are simulated: the directory and its lock are root's, a file the test assigns is that login's, and any
// other file is login 501's (the runner under test).
type macLedgerRig struct {
	dir, root string
	host      *macHost
	mu        sync.Mutex
	owners    map[string]int
	notRoot   map[string]bool
}

func newMacLedgerRig(t *testing.T) *macLedgerRig {
	t.Helper()
	top := t.TempDir()
	_ = os.Chmod(top, 0o755)
	dir := filepath.Join(top, "macos-vm-admission")
	if err := os.Mkdir(dir, 0o755); err != nil || os.Chmod(dir, os.ModeSticky|0o777) != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerLock(dir), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &macLedgerRig{dir: dir, root: filepath.Join(t.TempDir(), "macos-vm.noindex"), host: &macHost{}, owners: map[string]int{}, notRoot: map[string]bool{}}
	return r
}

// fstat reports every path root's, except those in notRoot: the package's install, simulated.
func (r *macLedgerRig) fstat(fd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st.Uid, st.Gid = 0, 0
	if r.notRoot[name] {
		st.Uid = 501
	}
	return st, nil
}

func (r *macLedgerRig) owner(fi os.FileInfo) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.owners[fi.Name()]; ok {
		return u, true
	}
	return 501, true
}

func (r *macLedgerRig) own(name string, uid int) { r.mu.Lock(); r.owners[name] = uid; r.mu.Unlock() }

// open is the runner under test: login 501's runner "runner-a", with a ready base.
func (r *macLedgerRig) open(t *testing.T) *macOSVMs {
	t.Helper()
	m := openMacOSVMs(macOSVMSetup{ledgerDir: r.dir, root: r.root, runnerID: "runner-a", version: "1.0.0", uid: 501,
		dockerMemory: 8 * gib, host: r.host, owner: r.owner, fstat: r.fstat})
	if m.ledger == nil {
		t.Fatalf("ledger refused: %s", m.refusal)
	}
	base := m.layout.BaseDir(0)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	man := `{"format":1,"state":"ready","guest_os":"macOS 26.1","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"]}`
	if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	return m
}

// other is a second login's ledger (uid 502); its run entry is admitted and assigned to it.
func (r *macLedgerRig) otherRun(t *testing.T, helperPID int) admission.Entry {
	t.Helper()
	l, err := admission.Open(admission.Config{Dir: r.dir, UID: 502, RunnerID: "runner-b", Host: r.host, DockerMemoryBytes: 8 * gib,
		Volume: r.dir, Fstat: r.fstat, FileOwner: func(fi os.FileInfo) int { u, _ := r.owner(fi); return u }})
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Admit(admission.Request{Kind: admission.KindRun, GuestCPUs: 4, GuestMemory: 8 * gib,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: 20, OwnerStart: 2})
	if err != nil {
		t.Fatal(err)
	}
	r.own("e-"+e.ID+".json", 502)
	if _, err := l.Update(e.ID, func(x *admission.Entry) { x.HelperPID, x.HelperStart, x.State = helperPID, 9, admission.StateRunning }); err != nil {
		t.Fatal(err)
	}
	return e
}

// The max the runner offers follows another login's VMs on the recompute, and every change is resent;
// a recompute that changes nothing sends nothing.
func TestMacOSVMMaxResentOnChange(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: 20, UID: 502, Start: 2, Name: "wardyn-runnerd"})
	m := r.open(t)
	m.interval = 10 * time.Millisecond
	if !m.recompute() {
		t.Fatal("the first recompute reported no change")
	}
	if c := m.current(); c.Max != 2 || c.Base.State != runnerwire.MacVMBaseReady || c.MinMemoryMiB != 8192 || c.Harnesses[0] != "claude-code" {
		t.Fatalf("caps before: %+v", c)
	}
	var sent atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.run(ctx, func() { sent.Add(1) })
	time.Sleep(50 * time.Millisecond)
	if n := sent.Load(); n != 0 {
		t.Fatalf("resent %d time(s) with nothing changed", n)
	}
	r.host.setProcs(admission.Proc{PID: 20, UID: 502, Start: 2, Name: "wardyn-runnerd"}, admission.Proc{PID: 90, UID: 502, Start: 9, Name: admission.HelperProcess})
	r.otherRun(t, 90)
	waitFor(t, func() bool { return sent.Load() == 1 && m.current().Max == 1 })
}

// Each recompute first runs the tombstone pass: another login's VM whose helper is gone stops holding
// a slot, and the runner offers it again.
func TestMacOSVMRecomputeTombstones(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: 90, UID: 502, Start: 9, Name: admission.HelperProcess})
	m := r.open(t)
	e := r.otherRun(t, 90)
	m.recompute()
	if c := m.current(); c.Max != 1 {
		t.Fatalf("max %d with the other login's VM live, want 1", c.Max)
	}
	r.host.setProcs() // the other login's helper is gone
	m.recompute()
	if _, err := os.Stat(filepath.Join(r.dir, "e-"+e.ID+".json.tombstone")); err != nil {
		t.Fatalf("no tombstone for the dead VM: %v", err)
	}
	if c := m.current(); c.Max != 2 {
		t.Fatalf("max %d after the tombstone pass, want 2", c.Max)
	}
}

// A prepare or tools-update entry of this runner's whose helper has exited is released by the recompute.
func TestMacOSVMReconcilesFinishedPrepare(t *testing.T) {
	for _, kind := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate} {
		t.Run(string(kind), func(t *testing.T) {
			r := newMacLedgerRig(t)
			r.host.setProcs(admission.Proc{PID: 30, UID: 501, Start: 3, Name: "wardyn-runnerd"})
			m := r.open(t)
			e, err := m.ledger.Admit(admission.Request{Kind: kind, GuestCPUs: 4, GuestMemory: 8 * gib,
				DiskReserve: admission.PrepareDiskReservation(macvm.BaseDiskBytes), OwnerPID: 30, OwnerStart: 3})
			if err != nil {
				t.Fatal(err)
			}
			if kind == admission.KindPrepare && !m.preparing() {
				t.Fatal("a live prepare is not reported")
			}
			r.host.setProcs()
			m.recompute()
			if counted, _ := m.ledger.Counted(); len(counted) != 0 {
				t.Fatalf("finished %s %s still counted: %+v", kind, e.ID, counted)
			}
		})
	}
}

// The runner refuses a ledger the package did not install whole, and never repairs it.
func TestMacOSVMLedgerChecked(t *testing.T) {
	cases := map[string]func(r *macLedgerRig){
		"dir not root's":    func(r *macLedgerRig) { r.notRoot[r.dir] = true },
		"parent not root's": func(r *macLedgerRig) { r.notRoot[filepath.Dir(r.dir)] = true },
		"lock not root's":   func(r *macLedgerRig) { r.notRoot[ledgerLock(r.dir)] = true },
		"no lock":           func(r *macLedgerRig) { _ = os.Remove(ledgerLock(r.dir)) },
		"not sticky":        func(r *macLedgerRig) { _ = os.Chmod(r.dir, 0o777) },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			r := newMacLedgerRig(t)
			damage(r)
			m := openMacOSVMs(macOSVMSetup{ledgerDir: r.dir, root: r.root, runnerID: "runner-a", uid: 501, host: r.host, owner: r.owner, fstat: r.fstat})
			if m.enabled() || m.refusal != macOSVMLedgerDamaged {
				t.Fatalf("enabled %v, refusal %q", m.enabled(), m.refusal)
			}
			m.recompute()
			if c := m.current(); c.Refusal != macOSVMLedgerDamaged || c.Max != 0 {
				t.Fatalf("caps %+v", c)
			}
			if _, err := os.Stat(ledgerLock(r.dir)); name == "no lock" && err == nil {
				t.Fatal("the runner created the lock")
			}
		})
	}
}

func TestMacOSVMArithmetic(t *testing.T) {
	for _, tc := range []struct {
		host, docker, total, perRun uint64
		want                        int
	}{
		{host: 32 * gib, docker: 8 * gib, total: 16 * gib, perRun: 8 * gib, want: 2},
		{host: 24 * gib, docker: 8 * gib, total: 16 * gib, perRun: 8 * gib, want: 1},
		{host: 64 * gib, docker: 16 * gib, total: 8 * gib, perRun: 8 * gib, want: 1},
		{host: 16 * gib, docker: 8 * gib, total: 8 * gib, perRun: 8 * gib, want: 0},
		{host: 128 * gib, docker: 8 * gib, total: 64 * gib, perRun: 8 * gib, want: 2},
	} {
		a := macOSVMArithmetic{host: tc.host, docker: tc.docker, total: tc.total, perRun: tc.perRun}
		lines := strings.Join(a.lines(), "\n")
		if a.max() != tc.want || !strings.Contains(lines, "at most "+string(rune('0'+tc.want))) {
			t.Fatalf("%+v: max %d, want %d\n%s", tc, a.max(), tc.want, lines)
		}
		if strings.Contains(lines, "To run one") != (tc.want == 0) {
			t.Fatalf("%+v: what to change shown only when none fits:\n%s", tc, lines)
		}
	}
}

func TestMacOSVMSettingResolves(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "ON": true, "": false, "0": false, "off": false} {
		if macOSVMOn(v) != want {
			t.Fatalf("macOSVMOn(%q) = %v", v, !want)
		}
	}
	t.Setenv("WARDYN_RUNNER_MACOS_VM", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := serveConfig(newServeCommand(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	var o serveOptions
	var org, state string
	cfg.applyTo(&org, &state, &o)
	if !o.macOSVM {
		t.Fatal("WARDYN_RUNNER_MACOS_VM=1 did not turn macOS VMs on")
	}
}

func TestMacOSVMPrepareProgress(t *testing.T) {
	log := filepath.Join(t.TempDir(), "prepare.log")
	_ = os.WriteFile(log, []byte(`{"phase":"check","fraction":1}`+"\n"+`{"phase":"install","fraction":0.5}`+"\n"), 0o600)
	if p := prepareProgress(log); p != 45 { // (4 + 0.5) of 10 phases
		t.Fatalf("progress %d", p)
	}
	_ = os.WriteFile(log, []byte(`{"phase":"install","fraction":0.5}`+"\nwardyn-macvm: no space left\n"), 0o600)
	if msg := prepareFailure(log); msg != "no space left" {
		t.Fatalf("failure %q", msg)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The free-space check ends the newest macOS VM run each time the VM volume has under 5 GiB free, and
// nothing while it has more.
func TestMacOSVMDiskCheckEndsNewestBelowFloor(t *testing.T) {
	var free atomic.Int64
	free.Store(6 << 30)
	var ended atomic.Int32
	m := &macOSVMs{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watchDisk(ctx, 5*time.Millisecond, func() (int64, error) { return free.Load(), nil },
		func(context.Context, int64) (string, error) { ended.Add(1); return "run-1", nil })
	time.Sleep(50 * time.Millisecond)
	if n := ended.Load(); n != 0 {
		t.Fatalf("ended %d run(s) with 6 GiB free", n)
	}
	free.Store(5<<30 - 1)
	waitFor(t, func() bool { return ended.Load() >= 1 })
}

// remove deletes the bases and the download cache only: the ledger, prepare's log and the VM directory
// (which carries the Time Machine exclusion) stay. It refuses while this runner holds a VM.
func TestMacOSVMRemoveDeletesBasesAndCacheOnly(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	_ = os.MkdirAll(m.layout.CacheDir(), 0o700)
	_ = os.WriteFile(filepath.Join(m.layout.CacheDir(), "restore.ipsw"), []byte("ipsw"), 0o600)
	_ = os.WriteFile(m.layout.PrepareLog(), []byte("{}\n"), 0o600)
	r.host.setProcs(admission.Proc{PID: 30, UID: 501, Start: 3, Name: "wardyn-runnerd"})
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindRun, GuestCPUs: 4, GuestMemory: 8 * gib,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: 30, OwnerStart: 3})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := m.remove(&out); err == nil {
		t.Fatal("removed the base under a running VM")
	}
	if err := m.ledger.Release(e.ID, admission.Destroyed{Stopped: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.remove(&out); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{filepath.Join(r.root, "bases"), m.layout.CacheDir()} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s left: %v", gone, err)
		}
	}
	for _, kept := range []string{r.root, m.layout.PrepareLog(), ledgerLock(r.dir)} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s removed: %v", kept, err)
		}
	}
}

// fakeHelper is a wardyn-macvm that appends each call's arguments to a file and answers probe with
// probeExit; it claims no entry. It answers the args file.
func fakeHelper(t *testing.T, probeExit int) (helper, args string) {
	t.Helper()
	bin := t.TempDir()
	args = filepath.Join(bin, "args")
	helper = filepath.Join(bin, "wardyn-macvm")
	script := "#!/bin/sh\necho \"$@\" >>" + args + "\nif [ \"$1\" = probe ]; then echo '{\"host\":\"ok\",\"provisioning\":\"this build has no guest provisioning\",\"thermal\":\"nominal\"}'; exit " +
		strconv.Itoa(probeExit) + "; fi\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return helper, args
}

// calls is the fake helper's calls so far, one line each.
func calls(t *testing.T, args string) []string {
	t.Helper()
	b, _ := os.ReadFile(args)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// prepare runs the helper's probe, then excludes the VM directory from Time Machine and runs the helper's
// prepare with this runner's entry and --keep-ipsw passed through; a helper that ends without claiming
// the entry frees it. On a ready base, plain prepare starts nothing, and --new-image re-prepares.
func TestMacOSVMPrepareExcludesAndPassesKeepIPSW(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	var excluded []string
	prevEx, prevWait := excludeFromBackups, prepareClaimWait
	excludeFromBackups = func(dir string) error { excluded = append(excluded, dir); return nil }
	prepareClaimWait = 2 * time.Second
	t.Cleanup(func() { excludeFromBackups, prepareClaimWait = prevEx, prevWait })
	helper, args := fakeHelper(t, 0)
	a := prepareArgs{helper: helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192, keepIPSW: true}
	if err := m.startPrepare(context.Background(), a); !errors.Is(err, errMacOSBaseReady) {
		t.Fatalf("plain prepare on a ready base: %v", err)
	}
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Fatalf("plain prepare on a ready base ran the helper: %q", calls(t, args))
	}
	for _, newImage := range []bool{true, false} {
		if !newImage {
			_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile))
		}
		_ = os.Remove(args)
		a.newImage = newImage
		err := m.startPrepare(context.Background(), a)
		if err == nil || !strings.Contains(err.Error(), "ended at once") {
			t.Fatalf("new image %v: prepare with a helper that claims nothing: %v", newImage, err)
		}
		got := calls(t, args)
		if len(got) != 2 || got[0] != "probe" {
			t.Fatalf("new image %v: helper calls %q, want probe then prepare", newImage, got)
		}
		fields := strings.Fields(got[1])
		for _, want := range []string{"prepare", "--keep-ipsw", "--runner-id", "--ledger-entry", "--cpus", "--memory-mib"} {
			if !slices.Contains(fields, want) {
				t.Fatalf("helper args %q lack %s", fields, want)
			}
		}
		if slices.Contains(fields, "--reprepare") != newImage {
			t.Fatalf("new image %v: helper args %q", newImage, fields)
		}
	}
	if len(excluded) != 2 || excluded[0] != r.root {
		t.Fatalf("Time Machine exclusion %v, want [%s %s]", excluded, r.root, r.root)
	}
	if counted, _ := m.ledger.Counted(); len(counted) != 0 {
		t.Fatalf("an unclaimed prepare entry stayed: %+v", counted)
	}
}

// status never shows the one-time setup code, in text or JSON, whatever prepare.log holds: it names the
// owner-only file the helper left it in, and a file anyone else could read is not named. Nothing logs it.
// The JSON's keys are pinned.
func TestMacOSVMStatusNeverShowsTheSetupCode(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.helper, _ = fakeHelper(t, 0)
	man := `{"format":1,"state":"ready","guest_os":"macOS 27.0","build_version":"26A428","min_cpus":4,"min_memory_mib":8192,"desktop":"observe","harnesses":["none"]}`
	_ = os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile), []byte(man), 0o600)
	const code = "K7MP2QXR"
	_ = os.WriteFile(m.layout.PrepareLog(), []byte(`{"phase":"install","fraction":1}`+"\n"+`{"phase":"setup_by_hand","detail":"`+code+`"}`+"\n"), 0o600)
	codeFile := macvm.Layout{Root: r.root}.SetupCode()
	if filepath.Base(codeFile) != "setup-code.json" { // design D:1306
		t.Fatalf("setup code file %s", codeFile)
	}
	// doctor reads the login's own VM directory: point HOME at one that holds the same file.
	t.Setenv("HOME", t.TempDir())
	docRoot, err := macvm.DefaultRoot()
	if err != nil || os.MkdirAll(docRoot, 0o700) != nil {
		t.Fatal(err)
	}
	docFile := macvm.Layout{Root: docRoot}.SetupCode()
	var logs strings.Builder
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	for _, mode := range []os.FileMode{0o644, 0o600} {
		for _, f := range []string{codeFile, docFile} {
			_ = os.WriteFile(f, []byte(`{"code":"`+code+`"}`+"\n"), mode)
			_ = os.Chmod(f, mode)
		}
		st, err := m.status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var text, docText, docJSON strings.Builder
		st.writeText(&text)
		js, _ := json.Marshal(st)
		rep := doctor.Report{Stages: macOSVMDoctorStages()}
		_ = rep.WriteText(&docText)
		_ = rep.WriteJSON(&docJSON)
		if named := strings.Contains(docText.String(), docFile); named != (mode == 0o600) {
			t.Fatalf("mode %o: doctor names the code file %v:\n%s", mode, named, docText.String())
		}
		for _, out := range []string{text.String(), string(js), docText.String(), docJSON.String()} {
			if strings.Contains(out, code) {
				t.Fatalf("mode %o: the one-time code is in the status:\n%s", mode, out)
			}
		}
		if named := strings.Contains(text.String(), "with the one-time code in "+codeFile); named != (mode == 0o600) {
			t.Fatalf("mode %o: code file named %v:\n%s", mode, named, text.String())
		}
		if !strings.Contains(text.String(), "desktop: view only") || st.Thermal != "nominal" {
			t.Fatalf("mode %o: status %+v\n%s", mode, st, text.String())
		}
		var keys map[string]any
		_ = json.Unmarshal(js, &keys)
		got := slices.Sorted(maps.Keys(keys))
		want := []string{"base", "desktop", "error", "harnesses", "held_disk_bytes", "identity", "leaked", "max", "phase", "state", "thermal", "tools_dial"}
		if !slices.Equal(got, want) {
			t.Fatalf("status JSON keys %v, want %v", got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), code) {
		t.Fatalf("the one-time code was logged:\n%s", logs.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// setup's --macos-vm: on or off, on only on an Apple silicon Mac, kept in runnerd.json; when on, setup
// shows the memory sum and the max it gives with the limits just registered.
func TestMacOSVMSetupFlagAndSum(t *testing.T) {
	prevHost, prevMem, prevMeasure := macOSVMHost, macHostMemory, measureHost
	t.Cleanup(func() { macOSVMHost, macHostMemory, measureHost = prevHost, prevMem, prevMeasure })
	macOSVMHost = func(string) bool { return false }
	if checkMacOSVMFlag("maybe", "darwin") == nil || checkMacOSVMFlag("on", "linux") == nil || checkMacOSVMFlag("off", "linux") != nil {
		t.Fatal("--macos-vm checks")
	}
	macOSVMHost = func(string) bool { return true }
	if checkMacOSVMFlag("on", "darwin") != nil {
		t.Fatal("--macos-vm on refused on an Apple silicon Mac")
	}
	for v, want := range map[string]string{"on": "1", "off": ""} {
		p := setupPlan{setupOptions: setupOptions{macOSVM: v}, existing: runnerFile{MacOSVM: "1"}}
		if got := p.runnerFile().MacOSVM; got != want {
			t.Fatalf("--macos-vm %s wrote macos_vm %q", v, got)
		}
	}
	if p := (setupPlan{existing: runnerFile{MacOSVM: "1"}}); p.runnerFile().MacOSVM != "1" {
		t.Fatal("a rerun without --macos-vm dropped the earlier answer")
	}
	macHostMemory = func() (uint64, error) { return 32 * gib, nil }
	measureHost = func(context.Context) (types.RunnerAmounts, error) {
		return types.RunnerAmounts{MemoryMiB: 8 << 10}, nil
	}
	cmd := newSetupCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	plan := setupPlan{setupOptions: setupOptions{macOSVM: "on"}}
	limits := &types.RunnerLimits{Total: types.RunnerAmounts{MemoryMiB: 16 << 10}, PerRun: types.RunnerAmounts{MemoryMiB: 8 << 10}}
	if err := askMacOSVM(cmd, "darwin", &plan, limits); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "So this Mac runs at most 2 macOS VM(s) at once") {
		t.Fatalf("setup's sum:\n%s", out.String())
	}
}

// ledgerLock is the ledger's lock, beside its directory.
func ledgerLock(dir string) string { return filepath.Join(filepath.Dir(dir), "admission.lock") }

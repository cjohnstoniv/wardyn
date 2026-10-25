// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// The macOS VM tests drive the driver against the helper's real run loop (macvm.Run) on the fake
// Virtualization engine, in this process, with a fake host process table, a ledger directory two
// simulated logins share, the fake Docker engine (whose relay attach is a real socket) and a guest
// agent answering on the fake machine's vsock.

const macHelperPID = 4242

type macTestHost struct {
	mu    sync.Mutex
	procs map[int]admission.Proc
	// freeDisk is the volume's free space; zero is 500 GiB.
	freeDisk int64
	// onProcesses runs once, at the next process-table read (the next ledger operation).
	onProcesses func()
}

func (h *macTestHost) BootTime() (int64, error)              { return 1, nil }
func (h *macTestHost) MemoryBytes() (uint64, error)          { return 64 << 30, nil }
func (h *macTestHost) AvailableMemoryBytes() (uint64, error) { return 48 << 30, nil }
func (h *macTestHost) FreeDiskBytes(string) (int64, error)   { return cmp.Or(h.freeDisk, 500<<30), nil }
func (h *macTestHost) Processes() ([]admission.Proc, error) {
	h.mu.Lock()
	fn := h.onProcesses
	h.onProcesses = nil
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]admission.Proc, 0, len(h.procs))
	for _, p := range h.procs {
		out = append(out, p)
	}
	return out, nil
}
func (h *macTestHost) set(p admission.Proc) { h.mu.Lock(); h.procs[p.PID] = p; h.mu.Unlock() }
func (h *macTestHost) gone(pid int)         { h.mu.Lock(); delete(h.procs, pid); h.mu.Unlock() }

// macDocker is the fake engine with the relay's attach a real stream socket, as the engine's is.
type macDocker struct {
	*fakeDocker
	mu    sync.Mutex
	relay []net.Conn // the relay's ends, held open as the relay would
}

func (m *macDocker) ContainerAttach(ctx context.Context, id string, opts client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	m.fakeDocker.mu.Lock()
	c := m.fakeDocker.containers[id]
	m.fakeDocker.mu.Unlock()
	if c == nil || !strings.HasPrefix(c.name, "wardyn-vmnet-") { // a run's relay, or a base job's (StartMacPrepNet)
		return m.fakeDocker.ContainerAttach(ctx, id, opts)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return client.ContainerAttachResult{}, err
	}
	conn := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "attach")
		defer f.Close()
		c, _ := net.FileConn(f)
		return c
	}
	engineSide, relaySide := conn(fds[0]), conn(fds[1])
	m.mu.Lock()
	m.relay = append(m.relay, relaySide)
	m.mu.Unlock()
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(engineSide, "application/vnd.docker.raw-stream")}, nil
}

type macRig struct {
	f      *macDocker
	d      *Driver
	c      *MacVMConfig
	host   *macTestHost
	eng    *macvmtest.Engine
	ledger string
	mu     sync.Mutex
	owners map[string]int
	init   *guestagent.Init
	// secret is the run's desktop password the helper serves on ctl.sock (nil: none); setPassword is
	// whether the guest says it set it.
	secret      *macvm.DesktopSecret
	setPassword bool
	runs        sync.WaitGroup
	stop        context.CancelFunc
}

func newMacRig(t *testing.T) *macRig {
	t.Helper()
	root, err := os.MkdirTemp("", "mvt") // short: unix socket paths are bounded
	if err != nil {
		t.Fatal(err)
	}
	ledgerDir := filepath.Join(root, "ledger")
	if err := os.Mkdir(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(ledgerDir, os.ModeSticky|0o777)
	if err := os.WriteFile(filepath.Join(root, "admission.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &macRig{f: &macDocker{fakeDocker: newFakeDocker()}, host: &macTestHost{procs: map[int]admission.Proc{}}, eng: &macvmtest.Engine{},
		ledger: ledgerDir, owners: map[string]int{}, setPassword: true}
	r.host.set(admission.Proc{PID: os.Getpid(), UID: 501, Start: 1, Name: "wardyn-runnerd"})
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	t.Cleanup(func() {
		cancel()
		r.runs.Wait()
		r.f.mu.Lock()
		for _, c := range r.f.relay {
			_ = c.Close()
		}
		r.f.mu.Unlock()
		_ = os.RemoveAll(root)
	})
	r.d = newWithClient(r.f, Config{ProxyImage: "wardyn-proxy:dev"})
	layout := macvm.Layout{Root: filepath.Join(root, "vm")}
	makeMacBase(t, layout)
	r.c = &MacVMConfig{Layout: layout, Helper: "/nonexistent/wardyn-macvm", Identity: macvm.IdentityFresh, Ledger: r.open(t, 501, "runner-a"),
		Host: r.host, RunnerID: "runner-a", HostOS: "macOS 26.1 (25B78)", BootTimeout: 20 * time.Second,
		SockRoot: macvmtest.SockRoot(t), ToolsDialFile: filepath.Join(layout.Root, "run-tools-dial"), sockFstat: macvmtest.SockFstat}
	r.c.sweepSock = sweepSockForTest
	r.c.spawn = func(dir string) error { r.helper(ctx, dir); return nil }
	r.c.signal = func(int, unix.Signal) error { cancel(); return nil }
	if r.secret, err = macvm.NewDesktopSecret(); err != nil {
		t.Fatal(err)
	}
	if err := r.d.EnableMacVM(r.c); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *macRig) owner(fi os.FileInfo) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.owners[fi.Name()]; ok {
		return u
	}
	return 501
}

func (r *macRig) open(t *testing.T, uid int, runnerID string) *admission.Ledger {
	t.Helper()
	l, err := admission.Open(admission.Config{Dir: r.ledger, UID: uid, RunnerID: runnerID, Host: r.host, DockerMemoryBytes: 8 << 30, Volume: r.ledger, FileOwner: r.owner,
		Fstat: func(fd int, _ string) (unix.Stat_t, error) { // the package's install: every directory is root's
			var st unix.Stat_t
			err := unix.Fstat(fd, &st)
			st.Uid, st.Gid = 0, 0
			return st, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// helper runs the helper's run loop for dir as wardyn-macvm would: it claims its entry first, and is
// in the process table until it returns. Its guest agent answers on the fake machine.
func (r *macRig) helper(ctx context.Context, dir string) {
	r.runs.Add(2)
	r.host.set(admission.Proc{PID: macHelperPID, UID: 501, Start: 7, Name: admission.HelperProcess})
	go func() {
		defer r.runs.Done()
		defer r.host.gone(macHelperPID)
		_ = macvm.Run(ctx, macvm.RunOptions{Engine: r.eng, RunDir: dir, NetWait: 10 * time.Second, DesktopPassword: r.secret, SockFstat: macvmtest.SockFstat, Claim: func(cfg macvm.RunConfig) error {
			_, err := r.c.Ledger.ClaimHelper(cfg.LedgerEntry, macHelperPID, 7)
			return err
		}})
	}()
	go func() {
		defer r.runs.Done()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/ready", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(guestagent.Ready{Version: "test"})
		})
		mux.HandleFunc("POST /v1/init", func(w http.ResponseWriter, req *http.Request) {
			var in guestagent.Init
			_ = json.NewDecoder(req.Body).Decode(&in)
			r.mu.Lock()
			r.init = &in
			res := guestagent.InitResult{}
			if in.DesktopPassword != "" {
				set := r.setPassword
				res.DesktopPasswordSet = &set
			}
			r.mu.Unlock()
			_ = json.NewEncoder(w).Encode(res)
		})
		mux.HandleFunc("GET /v1/dial/tools", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(guestagent.ToolsDial{Class: guestagent.DialRefused, Error: "connection refused", Dials: 1})
		})
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) {
			ms := r.eng.Machines()
			if len(ms) == 0 {
				return nil, net.ErrClosed
			}
			return ms[len(ms)-1].DialGuest(macvm.AgentPort)
		}, mux)
	}()
}

func makeMacBase(t *testing.T, l macvm.Layout) {
	t.Helper()
	base := l.BaseDir(0)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{macvm.DiskFile: "base-disk", macvm.AuxFile: "base-aux", macvm.MachineIDFile: "base-machine-id"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMacManifest(t, l, "control")
}

// writeMacManifest writes the base's manifest with the given desktop.
func writeMacManifest(t *testing.T, l macvm.Layout, desktop string) {
	t.Helper()
	m := `{"format":1,"state":"ready","guest_os":"macOS 26.1","min_cpus":2,"min_memory_mib":4096,"hardware_model":"aHc=","desktop":"` + desktop + `","harnesses":["claude-code"]}`
	if err := os.WriteFile(filepath.Join(l.BaseDir(0), macvm.ManifestFile), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
}

func macSpec() runner.SandboxSpec {
	s := testSpec()
	s.MachineKind = types.MachineMacOSVM
	s.Resources = runner.Resources{CPUMillis: 4000, MemoryMiB: 8192}
	s.ProxyConfig.MITMCACertPEM = "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----"
	s.Env["WARDYN_HARNESSES"] = "claude-code"
	runner.SetSandboxCATrustVars(s.Env, false) // as the dispatcher fills them, with the Linux paths
	return s
}

// A macOS VM run: the ledger entry is taken before the helper starts, the relay is the guest's network
// (attached before its start and handed over before the VM starts), the guest agent gets the sandbox
// with the run's CA at its own path and none of the Linux CA paths, and the teardown releases the entry
// only after the helper has gone and the run directory is deleted.
func TestMacVMCreateAndTeardown(t *testing.T) {
	r := newMacRig(t)
	spec := macSpec()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Machine == nil || sb.Machine.Accelerator != types.MacVMAccelerator || sb.Machine.GuestOS != "macOS 26.1" || sb.EnforcedClass != types.CC3 {
		t.Fatalf("sandbox %+v machine %+v", sb, sb.Machine)
	}
	counted, err := r.c.Ledger.Counted()
	if err != nil || len(counted) != 1 || counted[0].HelperPID != macHelperPID || counted[0].RunID != spec.RunID.String() || counted[0].State != admission.StateRunning {
		t.Fatalf("ledger %+v %v", counted, err)
	}
	if ms := r.eng.Machines(); len(ms) != 1 || ms[0].Plan.CPUs != 4 || ms[0].Plan.MemoryBytes != 8<<30 {
		t.Fatalf("machines %+v", ms)
	}
	r.f.fakeDocker.mu.Lock()
	var relay *createdContainer
	for _, c := range r.f.containers {
		if c.cfg.Labels[labelComponent] == componentVMNet {
			relay = c
		}
	}
	r.f.fakeDocker.mu.Unlock()
	if relay == nil || relay.cfg.Labels[labelMachineKind] != string(types.MachineMacOSVM) || !relay.cfg.OpenStdin || relay.cfg.Cmd[0] != "-stdio" {
		t.Fatalf("relay %+v", relay)
	}
	r.mu.Lock()
	in := r.init
	r.mu.Unlock()
	if in == nil {
		t.Fatal("the guest agent was never handed its sandbox")
	}
	assertMacCA(t, *in)
	if pw := r.password(t); in.DesktopPassword != pw || macDesktopOff(r.c.runDir(spec.RunID)) {
		t.Fatalf("desktop password handed to the guest is not the helper's (%d bytes), desktop off %v", len(in.DesktopPassword), macDesktopOff(r.c.runDir(spec.RunID)))
	}
	if st, err := r.d.Status(context.Background(), sb.Ref); err != nil || st.State != types.RunRunning {
		t.Fatalf("status %+v %v", st, err)
	}

	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(r.c.runDir(spec.RunID)); !os.IsNotExist(err) {
		t.Fatalf("run directory left: %v", err)
	}
	counted, _ = r.c.Ledger.Counted()
	debts, _ := r.c.Ledger.Debts()
	if len(counted)+len(debts) != 0 {
		t.Fatalf("entry left after teardown: %+v %+v", counted, debts)
	}
}

// A call the organisation resets, which cancels its context, cannot cut the destroy short: the run
// directory is still removed and the ledger entry released, not left for the orphan sweep.
func TestMacVMDestroySurvivesACancelledCall(t *testing.T) {
	r := newMacRig(t)
	spec := macSpec()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.host.mu.Lock()
	r.host.onProcesses = cancel // the destroy's first ledger operation
	r.host.mu.Unlock()
	_ = r.d.StopSandbox(ctx, sb.Ref)
	if ctx.Err() == nil {
		t.Fatal("the call was never cancelled")
	}
	if _, err := os.Lstat(r.c.runDir(spec.RunID)); !os.IsNotExist(err) {
		t.Fatalf("run directory left after a cancelled call: %v", err)
	}
	counted, _ := r.c.Ledger.Counted()
	debts, _ := r.c.Ledger.Debts()
	if len(counted)+len(debts) != 0 {
		t.Fatalf("entry left after a cancelled call: %+v %+v", counted, debts)
	}
}

// assertMacCA: none of the dispatcher's Linux CA paths reach a macOS guest, the run's CA does at the
// guest's own path, and the rest of the env is untouched.
func assertMacCA(t *testing.T, in guestagent.Init) {
	t.Helper()
	linux := map[string]string{}
	runner.SetSandboxCATrustVars(linux, false)
	for k, v := range in.Env {
		if lv, ok := linux[k]; ok && v == lv {
			t.Errorf("%s=%s forwarded to the macOS guest", k, v)
		}
	}
	if in.Env["FOO"] != "bar" {
		t.Errorf("env %v lost FOO", in.Env)
	}
	var ca *guestagent.File
	for i := range in.Files {
		if in.Files[i].Path == guestagent.MacCABundlePath {
			ca = &in.Files[i]
		}
	}
	if ca == nil || !strings.Contains(string(ca.Content), "BEGIN CERTIFICATE") || ca.Mode != 0o644 {
		t.Errorf("CA file %+v", ca)
	}
}

func TestMacGuestInitForwardsNoLinuxCAPaths(t *testing.T) {
	spec := macSpec()
	spec.Env["SSL_CERT_FILE"] = "/Users/agent/own.pem" // the operator's own value stays
	in := macGuestInit(spec, "")
	assertMacCA(t, in)
	if in.Env["SSL_CERT_FILE"] != "/Users/agent/own.pem" {
		t.Fatalf("an operator's own CA variable was dropped: %v", in.Env)
	}
}

// The owner's return after another login tombstoned its entry: the orphan sweep deletes the run
// directory the dead helper left, then the entry, which clears its disk debt.
func TestMacVMOwnerReturnSweepsTombstonedRun(t *testing.T) {
	r := newMacRig(t)
	runID := uuid.New()
	e, err := r.c.Ledger.Admit(admission.Request{Kind: admission.KindRun, RunID: runID.String(), GuestCPUs: 4, GuestMemory: 8 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 1})
	if err != nil {
		t.Fatal(err)
	}
	r.host.set(admission.Proc{PID: macHelperPID, UID: 501, Start: 7, Name: admission.HelperProcess})
	if _, err := r.c.Ledger.ClaimHelper(e.ID, macHelperPID, 7); err != nil {
		t.Fatal(err)
	}
	dir := r.c.runDir(runID)
	if err := macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: runID.String(), BaseDir: r.c.Layout.BaseDir(0), CPUs: 4, MemoryMiB: 8192,
		Identity: macvm.IdentityFresh, RunnerID: r.c.RunnerID, LedgerEntry: e.ID}); err != nil {
		t.Fatal(err)
	}
	sockDir := r.makeSockResidue(t, runID) // the dead helper's socket directory
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(dir, past, past)
	// The login ended: its helper and runner are gone, and another login tombstones the entry.
	r.host.gone(macHelperPID)
	r.host.gone(os.Getpid())
	if n, err := r.open(t, 502, "runner-b").Sweep(); n != 1 || err != nil {
		t.Fatalf("other login's sweep = %d, %v", n, err)
	}
	tomb := "e-" + e.ID + ".json.tombstone"
	r.mu.Lock()
	r.owners[tomb] = 502
	r.mu.Unlock()
	if debts, _ := r.c.Ledger.Debts(); len(debts) != 1 {
		t.Fatalf("the tombstoned entry's disk is not a debt: %+v", debts)
	}

	// The owner logs in again.
	r.host.set(admission.Proc{PID: os.Getpid(), UID: 501, Start: 2, Name: "wardyn-runnerd"})
	if _, err := r.d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("the tombstoned run's directory was not swept: %v", err)
	}
	if _, err := os.Lstat(sockDir); !os.IsNotExist(err) {
		t.Fatalf("the swept run's socket directory stayed: %v", err)
	}
	counted, _ := r.c.Ledger.Counted()
	debts, _ := r.c.Ledger.Debts()
	if len(counted)+len(debts) != 0 {
		t.Fatalf("the tombstoned entry was not deleted: %+v %+v", counted, debts)
	}
	if _, err := os.Lstat(filepath.Join(r.ledger, "e-"+e.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("entry file left: %v", err)
	}
}

// ERRATA E5 (run half): on a base with a desktop, a run is ready only once its desktop password is set. A
// helper with no password for the run, or a guest that did not set it, fails the run by name
// (macvm.ErrDesktopPassword), and the failed create leaves no ledger entry, run directory or socket
// directory.
func TestMacVMRunWithoutItsDesktopPasswordFailsByName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(r *macRig)
		booted bool // the guest was handed its sandbox before the failure
	}{
		{"helper has none", func(r *macRig) { r.secret = nil }, false},
		{"guest did not set it", func(r *macRig) { r.setPassword = false }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newMacRig(t)
			tc.setup(r)
			spec := macSpec()
			if _, err := r.d.CreateSandbox(context.Background(), spec); !errors.Is(err, macvm.ErrDesktopPassword) {
				t.Fatalf("create: %v, want the named desktop password failure", err)
			}
			r.mu.Lock()
			booted := r.init != nil
			r.mu.Unlock()
			if booted != tc.booted {
				t.Fatalf("the guest was handed its sandbox: %v, want %v", booted, tc.booted)
			}
			if counted, _ := r.c.Ledger.Counted(); len(counted) != 0 {
				t.Fatalf("the failed run kept its ledger entry: %+v", counted)
			}
			if _, err := os.Lstat(r.c.runDir(spec.RunID)); !os.IsNotExist(err) {
				t.Fatalf("the failed run's directory stayed: %v", err)
			}
			if _, err := os.Lstat(r.c.sockDir(spec.RunID)); !os.IsNotExist(err) {
				t.Fatalf("the failed run's socket directory stayed: %v", err)
			}
		})
	}
}

// A base with no desktop runs with its desktop off: nothing to set, so no failure.
func TestMacVMDesktopOffOnABaseWithNoDesktop(t *testing.T) {
	r := newMacRig(t)
	writeMacManifest(t, r.c.Layout, "none")
	sb, err := r.d.CreateSandbox(context.Background(), macSpec())
	if err != nil {
		t.Fatalf("the run did not start: %v", err)
	}
	if r.init.DesktopPassword != "" {
		t.Fatal("a password reached the guest of a base with no desktop")
	}
	if _, err := r.d.DialDesktop(context.Background(), sb.Ref); !errors.Is(err, errMacDesktopOff) {
		t.Fatalf("desktop of a run with no desktop: %v", err)
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
}

// The desktop password reaches the guest agent only: nothing the runner logs and no file the run or the
// ledger leaves carries it, including when the guest does not take it.
func TestMacVMDesktopPasswordNeverLoggedOrWritten(t *testing.T) {
	for _, set := range []bool{true, false} {
		logs := &syncWriter{w: &bytes.Buffer{}}
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		r := newMacRig(t)
		r.setPassword = set
		pw := r.password(t)
		spec := macSpec()
		sb, err := r.d.CreateSandbox(context.Background(), spec)
		if (err == nil) != set {
			slog.SetDefault(prev)
			t.Fatal(err)
		}
		var files []string
		for _, root := range []string{r.c.runDir(spec.RunID), r.ledger} {
			_ = filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
				if err == nil && de.Type().IsRegular() {
					if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), pw) {
						files = append(files, p)
					}
				}
				return nil
			})
		}
		slog.Info("the guest's sandbox", "init", *r.init)
		if set {
			_ = r.d.StopSandbox(context.Background(), sb.Ref)
		}
		slog.SetDefault(prev)
		if strings.Contains(logs.String(), pw) {
			t.Fatalf("set=%v: the desktop password is in the runner's log:\n%s", set, logs.String())
		}
		if len(files) > 0 {
			t.Fatalf("set=%v: the desktop password was written to %v", set, files)
		}
		if !strings.Contains(logs.String(), "[redacted]") {
			t.Fatalf("set=%v: the Init dump was not redacted:\n%s", set, logs.String())
		}
	}
}

// The runner's free-space check ends the newest macOS VM run: its VM stops, the run reports the low disk,
// and the teardown deletes it as any other.
func TestMacVMEndNewestForLowDisk(t *testing.T) {
	r := newMacRig(t)
	if run, err := r.d.EndNewestMacVM(context.Background(), 3<<30); run != "" || err != nil {
		t.Fatalf("with no VM: %q %v", run, err)
	}
	spec := macSpec()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := r.d.EndNewestMacVM(context.Background(), 3<<30); run != spec.RunID.String() || err != nil {
		t.Fatalf("ended %q, %v", run, err)
	}
	if st, err := r.d.Status(context.Background(), sb.Ref); err != nil || st.State != types.RunStopped || st.Message != "This Mac ran low on disk (3 GB free), so the macOS VM was stopped." {
		t.Fatalf("status %+v %v", st, err)
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if counted, _ := r.c.Ledger.Counted(); len(counted) != 0 {
		t.Fatalf("entry left: %+v", counted)
	}
}

// password is the helper's desktop password for the run.
func (r *macRig) password(t *testing.T) string {
	t.Helper()
	pw, err := r.secret.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return pw
}

// sweepSockForTest is macvm.SweepSockResidue with the test's root-ownership stand-in: a key whose run
// directory is gone is removed.
func sweepSockForTest(l macvm.Layout, sockRoot string) ([]string, error) {
	sc := macvm.SockConfig(sockRoot)
	sc.Fstat = macvmtest.SockFstat
	return sc.SweepRunDirs(func(k string) bool {
		_, err := os.Lstat(filepath.Join(l.RunsDir(), k))
		return errors.Is(err, os.ErrNotExist)
	})
}

// CoversKind says which kinds Status can enumerate: containers and Linux VMs always, macOS VMs only
// while they are on, so a runnerd with them off reports its macOS VM runs unknown, not gone (#3667).
func TestDriverCoversKind(t *testing.T) {
	r := newMacRig(t)
	for _, kind := range []types.MachineKind{"", types.MachineContainer, types.MachineLinuxVM, types.MachineMacOSVM} {
		if !r.d.CoversKind(kind) {
			t.Errorf("CoversKind(%q) = false with macOS VMs on", kind)
		}
	}
	if r.d.CoversKind(types.MachineKind("zx_vm")) {
		t.Error("CoversKind answered true for an unknown kind")
	}
	if err := r.d.EnableMacVM(nil); err != nil {
		t.Fatal(err)
	}
	if r.d.CoversKind(types.MachineMacOSVM) {
		t.Error("CoversKind(macos_vm) = true with macOS VMs off")
	}
	for _, kind := range []types.MachineKind{"", types.MachineLinuxVM} {
		if !r.d.CoversKind(kind) {
			t.Errorf("CoversKind(%q) = false with macOS VMs off", kind)
		}
	}
}

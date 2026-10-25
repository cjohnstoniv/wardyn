// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"golang.org/x/sys/unix"
)

// host is this test process as the only process on a roomy Mac.
type host struct{}

func (host) BootTime() (int64, error)              { return 1, nil }
func (host) MemoryBytes() (uint64, error)          { return 64 << 30, nil }
func (host) AvailableMemoryBytes() (uint64, error) { return 48 << 30, nil }
func (host) FreeDiskBytes(string) (int64, error)   { return 500 << 30, nil }
func (host) Processes() ([]admission.Proc, error) {
	return []admission.Proc{{PID: os.Getpid(), UID: os.Getuid(), Start: 7, Name: "wardyn-macvm"}}, nil
}

// rootFstat reports every file as root:wheel's, as the runner package installs
// the ledger.
func rootFstat(fd int, _ string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstat(fd, &st)
	st.Uid, st.Gid = 0, 0
	return st, err
}

func testDeps(eng *macvmtest.Engine) (deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return deps{
		engine: func() (macvm.Engine, error) { return eng, nil }, host: host{}, stdout: &out, stderr: &errb,
		prepare: prepareDeps, fstat: rootFstat, sockFstat: macvmtest.SockFstat,
	}, &out, &errb
}

// makeLedger lays out the installed ledger under root and returns its
// entries dir.
func makeLedger(t *testing.T, root string) string {
	t.Helper()
	top := filepath.Join(root, "Wardyn")
	ledger := filepath.Join(top, "macos-vm-admission")
	if err := os.MkdirAll(ledger, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(top, 0o755)
	_ = os.Chmod(ledger, os.ModeSticky|0o777)
	if err := os.WriteFile(filepath.Join(top, "admission.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(top, "admission.lock"), 0o644)
	return ledger
}

func TestUsage(t *testing.T) {
	d, out, errb := testDeps(&macvmtest.Engine{})
	for _, args := range [][]string{nil, {"bogus"}, {"run"}, {"status"}, {"prepare", "--cpus", "4"}} {
		if code := run(context.Background(), args, d); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errb.String())
		}
	}
	if code := run(context.Background(), []string{"version"}, d); code != 0 || out.Len() == 0 {
		t.Fatalf("version: %d %q", code, out.String())
	}
}

// TestPrepareRefusedWhereProvisioningUnsupported: prepare is refused with
// the provisioner's reason where this build or host cannot boot a first boot.
func TestPrepareRefusedWhereProvisioningUnsupported(t *testing.T) {
	sup := macvm.NewProvisioner().Supported()
	if sup == nil || errors.Is(sup, macvm.ErrNoProvisioning) {
		t.Skip("this host can boot a first boot")
	}
	d, _, errb := testDeps(&macvmtest.Engine{})
	code := run(context.Background(), []string{"prepare", "--root", t.TempDir(), "--cpus", "4", "--memory-mib", "8192"}, d)
	if code != 1 || !strings.Contains(errb.String(), sup.Error()+"; prepare is not available") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// TestPrepareDepsWiresEveryPart: prepare gets macvm.NewProvisioner, the
// vmnet prep network and stage 1, and is refused only where no first boot
// can run; a host with no guest provisioning prepares by hand.
func TestPrepareDepsWiresEveryPart(t *testing.T) {
	var o macvm.PrepareOptions
	err := prepareDeps(&o, slog.New(slog.DiscardHandler))
	if reflect.TypeOf(o.FirstBoot) != reflect.TypeOf(macvm.NewProvisioner()) {
		t.Fatalf("FirstBoot = %T; want %T", o.FirstBoot, macvm.NewProvisioner())
	}
	if o.PrepNet == nil || o.Bootstrap == nil {
		t.Fatalf("PrepNet or Bootstrap unset: %+v", o)
	}
	if sup := macvm.NewProvisioner().Supported(); sup == nil || errors.Is(sup, macvm.ErrNoProvisioning) {
		if err != nil {
			t.Fatalf("prepareDeps on a host that can boot a first boot: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), sup.Error()) {
		t.Fatalf("prepareDeps: %v; want the provisioner's %q", err, sup)
	}
	// A macOS 26 host (no guest provisioning) is not refused; any other answer is.
	for _, tc := range []struct {
		sup    error
		refuse bool
	}{
		{fmt.Errorf("macvm provision: no class VZMacGuestProvisioningOptions; %w needs macOS 27", macvm.ErrNoProvisioning), false},
		{fmt.Errorf("macvm provision: has no %w", macvm.ErrNoVirtualization), true},
		{macvm.ErrProvisionUnsupported, true},
	} {
		o := macvm.PrepareOptions{FirstBoot: &macvmtest.FirstBoot{Unsupported: tc.sup}}
		if err := prepareDeps(&o, slog.New(slog.DiscardHandler)); (err != nil) != tc.refuse {
			t.Errorf("%v: prepareDeps %v, want refused %v", tc.sup, err, tc.refuse)
		}
	}
}

// TestMainDepsProbeProvisioning: the helper's own probe asks
// macvm.NewProvisioner, so it reports provisioning truthfully.
func TestMainDepsProbeProvisioning(t *testing.T) {
	d := mainDeps()
	if reflect.TypeOf(d.firstBoot) != reflect.TypeOf(macvm.NewProvisioner()) {
		t.Fatalf("firstBoot = %T; want %T", d.firstBoot, macvm.NewProvisioner())
	}
	var out bytes.Buffer
	d.engine = func() (macvm.Engine, error) { return &macvmtest.Engine{}, nil }
	d.stdout = &out
	_ = run(context.Background(), []string{"probe"}, d)
	var r probeReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("%v %q", err, out.String())
	}
	if want := okOr(macvm.NewProvisioner().Supported()); r.Provisioning != want {
		t.Fatalf("provisioning %q; want %q", r.Provisioning, want)
	}
}

func TestPlatformEngineOffMac(t *testing.T) {
	if _, err := platformEngine(); err == nil {
		t.Skip("a real engine is linked")
	}
}

// run claims its admission entry, runs the fake VM, and status reports the
// configured identifier's hash.
func TestRunClaimsEntryAndStatusReportsIdentity(t *testing.T) {
	root := t.TempDir()
	ledger := makeLedger(t, root)
	l, err := admission.Open(admission.Config{Dir: ledger, UID: os.Getuid(), RunnerID: "r1", Host: host{}, Fstat: rootFstat})
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Admit(admission.Request{Kind: admission.KindRun, GuestMemory: 8 << 30, DiskReserve: 1 << 30, OwnerPID: os.Getpid(), OwnerStart: 7})
	if err != nil {
		t.Fatal(err)
	}

	layout := macvm.Layout{Root: filepath.Join(root, "vm")}
	base := layout.BaseDir(0)
	_ = os.MkdirAll(base, 0o700)
	for n, b := range map[string]string{macvm.DiskFile: "d", macvm.AuxFile: "a", macvm.MachineIDFile: "base-id"} {
		_ = os.WriteFile(filepath.Join(base, n), []byte(b), 0o600)
	}
	_ = os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(`{"format":1,"state":"ready","hardware_model":"aHc="}`), 0o600)
	const runID = "0a1b2c3d-4e5f-4061-8273-8495a6b7c8d9"
	dir := layout.RunDir(runID)
	cfg := macvm.RunConfig{RunID: runID, BaseDir: base, CPUs: 2, MemoryMiB: 4096, Identity: macvm.IdentityTravel,
		RunnerID: "r1", LedgerEntry: e.ID, LedgerDir: ledger, SockRoot: macvmtest.SockRoot(t)}
	if err := macvm.WriteRunConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	eng := &macvmtest.Engine{}
	d, _, errb := testDeps(eng)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run", "--dir", dir}, d) }()

	sd, out, _ := testDeps(eng)
	deadline := time.Now().Add(5 * time.Second)
	for run(context.Background(), []string{"status", "--dir", dir}, sd) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("status never answered: %s", errb.String())
		}
		out.Reset()
		time.Sleep(10 * time.Millisecond)
	}
	var st macvm.Status
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.RunID != runID || st.Identity != macvm.IdentityTravel || st.MachineIDSHA256 != macvm.IDHash([]byte("base-id")) {
		t.Fatalf("status %+v", st)
	}
	es, _ := l.Counted()
	if len(es) != 1 || es[0].HelperPID != os.Getpid() || es[0].State != admission.StateStarting {
		t.Fatalf("entry not claimed by the helper: %+v", es)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("run exit %d: %s", code, errb.String())
	}
	if fi, err := os.Stat(filepath.Join(dir, macvm.HelperLog)); err != nil || fi.Size() == 0 {
		t.Fatalf("helper.log: %v", err)
	}
}

func TestRunRefusedWithoutEntry(t *testing.T) {
	root := t.TempDir()
	layout := macvm.Layout{Root: root}
	base := layout.BaseDir(0)
	_ = os.MkdirAll(base, 0o700)
	for n, b := range map[string]string{macvm.DiskFile: "d", macvm.AuxFile: "a", macvm.MachineIDFile: "id"} {
		_ = os.WriteFile(filepath.Join(base, n), []byte(b), 0o600)
	}
	_ = os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(`{"format":1,"state":"ready","hardware_model":"aHc="}`), 0o600)
	dir := layout.RunDir("r")
	_ = macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: "r", BaseDir: base, CPUs: 2, MemoryMiB: 4096, Identity: macvm.IdentityFresh,
		SockRoot: macvmtest.SockRoot(t)})
	d, _, errb := testDeps(&macvmtest.Engine{})
	if code := run(context.Background(), []string{"run", "--dir", dir}, d); code != 1 || !strings.Contains(errb.String(), "run.json names no admission entry") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// leaked writes one leaked-slot entry for the runner, leaving the refused run's own entry out of the cap
// check: the run and one other VM are the cap's two, and the framework's refusal is then a leaked slot.
func TestLeakedRecordsOneEntryForTheRunner(t *testing.T) {
	ledger := makeLedger(t, t.TempDir())
	l, err := admission.Open(admission.Config{Dir: ledger, UID: os.Getuid(), RunnerID: "r1", Host: host{}, Fstat: rootFstat})
	if err != nil {
		t.Fatal(err)
	}
	req := admission.Request{Kind: admission.KindRun, GuestMemory: 4 << 30, DiskReserve: 1 << 30, OwnerPID: os.Getpid(), OwnerStart: 7}
	own, err := l.Admit(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Admit(req); err != nil {
		t.Fatal(err)
	}
	d, _, _ := testDeps(&macvmtest.Engine{})
	d.host = host{}
	if err := leaked(macvm.RunConfig{RunnerID: "r1", LedgerEntry: own.ID, LedgerDir: ledger}, d); err != nil {
		t.Fatal(err)
	}
	var kinds []admission.Kind
	es, _ := l.Counted()
	for _, e := range es {
		kinds = append(kinds, e.Kind)
		if e.Kind == admission.KindLeaked && e.RunnerID != "r1" {
			t.Fatalf("leaked entry for runner %q", e.RunnerID)
		}
	}
	n := 0
	for _, k := range kinds {
		if k == admission.KindLeaked {
			n++
		}
	}
	if n != 1 || len(es) != 3 {
		t.Fatalf("counted entries %v, want the two runs and one leaked slot", kinds)
	}
}

// prepare without an entry says what prepare is missing, not run.json.
func TestPrepareRefusedWithoutEntry(t *testing.T) {
	d, _, errb := testDeps(&macvmtest.Engine{})
	d.prepare = func(o *macvm.PrepareOptions, _ *slog.Logger) error { o.FirstBoot = &macvmtest.FirstBoot{}; return nil }
	code := run(context.Background(), []string{"prepare", "--root", t.TempDir(), "--cpus", "4", "--memory-mib", "8192"}, d)
	if code != 1 || !strings.Contains(errb.String(), "prepare needs --runner-id and --ledger-entry") || strings.Contains(errb.String(), "run.json") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hostErr   error
		firstBoot macvm.FirstBooter
		want      int
	}{
		{"supported", nil, &macvmtest.FirstBoot{}, 0},
		{"macOS 26: no guest provisioning, setup by hand", nil, &macvmtest.FirstBoot{Unsupported: fmt.Errorf("no class X; %w needs macOS 27", macvm.ErrNoProvisioning)}, 0},
		{"old macOS", errors.New("macOS VMs need macOS 26 or later"), &macvmtest.FirstBoot{}, 1},
		{"no provisioning linked", nil, nil, 1},
		{"provisioning unsupported", nil, &macvmtest.FirstBoot{Unsupported: errors.New("too old")}, 1},
	} {
		d, out, _ := testDeps(&macvmtest.Engine{HostErr: tc.hostErr})
		d.firstBoot = tc.firstBoot
		code := run(context.Background(), []string{"probe"}, d)
		var r probeReport
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("%s: %v %q", tc.name, err, out.String())
		}
		if code != tc.want || r.Supported != (tc.want == 0) || r.GOOS == "" || r.HelperVersion == "" {
			t.Fatalf("%s: exit %d, report %+v", tc.name, code, r)
		}
		if fb, _ := tc.firstBoot.(*macvmtest.FirstBoot); tc.want == 0 && (r.Provisioning == "ok") != (fb.Unsupported == nil) {
			t.Fatalf("%s: provisioning %q", tc.name, r.Provisioning)
		}
		if tc.hostErr != nil && r.Host != tc.hostErr.Error() {
			t.Fatalf("%s: host %q", tc.name, r.Host)
		}
		if r.Thermal != "nominal" {
			t.Fatalf("%s: thermal %q", tc.name, r.Thermal)
		}
	}
	// runnerd reads the thermal state from probe: a hot Mac says so.
	d, out, _ := testDeps(&macvmtest.Engine{Thermal: macvm.ThermalSerious})
	d.firstBoot = &macvmtest.FirstBoot{}
	_ = run(context.Background(), []string{"probe"}, d)
	if !strings.Contains(out.String(), `"thermal": "serious"`) {
		t.Fatalf("probe on a hot Mac: %s", out.String())
	}
	d, out, _ = testDeps(nil)
	d.engine = platformEngine
	d.firstBoot = &macvmtest.FirstBoot{}
	if code := run(context.Background(), []string{"probe"}, d); vzLinked == (code == 1 && strings.Contains(out.String(), `"vz": false`)) {
		t.Fatalf("probe on this build: exit %d %s", code, out.String())
	}
}

// probe says, as a code, why first boot cannot when the Virtualization framework itself answered: no
// virtualization support or no provisioning. A build without the framework (the stub, or no first boot)
// gives none, and a supported Mac gives none.
func TestProbeReasonCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstBoot macvm.FirstBooter
		want      string
	}{
		{"supported", &macvmtest.FirstBoot{}, ""},
		{"no virtualization", &macvmtest.FirstBoot{Unsupported: fmt.Errorf("macvm provision: has no %w; needs macOS 27", macvm.ErrNoVirtualization)}, "no_virtualization"},
		{"no provisioning", &macvmtest.FirstBoot{Unsupported: errors.New("macvm provision: no class VZMacGuestProvisioningOptions")}, "no_provisioning"},
		{"no provisioning, macOS 26", &macvmtest.FirstBoot{Unsupported: fmt.Errorf("no class X; %w needs macOS 27", macvm.ErrNoProvisioning)}, "no_provisioning"},
		{"stub", &macvmtest.FirstBoot{Unsupported: macvm.ErrProvisionUnsupported}, ""},
		{"no first boot", nil, ""},
	} {
		d, out, _ := testDeps(&macvmtest.Engine{})
		d.firstBoot = tc.firstBoot
		_ = run(context.Background(), []string{"probe"}, d)
		var r probeReport
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("%s: %v %q", tc.name, err, out.String())
		}
		if r.Reason != tc.want {
			t.Fatalf("%s: reason %q, want %q", tc.name, r.Reason, tc.want)
		}
	}
}

// ledger-setup runs only as root, with one base path.
func TestLedgerSetupRefusesNonRoot(t *testing.T) {
	d, _, errb := testDeps(&macvmtest.Engine{})
	base := filepath.Join(t.TempDir(), "Wardyn")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	d.getuid = func() int { return 501 }
	if rc := run(context.Background(), []string{"ledger-setup", base}, d); rc != 1 || !strings.Contains(errb.String(), "must run as root") {
		t.Fatalf("non-root ledger-setup: rc %d, %q", rc, errb.String())
	}
	if des, _ := os.ReadDir(base); len(des) != 0 {
		t.Fatalf("a refused ledger-setup changed %s: %v", base, des)
	}
	d.getuid = func() int { return 0 }
	if rc := run(context.Background(), []string{"ledger-setup"}, d); rc != 2 {
		t.Fatalf("ledger-setup with no base: rc %d", rc)
	}
}

// TestProbeReportsGuestAgent: probe's guest_agent says whether the agent
// beside the helper is missing, another build, or this build.
func TestProbeReportsGuestAgent(t *testing.T) {
	p, err := guestAgentPath()
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sameBuild, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
	for _, tc := range []struct {
		name  string
		agent []byte // nil: no file
		want  string
	}{
		{"no agent", nil, "missing"},
		{"another build", []byte("not this helper's build"), "mismatch"},
		{"this build", sameBuild, "ok"},
	} {
		_ = os.Remove(p)
		if tc.agent != nil {
			if err := os.WriteFile(p, tc.agent, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		d, out, _ := testDeps(&macvmtest.Engine{})
		d.firstBoot = &macvmtest.FirstBoot{}
		_ = run(context.Background(), []string{"probe"}, d)
		var r probeReport
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("%s: %v %q", tc.name, err, out.String())
		}
		if r.GuestAgent != tc.want || !strings.Contains(out.String(), `"guest_agent": "`+tc.want+`"`) {
			t.Fatalf("%s: guest_agent %q; want %q", tc.name, r.GuestAgent, tc.want)
		}
	}
}

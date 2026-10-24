// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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
		prepare: prepareDeps, fstat: rootFstat,
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

func TestPrepareRefusedUntilWired(t *testing.T) {
	d, _, errb := testDeps(&macvmtest.Engine{})
	code := run(context.Background(), []string{"prepare", "--root", t.TempDir(), "--cpus", "4", "--memory-mib", "8192"}, d)
	if code != 1 || !strings.Contains(errb.String(), "prepare is not available") {
		t.Fatalf("exit %d: %s", code, errb.String())
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
	dir := layout.RunDir("run-9")
	cfg := macvm.RunConfig{RunID: "run-9", BaseDir: base, CPUs: 2, MemoryMiB: 4096, Identity: macvm.IdentityTravel,
		RunnerID: "r1", LedgerEntry: e.ID, LedgerDir: ledger}
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
	if st.RunID != "run-9" || st.Identity != macvm.IdentityTravel || st.MachineIDSHA256 != macvm.IDHash([]byte("base-id")) {
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
	_ = macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: "r", BaseDir: base, CPUs: 2, MemoryMiB: 4096, Identity: macvm.IdentityFresh})
	d, _, errb := testDeps(&macvmtest.Engine{})
	if code := run(context.Background(), []string{"run", "--dir", dir}, d); code != 1 || !strings.Contains(errb.String(), "run.json names no admission entry") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// prepare without an entry says what prepare is missing, not run.json.
func TestPrepareRefusedWithoutEntry(t *testing.T) {
	d, _, errb := testDeps(&macvmtest.Engine{})
	d.prepare = func(o *macvm.PrepareOptions) error { o.FirstBoot = &macvmtest.FirstBoot{}; return nil }
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
		{"old macOS", errors.New("macOS VMs need macOS 27 or later"), &macvmtest.FirstBoot{}, 1},
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
		if tc.hostErr != nil && r.Host != tc.hostErr.Error() {
			t.Fatalf("%s: host %q", tc.name, r.Host)
		}
	}
	d, out, _ := testDeps(nil)
	d.engine = platformEngine
	d.firstBoot = &macvmtest.FirstBoot{}
	if code := run(context.Background(), []string{"probe"}, d); vzLinked == (code == 1 && strings.Contains(out.String(), `"vz": false`)) {
		t.Fatalf("probe on this build: exit %d %s", code, out.String())
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

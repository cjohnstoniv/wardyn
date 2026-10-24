// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
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

func testDeps(eng *macvmtest.Engine) (deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return deps{
		engine: func() (macvm.Engine, error) { return eng, nil }, host: host{}, stdout: &out, stderr: &errb,
		prepare: prepareDeps,
	}, &out, &errb
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
	ledger := filepath.Join(root, "ledger")
	if err := os.Mkdir(ledger, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(ledger, os.ModeSticky|0o777)
	if err := os.WriteFile(filepath.Join(ledger, "admission.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := admission.Open(admission.Config{Dir: ledger, UID: os.Getuid(), RunnerID: "r1", Host: host{}})
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
	if code := run(context.Background(), []string{"run", "--dir", dir}, d); code != 1 || !strings.Contains(errb.String(), "admission") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A derived base that is not current is derived once no run on it lives and no other job does; while its
// derivation lives it reads rebuilding, an ended one that left it stale reads failed until Standard or its
// list changes, and a current one reads ready with its own manifest's facts.
func TestMacOSVMDerivedBases(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xcode-16.json"), []byte(`{"version":1,"items":[{"kind":"pkg","name":"xcode","path":"/opt/x.pkg","sha256":"`+
		string(slices.Repeat([]byte("a"), 64))+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.bases.once.Do(func() {})
	m.bases.dir = dir
	started := make(chan string, 4)
	m.bases.start = func(v macvm.BaseVariant, _ macvm.Manifest) error { started <- v.Name; return nil }
	man, _ := macvm.ReadManifest(m.layout.BaseDir(0))
	variant := m.layout.VariantDir("xcode-16")
	admit := func(kind admission.Kind, runID string) admission.Entry {
		t.Helper()
		e, err := m.ledger.Admit(admission.Request{Kind: kind, RunID: runID, GuestCPUs: 4, GuestMemory: 8 << 30,
			DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	release := func(e admission.Entry) {
		t.Helper()
		if err := m.ledger.Release(e.ID, admission.Destroyed{Stopped: true}); err != nil {
			t.Fatal(err)
		}
	}
	state := func(want string) runnerwire.MacVMDerivedBase {
		t.Helper()
		got := m.derivedBases(man)
		if len(got) != 1 || got[0].Name != "xcode-16" || got[0].Items != 1 || got[0].State != want {
			t.Fatalf("bases %+v, want one xcode-16 %s", got, want)
		}
		return got[0]
	}
	noStart := func() {
		t.Helper()
		select {
		case n := <-started:
			t.Fatalf("a derivation of %s started", n)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// A live run on the base: no derivation.
	run := uuid.NewString()
	runEntry := admit(admission.KindRun, run)
	if err := macvm.WriteRunConfig(m.layout.RunDir(run), macvm.RunConfig{RunID: run, BaseDir: variant}); err != nil {
		t.Fatal(err)
	}
	state(runnerwire.MacVMDerivedRebuilding)
	noStart()
	release(runEntry)

	// Another job lives: no derivation either.
	job := admit(admission.KindToolsUpdate, "")
	state(runnerwire.MacVMDerivedRebuilding)
	noStart()
	release(job)

	// Free: one derivation, then rebuilding while its job lives.
	state(runnerwire.MacVMDerivedRebuilding)
	if n := <-started; n != "xcode-16" {
		t.Fatalf("derived %s", n)
	}
	waitStarted(t, m)
	job = admit(admission.KindToolsUpdate, "")
	state(runnerwire.MacVMDerivedRebuilding)
	noStart()
	release(job)

	// The job ended and the base is still stale: failed, and not derived again from this Standard.
	state(runnerwire.MacVMDerivedFailed)
	state(runnerwire.MacVMDerivedFailed)
	noStart()

	// Current: ready, with its own manifest's facts.
	stamp, err := macvm.BaseStamp(m.layout.BaseDir(0))
	if err != nil {
		t.Fatal(err)
	}
	vs, _ := macvm.LoadBaseVariants(dir)
	writeDerived(t, variant, macvm.Manifest{Format: macvm.BaseFormat, State: macvm.BaseReady, GuestOS: "macOS 26.6.2", BuildVersion: "25G83",
		CreatedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Harnesses: []string{"none", "claude-code"}, DerivedFrom: stamp}, vs[0].List.Digest())
	got := state(runnerwire.MacVMDerivedReady)
	if got.GuestOS != "macOS 26.6.2" || got.BuildVersion != "25G83" || got.CreatedAt != "2026-10-28T00:00:00Z" || !slices.Equal(got.Harnesses, []string{"none", "claude-code"}) {
		t.Fatalf("ready base %+v", got)
	}
	noStart()

	// Standard changes: stale again, and derived again.
	writeToolsRecord(t, m.layout.BaseDir(0), macvm.ToolsRecord{Harnesses: []string{"none"}})
	state(runnerwire.MacVMDerivedRebuilding)
	if n := <-started; n != "xcode-16" {
		t.Fatalf("derived %s", n)
	}
}

// waitStarted waits for deriveNext's goroutine to have marked its derivation asked.
func waitStarted(t *testing.T, m *macOSVMs) {
	t.Helper()
	for range 100 {
		m.mu.Lock()
		done := !m.bases.starting && m.bases.asked != ""
		m.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the derivation was never marked started")
}

func writeDerived(t *testing.T, dir string, man macvm.Manifest, digest string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(dir, macvm.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeToolsRecord(t, dir, macvm.ToolsRecord{ItemsDigest: digest})
}

// macos_vm_bases_dir has a managed key and a runnerd.json field, and no flag or environment variable.
func TestRunnerConfigMacOSVMBasesDir(t *testing.T) {
	c := resolveRunnerConfig(inputs(nil, nil, map[string]string{"MacOSVMBasesDir": "/Library/Org/bases"}, runnerFile{MacOSVMBasesDir: "/x"}))
	if f := c.field("macos_vm_bases_dir"); f.Value != "/Library/Org/bases" {
		t.Fatalf("%+v", f)
	}
	if got := resolveRunnerConfig(inputs(nil, nil, nil, runnerFile{MacOSVMBasesDir: "/x"})).get("macos_vm_bases_dir"); got != "/x" {
		t.Fatalf("file: %q", got)
	}
}

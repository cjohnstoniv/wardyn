// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// The runner offers macOS VMs only when the helper's probe exits 0. One that fails turns them off with
// its reason kept on this Mac (the caps carry no refusal), and starts no prepare.
func TestMacOSVMOffersNoneUnlessTheProbePasses(t *testing.T) {
	for _, exit := range []int{0, 1} {
		r := newMacLedgerRig(t)
		r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
		m := r.open(t)
		var args string
		m.helper, args = fakeHelper(t, exit)
		m.gate(context.Background())
		m.recompute()
		c := m.current()
		if exit == 0 {
			if !m.enabled() || c.Max != 2 || m.why != "" {
				t.Fatalf("probe passed: enabled %v, caps %+v, why %q", m.enabled(), c, m.why)
			}
			continue
		}
		if m.enabled() || c.Max != 0 || c.Refusal != "" || !strings.Contains(m.why, "this build has no guest provisioning") {
			t.Fatalf("probe failed: enabled %v, caps %+v, why %q", m.enabled(), c, m.why)
		}
		// A prepare asked of a runner whose probe fails (the probe is run again) starts nothing.
		m2 := r.open(t)
		_ = os.Remove(filepath.Join(m2.layout.BaseDir(0), macvm.ManifestFile))
		if err := m2.startPrepare(context.Background(), prepareArgs{helper: m.helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192}); err == nil ||
			!strings.Contains(err.Error(), "probe") {
			t.Fatalf("prepare with a failing probe: %v", err)
		}
		for _, call := range calls(t, args) {
			if strings.HasPrefix(call, "prepare") {
				t.Fatalf("prepare ran after a failing probe: %q", calls(t, args))
			}
		}
		if counted, _ := m2.ledger.Counted(); len(counted) != 0 {
			t.Fatalf("a ledger entry was taken: %+v", counted)
		}
	}
}

// max is sized at the runner's per-run memory, not the base's minimum: per-run 6 GiB, minimum 4 GiB and
// an 11 GiB budget (64 GiB, less 49 for Docker and 4 for macOS) fit one guest, not two. With no base
// ready it offers none.
func TestMacOSVMMaxAtThePerRunMemory(t *testing.T) {
	r := newMacLedgerRig(t)
	open := func(perRun uint64) *macOSVMs {
		m := openMacOSVMs(macOSVMSetup{ledgerDir: r.dir, root: r.root, runnerID: "runner-a", version: "1.0.0", uid: 501,
			dockerMemory: 49 * gib, perRunMemory: perRun, host: r.host, owner: r.owner, fstat: r.fstat})
		if m.ledger == nil {
			t.Fatalf("ledger refused: %s", m.refusal)
		}
		return m
	}
	m := open(6 * gib)
	m.recompute()
	if c := m.current(); c.Max != 0 {
		t.Fatalf("no base: max %d", c.Max)
	}
	base := m.layout.BaseDir(0)
	_ = os.MkdirAll(base, 0o700)
	man := `{"format":1,"state":"ready","guest_os":"macOS 27.0","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":4096,"harnesses":["none"]}`
	if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	m.recompute()
	if c := m.current(); c.Max != 1 {
		t.Fatalf("per-run 6 GiB: max %d, want 1", c.Max)
	}
	m = open(0) // unknown per-run memory: guests at the base's minimum
	m.recompute()
	if c := m.current(); c.Max != 2 {
		t.Fatalf("per-run unknown: max %d, want 2", c.Max)
	}
}

// A base whose guest agent is not this runner's starts a tools update through the helper, once per base
// agent version: its own tools_update ledger entry, the helper's update-tools with that entry. A helper
// that claims nothing frees the entry.
func TestMacOSVMAgentDriftStartsAToolsUpdate(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	prev := prepareClaimWait
	prepareClaimWait = 2 * time.Second
	t.Cleanup(func() { prepareClaimWait = prev })
	bin := t.TempDir()
	args, kinds := filepath.Join(bin, "args"), filepath.Join(bin, "kinds")
	m.helper = filepath.Join(bin, "wardyn-macvm")
	script := "#!/bin/sh\necho \"$@\" >>" + args + "\ncat " + r.dir + "/e-*.json >>" + kinds + "\n"
	if err := os.WriteFile(m.helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 4)
	m.toolsUpdate = func(man macvm.Manifest) { errs <- m.startToolsUpdate(context.Background(), man) }
	man := `{"format":1,"state":"ready","guest_os":"macOS 27.0","agent_version":"0.9.0","min_cpus":4,"min_memory_mib":8192,"harnesses":["none"]}`
	_ = os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile), []byte(man), 0o600)
	m.recompute()
	m.recompute()
	select {
	case err := <-errs:
		if err == nil || !strings.Contains(err.Error(), "tools update ended at once") {
			t.Fatalf("tools update with a helper that claims nothing: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent drift started no tools update")
	}
	if len(errs) != 0 {
		t.Fatal("the tools update was asked twice for one base agent version")
	}
	got := calls(t, args)
	if len(got) != 1 || !strings.HasPrefix(got[0], "update-tools ") || !strings.Contains(got[0], "--ledger-entry ") || !strings.Contains(got[0], "--memory-mib 8192") {
		t.Fatalf("helper calls %q", got)
	}
	if b, _ := os.ReadFile(kinds); !strings.Contains(string(b), `"kind":"tools_update"`) {
		t.Fatalf("the update's ledger entry: %s", b)
	}
	if counted, _ := m.ledger.Counted(); len(counted) != 0 {
		t.Fatalf("an unclaimed tools-update entry stayed: %+v", counted)
	}
}

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

// driftBase makes m's ready base one whose guest agent is not this runner's.
func driftBase(t *testing.T, m *macOSVMs) macvm.Manifest {
	t.Helper()
	man := `{"format":2,"state":"ready","guest_os":"macOS 27.0","agent_version":"0.9.0","min_cpus":4,"min_memory_mib":8192,"harnesses":["none"]}`
	if err := os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := macvm.ReadManifest(m.layout.BaseDir(0))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// recordingHelper is a helper that records its argv and exits at once, claiming nothing.
func recordingHelper(t *testing.T, m *macOSVMs) (argsFile string) {
	t.Helper()
	bin := t.TempDir()
	argsFile = filepath.Join(bin, "args")
	m.helper = filepath.Join(bin, "wardyn-macvm")
	if err := os.WriteFile(m.helper, []byte("#!/bin/sh\necho \"$@\" >>"+argsFile+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	prev := prepareClaimWait
	prepareClaimWait = 2 * time.Second
	t.Cleanup(func() { prepareClaimWait = prev })
	return argsFile
}

// A runnerd that is not serving yet (its proxy image still missing) starts no base job: agent drift asks
// for a tools update only once serve is up, and then once.
func TestMacOSVMAgentDriftWaitsUntilServing(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	var asked int
	m.toolsUpdate = func(macvm.Manifest) { asked++ }
	driftBase(t, m)
	m.recompute()
	m.recompute()
	if asked != 0 {
		t.Fatalf("%d tools updates asked before serving", asked)
	}
	defer serving(m)()
	m.recompute()
	m.recompute()
	if asked != 1 {
		t.Fatalf("%d tools updates asked once serving, want 1", asked)
	}
}

// A tools update of this runner's that is still live refuses a second one before any helper starts.
func TestMacOSVMToolsUpdateNotStartedTwice(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	args := recordingHelper(t, m)
	man := driftBase(t, m)
	if _, err := m.ledger.Admit(admission.Request{Kind: admission.KindToolsUpdate, GuestCPUs: 4, GuestMemory: 8 * gib,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3}); err != nil {
		t.Fatal(err)
	}
	if err := m.startToolsUpdate(context.Background(), man); err == nil {
		t.Fatal("a second tools update started while one is live")
	}
	if b, err := os.ReadFile(args); err == nil {
		t.Fatalf("the helper ran: %q", b)
	}
	if counted, _ := m.ledger.Counted(); len(counted) != 1 {
		t.Fatalf("entries %+v, want only the live update's", counted)
	}
}

// A drift tools update whose start was refused (both VM slots in use) is asked again on a later
// recompute, but not before 10 minutes have passed.
func TestMacOSVMToolsUpdateRetriedAfterARefusedStart(t *testing.T) {
	r := newMacLedgerRig(t)
	self := admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"}
	other := admission.Proc{PID: 20, UID: 502, Start: 2, Name: "wardyn-runnerd"}
	r.host.setProcs(self, other, admission.Proc{PID: 60, UID: 502, Start: 9, Name: admission.HelperProcess},
		admission.Proc{PID: 61, UID: 502, Start: 9, Name: admission.HelperProcess})
	m := r.open(t)
	r.otherRun(t, 60)
	r.otherRun(t, 61)
	args := recordingHelper(t, m)
	now := time.Now()
	prevNow := driftNow
	driftNow = func() time.Time { return now }
	t.Cleanup(func() { driftNow = prevNow })
	errs := make(chan error, 4)
	m.toolsUpdate = func(man macvm.Manifest) { errs <- m.startToolsUpdate(context.Background(), man) }
	driftBase(t, m)
	defer serving(m)()

	m.recompute()
	if err := <-errs; err == nil {
		t.Fatal("a tools update started with both VM slots in use")
	}
	r.host.setProcs(self, other) // the other login's VMs end
	now = now.Add(5 * time.Minute)
	m.recompute()
	if len(errs) != 0 {
		t.Fatal("the refused tools update was asked again within 10 minutes")
	}
	now = now.Add(6 * time.Minute)
	m.recompute()
	select {
	case err := <-errs:
		if err == nil || !strings.Contains(err.Error(), "tools update ended at once") {
			t.Fatalf("the retried tools update: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refused tools update was never asked again")
	}
	if got := calls(t, args); len(got) != 1 || !strings.HasPrefix(got[0], macvm.UpdateToolsCommand+" ") {
		t.Fatalf("helper calls %q", got)
	}
}

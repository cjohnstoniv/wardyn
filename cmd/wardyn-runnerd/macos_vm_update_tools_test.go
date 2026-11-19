// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// `macos-vm update-tools` starts the same helper update drift starts, but passes --force: an update someone
// asks for updates a base that looks current. Without a ready base it starts nothing.
func TestMacOSVMUpdateToolsAsksTheHelperForcedUpdate(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	prev := prepareClaimWait
	prepareClaimWait = 2 * time.Second
	t.Cleanup(func() { prepareClaimWait = prev })
	var args string
	m.helper, args = fakeHelper(t, 0)
	_ = m.updateTools(context.Background()) // the fake helper claims nothing, so the start ends at once
	got := calls(t, args)
	if len(got) != 1 {
		t.Fatalf("helper calls %q, want one update-tools", got)
	}
	sub, set := helperFlags(t, strings.Fields(got[0]))
	if _, forced := set[macvm.FlagForce]; sub != macvm.UpdateToolsCommand || !forced || set[macvm.FlagLedgerEntry] == "" {
		t.Fatalf("update-tools argv %s %v, want %s with --%s and a ledger entry", sub, set, macvm.UpdateToolsCommand, macvm.FlagForce)
	}

	if err := os.Remove(m.layout.BaseDir(0) + "/" + macvm.ManifestFile); err != nil {
		t.Fatal(err)
	}
	if err := m.updateTools(context.Background()); err != errUpdateToolsNoBase {
		t.Fatalf("update-tools without a ready base: %v, want %v", err, errUpdateToolsNoBase)
	}
	if n := len(calls(t, args)); n != 1 {
		t.Fatalf("a base-less update-tools started the helper (%d calls)", n)
	}
}

// update-tools refuses, and starts nothing, while a prepare, tools update or edit-base of this runner lives:
// the helper's lock would only refuse it later, in tools-update.log.
func TestMacOSVMUpdateToolsRefusesWhileAJobRuns(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m := r.open(t)
	var args string
	m.helper, args = fakeHelper(t, 0)
	for _, k := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate, kindEditBase} {
		e, err := m.ledger.Admit(admission.Request{Kind: k, GuestMemory: 4 << 30, DiskReserve: 1 << 30, OwnerPID: os.Getpid(), OwnerStart: 3})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.updateTools(context.Background()); err != errUpdateToolsBusy {
			t.Fatalf("update-tools during %s: %v, want %v", k, err, errUpdateToolsBusy)
		}
		if err := m.ledger.Release(e.ID, admission.Destroyed{Stopped: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Fatalf("a refused update-tools ran the helper: %q", calls(t, args))
	}
}

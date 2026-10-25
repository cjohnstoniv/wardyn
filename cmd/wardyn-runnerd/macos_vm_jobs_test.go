// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// claimedStage2Job is a tools update whose helper (pid stage2HelperPID) lives, with no ctl.sock.
func claimedStage2Job(t *testing.T, r *macLedgerRig, m *macOSVMs) admission.Entry {
	t.Helper()
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"},
		admission.Proc{PID: stage2HelperPID, UID: 501, Start: 9, Name: admission.HelperProcess})
	prevPoll := jobSockPoll
	jobSockPoll = 10 * time.Millisecond
	t.Cleanup(func() { jobSockPoll = prevPoll })
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindToolsUpdate, GuestCPUs: 4, GuestMemory: 8 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: os.Getpid(), OwnerStart: 3})
	if err != nil {
		t.Fatal(err)
	}
	if e, err = m.ledger.ClaimHelper(e.ID, stage2HelperPID, 9); err != nil {
		t.Fatal(err)
	}
	return e
}

// An adopted network (an earlier runnerd started it) whose job's helper goes before its ctl.sock shows is
// removed by the wait for that socket, as it is by the wait after the hand-off.
func TestMacOSVMStage2SocketWaitRemovesAnAdoptedNetworkWhenTheVMGoes(t *testing.T) {
	r := newMacLedgerRig(t)
	nets := &prepNets{t: t}
	m := r.open(t)
	nets.use(m, macvmtest.SockRoot(t))
	e := claimedStage2Job(t, r, m)
	nets.up = map[string]bool{e.ID: true}
	done := make(chan struct{})
	go func() { defer close(done); m.handNet(context.Background(), e) }()
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handNet outlived the job's helper")
	}
	if built, undone, removed, up, _ := nets.counts(); built != 0 || undone != 0 || removed != 1 || up {
		t.Fatalf("built %d, undone %d, removed %d, up %v; want the adopted network removed once", built, undone, removed, up)
	}
}

// A stop of runnerd during that wait leaves the network: the helper outlives it and the next runnerd adopts it.
func TestMacOSVMStage2SocketWaitKeepsTheNetworkWhenRunnerdStops(t *testing.T) {
	r := newMacLedgerRig(t)
	nets := &prepNets{t: t}
	m := r.open(t)
	nets.use(m, macvmtest.SockRoot(t))
	e := claimedStage2Job(t, r, m)
	nets.up = map[string]bool{e.ID: true}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.handNet(ctx, e) }()
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handNet outlived its runnerd's stop")
	}
	if _, undone, removed, up, _ := nets.counts(); undone != 0 || removed != 0 || !up {
		t.Fatalf("undone %d, removed %d, up %v; want the network left up", undone, removed, up)
	}
}

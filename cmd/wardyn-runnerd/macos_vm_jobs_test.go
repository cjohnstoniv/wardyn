// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
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

// fakeStage2Helper listens on the job's ctl.sock and answers hello and status. On net_fd it takes the
// descriptor (attached from then on, as the helper's netFD does), then fails the send as the mode says:
// "lost" drops the connection with no reply, "refuse" answers an error and does not attach, and "gone"
// drops the connection and takes the whole socket down.
func fakeStage2Helper(t *testing.T, m *macOSVMs, id, mode string) {
	t.Helper()
	sc := macvm.SockConfig(m.sockRoot)
	sc.Fstat = macvmtest.SockFstat
	dir, err := sc.MakeRunDir(macvm.JobSockKey(id))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, macvm.ControlSock), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var attached atomic.Bool
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				rd, enc := bufio.NewReader(c), json.NewEncoder(c)
				for {
					line, err := rd.ReadBytes('\n')
					if err != nil {
						return
					}
					var req macvm.ControlRequest
					if json.Unmarshal(line, &req) != nil {
						return
					}
					switch req.Op {
					case macvm.OpHello:
						_ = enc.Encode(macvm.ControlReply{OK: true, Proto: macvm.ControlProto})
					case macvm.OpStatus:
						state := macvm.NetWaiting
						if attached.Load() {
							state = macvm.NetAttached
						}
						_ = enc.Encode(macvm.ControlReply{OK: true, Status: &macvm.Status{Proto: macvm.ControlProto, Network: state}})
					case macvm.OpNetFD:
						if mode == "refuse" {
							_ = enc.Encode(macvm.ControlReply{Error: "refused"})
							return
						}
						attached.Store(true)
						if mode == "gone" {
							_ = l.Close()
							_ = os.RemoveAll(dir)
						}
						return
					}
				}
			}()
		}
	}()
}

// A net_fd send that fails after the helper took the descriptor must not undo the network the
// helper holds; one that failed before it did is undone; one that cannot be told is left for the
// helper's own wait or the next start's sweep.
func TestMacOSVMFailedSendUndoesTheNetworkOnlyWhenTheHelperHoldsNone(t *testing.T) {
	for _, tc := range []struct {
		mode            string
		undone, removed int
		up              bool
		holds           bool
	}{
		{"lost", 0, 1, false, true}, // held: the hand-off stands, the network goes when the helper does
		{"refuse", 1, 0, false, false},
		{"gone", 0, 0, true, false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := newMacLedgerRig(t)
			nets := &prepNets{t: t}
			m := r.open(t)
			nets.use(m, macvmtest.SockRoot(t))
			e := claimedStage2Job(t, r, m)
			fakeStage2Helper(t, m, e.ID, tc.mode)
			done := make(chan struct{})
			go func() { defer close(done); m.handNet(context.Background(), e) }()
			if tc.holds {
				time.Sleep(300 * time.Millisecond)
				select {
				case <-done:
					t.Fatal("handNet returned while the helper holds the network")
				default:
				}
				r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("handNet did not return")
			}
			if built, undone, removed, up, _ := nets.counts(); built != 1 || undone != tc.undone || removed != tc.removed || up != tc.up {
				t.Fatalf("built %d, undone %d, removed %d, up %v; want 1, %d, %d, %v", built, undone, removed, up, tc.undone, tc.removed, tc.up)
			}
		})
	}
}

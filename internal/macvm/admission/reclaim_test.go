// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"testing"
	"time"
)

const (
	vzName     = "com.apple.Virtua" // VirtualizationProcess as the process table truncates it
	errMayBeVM = "admission: a Virtualization or helper process of this user may still be this VM"
)

// deadHelper is one login's run entry whose helper, pid 77, started an hour
// ago and is gone. It returns that start.
func deadHelper(t *testing.T) (*fakeHost, *Ledger, Entry, int64) {
	t.Helper()
	h := bigMac()
	l := newRig(t, h).open(t, 501, "r")
	e, err := l.Admit(runReq(8 * gib))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).UnixMicro()
	e, err = l.Update(e.ID, func(x *Entry) { x.HelperPID, x.HelperStart = 77, start })
	if err != nil {
		t.Fatal(err)
	}
	return h, l, e, start
}

func minutes(n int) int64 { return (time.Duration(n) * time.Minute).Microseconds() }

// Docker's Linux VM runs as a Virtualization process of the same user. One
// that started before the dead VM's helper is proven not to be that VM, so the
// slot is released without stopping Docker (#3700).
func TestReclaimOwnPastAnEarlierVirtualizationProcess(t *testing.T) {
	h, l, e, start := deadHelper(t)
	h.setProcs(
		Proc{PID: 50, UID: 501, Start: start - minutes(120), Name: vzName},            // Docker's Linux VM
		Proc{PID: 51, UID: 501, Start: start - minutes(121), Name: "com.docker.back"}, // its engine
		Proc{PID: 60, UID: 502, Start: start + minutes(5), Name: vzName},              // another login's VM
	)
	if err := l.ReclaimOwn(e.ID, ""); err != nil {
		t.Fatalf("a Virtualization process from before the helper kept the slot: %v", err)
	}
}

// A Virtualization or helper process of the user that started after the dead
// helper may be its VM, or its install process, still running: the slot stays,
// with Docker's earlier VM beside it or not.
func TestReclaimOwnKeepsAVMThatMayBeLive(t *testing.T) {
	docker := Proc{PID: 50, UID: 501, Name: vzName}
	for name, p := range map[string]Proc{
		"its VM":              {PID: 78, UID: 501, Start: minutes(1), Name: vzName},
		"its install process": {PID: 79, UID: 501, Start: minutes(2), Name: "wardyn-macvm"},
	} {
		t.Run(name, func(t *testing.T) {
			h, l, e, start := deadHelper(t)
			docker.Start, p.Start = start-minutes(120), start+p.Start
			for _, ps := range [][]Proc{{p}, {docker, p}} {
				h.setProcs(ps...)
				if err := l.ReclaimOwn(e.ID, ""); err == nil || err.Error() != errMayBeVM {
					t.Fatalf("past %+v: ReclaimOwn = %v", ps, err)
				}
			}
		})
	}
}

// Whatever cannot be proven not to be the VM keeps the slot.
func TestReclaimOwnKeepsUnknownAttribution(t *testing.T) {
	now := time.Now().UnixMicro()
	for name, tc := range map[string]struct {
		procs   func(start int64) []Proc
		procErr error
		noHelper,
		otherHelper bool
	}{
		// Within startSlack of the helper's start: a small clock step could hide the VM's.
		"just before the helper": {procs: func(s int64) []Proc { return []Proc{{PID: 50, UID: 501, Start: s - 5e6, Name: vzName}} }},
		// The dead helper's PID, reused by a later process: proves nothing.
		"reused pid": {procs: func(s int64) []Proc { return []Proc{{PID: 77, UID: 501, Start: s + minutes(3), Name: vzName}} }},
		// A start time after now: the clock went back, so no start proves anything.
		"clock went back": {procs: func(s int64) []Proc {
			return []Proc{{PID: 50, UID: 501, Start: s - minutes(120), Name: vzName}, {PID: 1, UID: 0, Start: now + minutes(60), Name: "launchd"}}
		}},
		// No helper recorded: the VM may have started any time after the owner.
		"no helper": {noHelper: true, procs: func(s int64) []Proc { return []Proc{{PID: 50, UID: 501, Start: s + minutes(1), Name: vzName}} }},
		// Another entry's live helper with no VM yet: the old count let this through.
		"another live helper": {otherHelper: true, procs: func(s int64) []Proc {
			return []Proc{{PID: 88, UID: 501, Start: s + minutes(10), Name: "wardyn-macvm"}}
		}},
		"process table error": {procErr: errors.New("sysctl failed"), procs: func(int64) []Proc { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			h, l, e, start := deadHelper(t)
			if tc.noHelper {
				var err error
				if e, err = l.Update(e.ID, func(x *Entry) { x.HelperPID, x.HelperStart, x.OwnerPID, x.OwnerStart = 0, 0, 10, start }); err != nil {
					t.Fatal(err)
				}
			}
			if tc.otherHelper {
				o, err := l.Admit(runReq(8 * gib))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := l.Update(o.ID, func(x *Entry) { x.HelperPID, x.HelperStart = 88, start+minutes(10) }); err != nil {
					t.Fatal(err)
				}
			}
			h.setProcs(tc.procs(start)...)
			h.mu.Lock()
			h.procErr = tc.procErr
			h.mu.Unlock()
			want := errMayBeVM
			if tc.procErr != nil {
				want = tc.procErr.Error()
			}
			if err := l.ReclaimOwn(e.ID, ""); err == nil || err.Error() != want {
				t.Fatalf("ReclaimOwn = %v, want %q", err, want)
			}
		})
	}
}

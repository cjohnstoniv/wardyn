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

// helperMono is deadHelper's helper's MonoStart; mono(n) is n ticks after it
// (before it when n is negative).
const helperMono = 1_000_000

func mono(n int) uint64 { return uint64(helperMono + n) }

// deadHelper is one login's run entry whose helper, pid 77, started an hour
// ago, claimed the entry and is gone. It returns that start.
func deadHelper(t *testing.T) (*fakeHost, *Ledger, Entry, int64) {
	t.Helper()
	h := bigMac()
	l := newRig(t, h).open(t, 501, "r")
	e, err := l.Admit(runReq(8 * gib))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).UnixMicro()
	h.setProcs(Proc{PID: 77, UID: 501, Start: start, MonoStart: helperMono, Name: HelperProcess})
	if e, err = l.ClaimHelper(e.ID, 77, start); err != nil || e.HelperMonoStart != helperMono {
		t.Fatalf("claim: %+v %v", e, err)
	}
	h.setProcs()
	return h, l, e, start
}

func minutes(n int) int64 { return (time.Duration(n) * time.Minute).Microseconds() }

// Docker's Linux VM runs as a Virtualization process of the same user. One
// created before the dead VM's helper, or before the owner of an entry no
// helper claimed, is proven not to be that VM, so the slot is released without
// stopping Docker (#3700). Another login's VM proves nothing about this one.
func TestReclaimOwnPastAnEarlierVirtualizationProcess(t *testing.T) {
	for name, noHelper := range map[string]bool{"after its helper": false, "after its owner": true} {
		t.Run(name, func(t *testing.T) {
			h, l, e, start := deadHelper(t)
			if noHelper {
				h.setProcs(Proc{PID: 10, UID: 501, Start: 1, MonoStart: helperMono, Name: "wardyn-runnerd"})
				o, err := l.Admit(runReq(8 * gib)) // owner pid 10, start 1
				if err != nil || o.OwnerMonoStart != helperMono {
					t.Fatalf("admit: %+v %v", o, err)
				}
				if err := l.Release(e.ID, Destroyed{Stopped: true}); err != nil {
					t.Fatal(err)
				}
				e = o
			}
			h.setProcs(
				Proc{PID: 50, UID: 501, Start: start - minutes(120), MonoStart: mono(-120), Name: vzName},            // Docker's Linux VM
				Proc{PID: 51, UID: 501, Start: start - minutes(121), MonoStart: mono(-121), Name: "com.docker.back"}, // its engine
				Proc{PID: 60, UID: 502, Start: start + minutes(5), Name: vzName},                                     // another login's VM
			)
			if err := l.ReclaimOwn(e.ID, ""); err != nil {
				t.Fatalf("a Virtualization process from before the helper kept the slot: %v", err)
			}
		})
	}
}

// A Virtualization or helper process of the user created after the dead
// helper may be its VM, or its install process, still running: the slot stays,
// with Docker's earlier VM beside it or not.
func TestReclaimOwnKeepsAVMThatMayBeLive(t *testing.T) {
	docker := Proc{PID: 50, UID: 501, MonoStart: mono(-120), Name: vzName}
	for name, p := range map[string]Proc{
		"its VM":              {PID: 78, UID: 501, Start: minutes(1), MonoStart: mono(1), Name: vzName},
		"its install process": {PID: 79, UID: 501, Start: minutes(2), MonoStart: mono(2), Name: "wardyn-macvm"},
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

// The helper starts, the clock is set back 30 seconds, the helper's VM or
// install process is created, and the clock is restored. That child's
// calendar start reads 20 seconds before the helper's, which proves nothing:
// it was created after the helper, so it keeps the slot once the helper is
// gone, with Docker's earlier VM beside it or not.
func TestReclaimOwnKeepsAVMCreatedWhileTheClockWasBack(t *testing.T) {
	for name, child := range map[string]string{"its VM": vzName, "its install process": HelperProcess} {
		t.Run(name, func(t *testing.T) {
			h, l, e, start := deadHelper(t)
			p := Proc{PID: 78, UID: 501, Start: start - (20 * time.Second).Microseconds(), MonoStart: mono(1), Name: child}
			docker := Proc{PID: 50, UID: 501, Start: start - minutes(120), MonoStart: mono(-120), Name: vzName}
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
	docker := func(s int64) Proc {
		return Proc{PID: 50, UID: 501, Start: s - minutes(120), MonoStart: mono(-120), Name: vzName}
	}
	for name, tc := range map[string]struct {
		procs       func(start int64) []Proc
		procErr     error
		entry       func(*Entry)
		otherHelper bool
	}{
		// An entry an earlier release wrote has no monotonic start: nothing is
		// proven to have been created before its helper.
		"an entry with no monotonic start": {entry: func(x *Entry) { x.OwnerMonoStart, x.HelperMonoStart = 0, 0 },
			procs: func(s int64) []Proc { return []Proc{docker(s)} }},
		// A process whose monotonic start could not be read.
		"an unreadable process start": {procs: func(s int64) []Proc {
			return []Proc{{PID: 50, UID: 501, Start: s - minutes(120), Name: vzName}}
		}},
		// The dead helper's PID, reused by a later process: proves nothing.
		"reused pid": {procs: func(s int64) []Proc {
			return []Proc{{PID: 77, UID: 501, Start: s + minutes(3), MonoStart: mono(3), Name: vzName}}
		}},
		// No helper recorded: the VM may have been created any time after the owner.
		"no helper": {entry: func(x *Entry) {
			x.HelperPID, x.HelperStart, x.HelperMonoStart, x.OwnerPID, x.OwnerMonoStart = 0, 0, 0, 10, helperMono
		}, procs: func(s int64) []Proc {
			return []Proc{{PID: 50, UID: 501, Start: s + minutes(1), MonoStart: mono(1), Name: vzName}}
		}},
		// Another entry's helper PID, but not its start: a reused PID, not that helper.
		"another entry's helper pid, reused": {otherHelper: true, procs: func(s int64) []Proc {
			return []Proc{{PID: 88, UID: 501, Start: s + minutes(11), MonoStart: mono(11), Name: "wardyn-macvm"}}
		}},
		"process table error": {procErr: errors.New("sysctl failed"), procs: func(int64) []Proc { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			h, l, e, start := deadHelper(t)
			if tc.entry != nil {
				var err error
				if e, err = l.Update(e.ID, tc.entry); err != nil {
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

// Another entry's live helper, matched by PID and start, is that entry's: a
// dead entry's slot is released past it while it has no VM yet. Its VM, or
// any other later process, still keeps the slot, and an entry of another uid
// proves nothing about this uid's processes.
func TestReclaimOwnPastAnotherEntrysHelper(t *testing.T) {
	for name, tc := range map[string]struct {
		otherUID int
		vm       bool
		release  bool
	}{
		"its helper, no VM yet":     {otherUID: 501, release: true},
		"its helper and a later VM": {otherUID: 501, vm: true},
		"recorded by another uid":   {otherUID: 502},
	} {
		t.Run(name, func(t *testing.T) {
			h := bigMac()
			r := newRig(t, h)
			a := r.open(t, 501, "a")
			e, err := a.Admit(runReq(8 * gib))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now().Add(-time.Hour).UnixMicro()
			if e, err = a.Update(e.ID, func(x *Entry) { x.HelperPID, x.HelperStart, x.HelperMonoStart = 77, start, helperMono }); err != nil {
				t.Fatal(err)
			}
			b := a
			if tc.otherUID != 501 {
				b = r.open(t, tc.otherUID, "b")
			}
			o, err := b.Admit(runReq(8 * gib))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.Update(o.ID, func(x *Entry) { x.HelperPID, x.HelperStart = 88, start+minutes(5) }); err != nil {
				t.Fatal(err)
			}
			ps := []Proc{{PID: 88, UID: 501, Start: start + minutes(5), MonoStart: mono(5), Name: "wardyn-macvm"}}
			if tc.vm {
				ps = append(ps, Proc{PID: 89, UID: 501, Start: start + minutes(6), MonoStart: mono(6), Name: vzName})
			}
			h.setProcs(ps...)
			err = a.ReclaimOwn(e.ID, "")
			if tc.release && err != nil {
				t.Fatalf("kept past the other entry's helper: %v", err)
			}
			if !tc.release && (err == nil || err.Error() != errMayBeVM) {
				t.Fatalf("ReclaimOwn = %v, want %q", err, errMayBeVM)
			}
		})
	}
}

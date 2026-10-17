// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A lease that lapsed while nobody took it (a database stall longer than attachLeaseTTL) is taken
// back by its holder; one another client has, or a take-over reserved, ends it; a database error
// ends nothing. And the keeper renews only a lease its writer has taken.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// leaseRecoveryWriter is alice, the writer on leaseCovRun with the lease, over leases.
func leaseRecoveryWriter(t *testing.T, leases *leaseCovLeases) (*Server, *leaseCovClient, func() func()) {
	t.Helper()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice := newLeaseCovClient("alice", attachSourceWeb)
	readOnly, release := srv.registerAttachHolder(leaseCovRun, alice.h)
	if readOnly {
		t.Fatal("alice was not the writer")
	}
	return srv, alice, release
}

// expectKeeps fails unless alice is still the writer, undisplaced, with the lease row her own.
func expectKeeps(t *testing.T, srv *Server, leases *leaseCovLeases, alice *leaseCovClient) {
	t.Helper()
	if w, _ := leaseCovState(srv, leaseCovRun); w != alice.h || alice.h.evicted.Load() || alice.wasDisplaced() {
		t.Fatalf("writer=%v evicted=%v displaced=%v, want alice to keep the slot", w, alice.h.evicted.Load(), alice.wasDisplaced())
	}
	if row, found := leases.row(leaseCovRun); !found || row.HolderID != alice.h.id || !row.ExpiresAt.After(leases.now) {
		t.Fatalf("lease row = %+v (found %v), want alice's, live", row, found)
	}
}

func TestLeaseRecoveryTickTakesBackALeaseThatLapsedUnclaimed(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	leases.advance(attachLeaseTTL + time.Second)

	srv.attachLeaseTick(t.Context())

	expectKeeps(t, srv, leases, alice)
	if calls := leases.reacquireCalls(); len(calls) != 1 || calls[0].HolderID != alice.h.id || calls[0].Principal != "alice" || !calls[0].Since.Equal(alice.h.since) {
		t.Fatalf("reacquire calls = %+v, want one under alice's own holder id", calls)
	}
	if !alice.h.canWrite() || alice.h.lease.suspect.Load() {
		t.Error("alice took her lease back but is still fenced")
	}
}

func TestLeaseRecoveryLiveTakesBackALeaseThatLapsedUnclaimed(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	alice.h.lease.verified.Store(0)
	leases.advance(attachLeaseTTL + time.Second)

	if alice.h.lease.live() {
		t.Fatal("a holder whose row lapsed passed the fence before taking it back")
	}
	srv.WaitBackground()

	expectKeeps(t, srv, leases, alice)
	holds := leases.holdsCalls()
	if !alice.h.lease.live() || leases.holdsCalls() != holds {
		t.Error("alice took her lease back but the fence does not trust it")
	}
}

func TestLeaseRecoveryADatabaseErrorKeepsTheWriterFencedAndLogsOnce(t *testing.T) {
	logs := leaseCovCaptureLogs(t)
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	leases.advance(attachLeaseTTL + time.Second)
	leases.fail("reacquire", errors.New("lease table unreachable"))

	srv.attachLeaseTick(t.Context())
	srv.attachLeaseTick(t.Context())

	if w, _ := leaseCovState(srv, leaseCovRun); w != alice.h || alice.wasDisplaced() || !alice.h.lease.held.Load() {
		t.Fatalf("writer=%v displaced=%v held=%v, want alice kept: an error is not a loss", w, alice.wasDisplaced(), alice.h.lease.held.Load())
	}
	if alice.h.canWrite() {
		t.Fatal("alice may write while her lease is unproven")
	}
	srv.WaitBackground() // the fence check started its own attempt
	if n := len(leases.reacquireCalls()); n != 3 {
		t.Fatalf("reacquire calls = %d, want one per tick and one from the fence check", n)
	}
	if got := logs.with("wardynd: could not take back a run's attach lease; the client's input is discarded until it can"); len(got) != 1 {
		t.Fatalf("logged %d times, want once for the run of errors", len(got))
	}

	leases.fail("reacquire", nil)
	srv.attachLeaseTick(t.Context())

	expectKeeps(t, srv, leases, alice)
	if !alice.h.canWrite() {
		t.Error("alice took her lease back but still cannot write")
	}
}

func TestLeaseRecoveryNeverTakesALeaseAnotherClientHolds(t *testing.T) {
	cases := []struct {
		name string
		row  store.AttachLease
	}{
		{"another replica's writer", store.AttachLease{HolderID: uuid.New(), Principal: "bob", Source: attachSourceWeb}},
		{"a take-over's reservation for alice herself", store.AttachLease{HolderID: uuid.New(), Principal: "alice", Source: attachLeaseReservedSource}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leases := newLeaseCovLeases()
			srv, alice, _ := leaseRecoveryWriter(t, leases)
			leases.advance(attachLeaseTTL + time.Second)
			row := tc.row
			row.RunID, row.ExpiresAt = leaseCovRun, leases.now.Add(attachLeaseTTL)
			leases.mu.Lock()
			leases.rows[leaseCovRun] = row
			leases.mu.Unlock()

			srv.attachLeaseTick(t.Context())

			if reason := alice.displacedWith(t); reason != attachLeaseLostReason {
				t.Errorf("displaced with %q, want %q", reason, attachLeaseLostReason)
			}
			if got, _ := leases.row(leaseCovRun); got.HolderID != row.HolderID || !got.ExpiresAt.Equal(row.ExpiresAt) {
				t.Errorf("lease row = %+v, want the other client's left as it was", got)
			}
			if w, _ := leaseCovState(srv, leaseCovRun); w != nil || alice.h.lease.held.Load() {
				t.Errorf("writer=%v held=%v, want alice ended", w, alice.h.lease.held.Load())
			}
		})
	}
}

// blockReacquire holds every ReacquireAttachLease until the returned release is called, and
// signals entered as each one starts.
func blockReacquire(leases *leaseCovLeases) (entered chan struct{}, release func()) {
	entered, gate := make(chan struct{}, 4), make(chan struct{})
	leases.beforeReacquire = func() {
		entered <- struct{}{}
		<-gate
	}
	return entered, func() { close(gate) }
}

func TestLeaseRecoveryAWriterThatLeftMeanwhileGivesTheLeaseBack(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, release := leaseRecoveryWriter(t, leases)
	leases.advance(attachLeaseTTL + time.Second)
	entered, unblock := blockReacquire(leases)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.attachLeaseTick(t.Context())
	}()
	<-entered

	releaseAttach(release) // the client's socket ends while the recovery is in flight
	unblock()
	<-done

	if row, found := leases.row(leaseCovRun); found {
		t.Fatalf("lease row = %+v, want none left for a client that is gone", row)
	}
	if alice.wasDisplaced() {
		t.Error("a client that left was displaced")
	}
}

func TestLeaseRecoveryAStaleRecoveryNeverEndsTheWriterThatReplacedIt(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, release := leaseRecoveryWriter(t, leases)
	bob := newLeaseCovClient("bob", attachSourceWeb)
	if ro, _ := srv.registerAttachHolder(leaseCovRun, bob.h); !ro {
		t.Fatal("bob was not an observer")
	}
	leases.advance(attachLeaseTTL + time.Second)
	entered, unblock := blockReacquire(leases)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.attachLeaseTick(t.Context())
	}()
	<-entered

	releaseAttach(release) // bob is promoted and takes the lease
	unblock()
	<-done

	if w, _ := leaseCovState(srv, leaseCovRun); w != bob.h || bob.wasDisplaced() || !bob.h.canWrite() || alice.wasDisplaced() {
		t.Fatalf("writer=%v displaced=%v/%v canWrite=%v, want bob the writer and nobody displaced", w, bob.wasDisplaced(), alice.wasDisplaced(), bob.h.canWrite())
	}
	if row, _ := leases.row(leaseCovRun); row.HolderID != bob.h.id {
		t.Fatalf("lease holder = %s, want bob's client", row.HolderID)
	}
}

func TestLeaseRecoveryATakeOverDuringRecoveryKeepsItsNoReconnectReason(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	leases.advance(attachLeaseTTL + time.Second)
	entered, unblock := blockReacquire(leases)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.attachLeaseTick(t.Context())
	}()
	<-entered

	prev, _ := srv.evictAttachHolderFor(leaseCovRun, "admin")
	if prev != alice.h {
		t.Fatalf("take-over displaced %v, want alice", prev)
	}
	prev.displace(attachTakeoverReason("admin"))
	unblock()
	<-done

	if reason := alice.displacedWith(t); reason != attachTakeoverReason("admin") {
		t.Errorf("displaced with %q, want the take-over's reason", reason)
	}
	if alice.wasDisplaced() {
		t.Error("alice was displaced a second time, after the take-over")
	}
	if row, found := leases.row(leaseCovRun); found && row.HolderID == alice.h.id {
		t.Errorf("lease row = %+v, want none left for the displaced client", row)
	}
}

// blockAcquire holds every AcquireAttachLease until the returned release is called, and signals
// entered as each one starts.
func blockAcquire(leases *leaseCovLeases) (entered chan struct{}, release func()) {
	entered, gate := make(chan struct{}, 4), make(chan struct{})
	leases.beforeAcquire = func() {
		entered <- struct{}{}
		<-gate
	}
	return entered, func() { close(gate) }
}

func TestLeaseKeeperLeavesAWriterStillTakingItsLease(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice := newLeaseCovClient("alice", attachSourceWeb)
	entered, unblock := blockAcquire(leases)
	readOnly := make(chan bool, 1)
	go func() {
		ro, _ := srv.registerAttachHolder(leaseCovRun, alice.h)
		readOnly <- ro
	}()
	<-entered

	srv.attachLeaseTick(t.Context())
	unblock()

	if <-readOnly {
		t.Fatal("alice was left read-only")
	}
	if n := len(leases.renewCalls()); n != 0 {
		t.Errorf("renew calls = %d, want none for a lease not yet taken", n)
	}
	expectKeeps(t, srv, leases, alice)
}

func TestLeaseKeeperLeavesAPromotedObserverStillTakingItsLease(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, release := leaseRecoveryWriter(t, leases)
	bob := newLeaseCovClient("bob", attachSourceWeb)
	if ro, _ := srv.registerAttachHolder(leaseCovRun, bob.h); !ro {
		t.Fatal("bob was not an observer")
	}
	entered, unblock := blockAcquire(leases)
	announced := make(chan func(), 1)
	go func() { announced <- release() }() // handoffAttachLease takes the lease for bob
	<-entered

	srv.attachLeaseTick(t.Context())
	unblock()

	if <-announced == nil {
		t.Fatal("bob's promotion was not announced")
	}
	for _, r := range leases.renewCalls() {
		if r.Holder == bob.h.id {
			t.Fatalf("the keeper renewed bob's lease before he took it")
		}
	}
	if w, _ := leaseCovState(srv, leaseCovRun); w != bob.h || bob.h.evicted.Load() || bob.wasDisplaced() || alice.wasDisplaced() {
		t.Fatalf("writer=%v evicted=%v displaced=%v, want bob the writer", w, bob.h.evicted.Load(), bob.wasDisplaced())
	}
	if row, found := leases.row(leaseCovRun); !found || row.HolderID != bob.h.id {
		t.Fatalf("lease row = %+v (found %v), want bob's", row, found)
	}
}

// FUG-L750 N5: a recovery that takes the lease back for a writer that has since left, and so gives it
// back, tells the other replicas the lease is free, as every other release does.
func TestLeaseRecoveryGivingTheLeaseBackTellsTheOtherReplicas(t *testing.T) {
	p := newLeaseCovPair(t)
	p.leases.advance(attachLeaseTTL + time.Second)
	entered, unblock := blockReacquire(p.leases)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.srv.attachLeaseTick(t.Context())
	}()
	<-entered

	releaseAttach(p.releaseAlice) // the client's socket ends while the recovery is in flight
	unblock()
	<-done

	if row, found := p.leases.row(leaseCovRun); found {
		t.Fatalf("lease row = %+v, want none left for a client that is gone", row)
	}
	// One notice is the leaving client's own release; the second is the recovery's.
	free := 0
	for _, k := range p.notices() {
		if k == livebus.KindAttachFree {
			free++
		}
	}
	if free != 2 {
		t.Errorf("attach_free notices = %d, want 2: the departing writer's and the lease the recovery gave back", free)
	}
}

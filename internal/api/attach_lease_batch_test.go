// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The keeper renews every lease its replica holds in batched statements, a lease the statement leaves out
// ends only its own holder, a recovery runs off the tick, and concurrent fence checks of one writer share one
// store read without ever trusting a confirmation older than attachLeaseFenceEvery. None of these tests calls
// t.Parallel: they change attachLeaseRenewBatch and the fakes' hooks.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// leaseBatchRun is the i'th run id of a test with several writers.
func leaseBatchRun(i int) uuid.UUID {
	return uuid.UUID{0: 0xBA, 14: byte(i >> 8), 15: byte(i)}
}

// leaseBatchWriters registers n writers, alice1..alicen, on runs leaseBatchRun(1..n) of srv.
func leaseBatchWriters(t *testing.T, srv *Server, n int) []*leaseCovClient {
	t.Helper()
	out := make([]*leaseCovClient, n)
	for i := range out {
		out[i] = newLeaseCovClient("alice", attachSourceWeb)
		if readOnly, _ := srv.registerAttachHolder(leaseBatchRun(i+1), out[i].h); readOnly {
			t.Fatalf("writer %d was not the writer", i+1)
		}
	}
	return out
}

// leaseBatchWaiter is bob on a second replica over the same leases, waiting for run's lease (another replica's
// holder has it), with the holder id a transfer to bob names.
func leaseBatchWaiter(t *testing.T, leases *leaseCovLeases, run uuid.UUID) (*Server, *leaseCovClient) {
	t.Helper()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	bob := newLeaseCovClient("bob", attachSourceWeb)
	if readOnly, _ := srv.registerAttachHolder(run, bob.h); !readOnly {
		t.Fatal("bob took a lease another replica holds")
	}
	return srv, bob
}

func TestLeaseBatchOneStatementRenewsEveryHeldLease(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	writers := leaseBatchWriters(t, srv, 3)
	for _, w := range writers {
		w.h.lease.verified.Store(nil)
	}
	leases.advance(attachLeaseRenewEvery)

	srv.attachLeaseTick(t.Context())

	if got := leases.batchCallCounts(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("renew statements = %v, want one of 3 keys", got)
	}
	for i, w := range writers {
		row, found := leases.row(leaseBatchRun(i + 1))
		if !found || row.HolderID != w.h.id || !row.ExpiresAt.Equal(leases.now.Add(attachLeaseTTL)) {
			t.Errorf("run %d row = %+v (found %v), want the writer's, renewed to now + the ttl", i+1, row, found)
		}
		if w.h.lease.verified.Load() == nil {
			t.Errorf("writer %d was not confirmed by its renew", i+1)
		}
	}
}

func TestLeaseBatchChunksAtTheStatementBound(t *testing.T) {
	old := attachLeaseRenewBatch
	attachLeaseRenewBatch = 2
	t.Cleanup(func() { attachLeaseRenewBatch = old })
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	leaseBatchWriters(t, srv, 5)

	srv.attachLeaseTick(t.Context())

	got := leases.batchCallCounts()
	sum := 0
	for _, n := range got {
		sum += n
		if n > 2 {
			t.Errorf("a statement carried %d keys, want at most 2", n)
		}
	}
	if sum != 5 || len(got) != 3 {
		t.Fatalf("renew statements = %v, want three covering all 5 keys", got)
	}
}

// A key the statement leaves out ends only its own holder: the run whose row went to another holder.
func TestLeaseBatchAKeyLeftOutIsALossOnlyForThatHolder(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	writers := leaseBatchWriters(t, srv, 3)
	for _, w := range writers {
		w.h.lease.verified.Store(nil)
	}
	run2 := leaseBatchRun(2)
	srv2, bob := leaseBatchWaiter(t, leases, run2)
	leases.put(run2, bob.h.id, "bob", attachSourceWeb) // the lease moved to bob on another replica

	srv.attachLeaseTick(t.Context())
	srv.WaitBackground()

	for _, i := range []int{0, 2} {
		w := writers[i]
		row, _ := leases.row(leaseBatchRun(i + 1))
		if w.h.lease.verified.Load() == nil || !row.ExpiresAt.Equal(leases.now.Add(attachLeaseTTL)) || w.wasDisplaced() || !w.h.canWrite() {
			t.Errorf("writer %d: verified=%v row=%+v displaced=%v, want it renewed and writing", i+1, w.h.lease.verified.Load() != nil, row, w.wasDisplaced())
		}
	}
	if reason := writers[1].displacedWith(t); reason != attachLeaseLostReason {
		t.Errorf("writer 2 displaced with %q, want %q", reason, attachLeaseLostReason)
	}
	if calls := leases.reacquireCalls(); len(calls) != 1 || calls[0].HolderID != writers[1].h.id {
		t.Errorf("reacquire calls = %+v, want exactly writer 2's", calls)
	}

	// Positive control: the new holder takes its lease on its own replica and writes.
	srv2.attachLeaseTick(t.Context())
	srv2.WaitBackground()
	if w, _ := leaseCovState(srv2, run2); w != bob.h || !bob.h.canWrite() {
		t.Errorf("bob = %v canWrite=%v, want the writer on the replica the lease moved to", w, bob.h.canWrite())
	}
}

// An error confirms none and ends none; once the database answers, the same holders write.
func TestLeaseBatchAnErrorConfirmsNothingAndEndsNobody(t *testing.T) {
	logs := leaseCovCaptureLogs(t)
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	writers := leaseBatchWriters(t, srv, 2)
	var before [2]time.Time
	for i, w := range writers {
		w.h.lease.verified.Store(nil)
		row, _ := leases.row(leaseBatchRun(i + 1))
		before[i] = row.ExpiresAt
	}
	boom := errors.New("lease table unreachable")
	leases.fail("renew", boom)
	leases.fail("holds", boom)

	srv.attachLeaseTick(t.Context())
	srv.WaitBackground()

	for i, w := range writers {
		row, _ := leases.row(leaseBatchRun(i + 1))
		if w.wasDisplaced() || w.h.lease.verified.Load() != nil || !row.ExpiresAt.Equal(before[i]) {
			t.Errorf("writer %d: displaced=%v verified=%v row expiry moved=%v, want nothing ended or confirmed", i+1, w.wasDisplaced(), w.h.lease.verified.Load() != nil, !row.ExpiresAt.Equal(before[i]))
		}
		if w.h.canWrite() {
			t.Errorf("writer %d may write while its lease is unproven", i+1)
		}
	}
	if got := logs.with("wardynd: could not renew a run's attach lease; the client keeps it until the database answers"); len(got) != 2 {
		t.Errorf("renew error logged %d times, want once per writer", len(got))
	}

	leases.fail("renew", nil)
	leases.fail("holds", nil)
	srv.attachLeaseTick(t.Context())
	srv.WaitBackground()
	for i, w := range writers {
		if !w.h.canWrite() {
			t.Errorf("writer %d cannot write once the database answers", i+1)
		}
	}
}

// A lapsed lease's recovery runs on its own goroutine, so a slow reacquire never holds the tick.
func TestLeaseBatchARecoveryRunsOffTheTick(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	leases.advance(attachLeaseTTL + time.Second)
	entered, unblock := blockReacquire(leases)
	var once sync.Once
	t.Cleanup(func() { once.Do(unblock) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.attachLeaseTick(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the tick waited for the recovery")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the recovery never reached the store")
	}
	once.Do(unblock)
	srv.WaitBackground()

	expectKeeps(t, srv, leases, alice)
}

// A take-over reserves the run's slot for the admin: the displaced writer's renew finds a row that is not its own.
func TestLeaseBatchATakeOverReservationEndsTheHolder(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	_, reservation, found, err := leases.ReserveAttachLease(t.Context(), leaseCovRun, uuid.Nil, "admin", "replica-b", attachLeaseTTL)
	if err != nil || !found {
		t.Fatalf("reserve: found=%v err=%v", found, err)
	}
	reserved, _ := leases.row(leaseCovRun)

	srv.attachLeaseTick(t.Context())
	srv.WaitBackground()

	if reason := alice.displacedWith(t); reason != attachLeaseLostReason {
		t.Errorf("alice displaced with %q, want %q", reason, attachLeaseLostReason)
	}
	if row, _ := leases.row(leaseCovRun); row != reserved || row.HolderID != reservation {
		t.Errorf("row = %+v, want the reservation left as it was", row)
	}

	// Positive control: the admin's own client takes the reserved slot and writes.
	admin := newLeaseCovClient("admin", attachSourceWeb)
	if readOnly, _ := srv.registerAttachHolder(leaseCovRun, admin.h); readOnly || !admin.h.canWrite() {
		t.Errorf("admin readOnly=%v canWrite=%v, want the writer", readOnly, admin.h.canWrite())
	}
}

func TestLeaseBatchTickStaysShortWithManyWriters(t *testing.T) {
	const writers = 2000
	const latency = 5 * time.Millisecond
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	leaseBatchWriters(t, srv, writers)
	leases.mu.Lock()
	leases.latency = latency
	leases.mu.Unlock()

	start := time.Now()
	srv.attachLeaseTick(t.Context())
	took := time.Since(start)

	got := leases.batchCallCounts()
	t.Logf("attach lease tick: %d writers, %v per statement, %d statements, %v", writers, latency, len(got), took)
	if took >= time.Second || len(got) != 4 {
		t.Fatalf("tick took %v in %d statements (%v), want under 1s in 4", took, len(got), got)
	}
}

// leaseBatchGate blocks every fence read until released, counting how many reached the store.
func leaseBatchGate(leases *leaseCovLeases) (entered *atomic.Int32, release func()) {
	entered, gate := new(atomic.Int32), make(chan struct{})
	leases.mu.Lock()
	leases.beforeHolds = func() {
		entered.Add(1)
		<-gate
	}
	leases.mu.Unlock()
	var once sync.Once
	return entered, func() { once.Do(func() { close(gate) }) }
}

// leaseBatchLive runs n concurrent live() calls on l and returns their answers once all return.
func leaseBatchLive(l *attachLease, n int, afterStart func()) []bool {
	answers := make([]bool, n)
	var wg sync.WaitGroup
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = l.live()
		}()
	}
	time.Sleep(100 * time.Millisecond)
	afterStart()
	wg.Wait()
	return answers
}

func TestLeaseFenceConcurrentChecksShareOneQuery(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	alice.h.lease.verified.Store(nil)
	_, release := leaseBatchGate(leases)
	t.Cleanup(release)
	before := leases.holdsCalls()

	answers := leaseBatchLive(alice.h.lease, 8, release)

	for i, ok := range answers {
		if !ok {
			t.Errorf("caller %d was refused, want the shared answer true", i)
		}
	}
	if got := leases.holdsCalls() - before; got != 1 {
		t.Errorf("store reads = %d for 8 concurrent checks, want 1", got)
	}
	srv.WaitBackground()
}

// A transfer while a shared check is in flight refuses the leader and every joiner.
func TestLeaseFenceASharedCheckRefusesEveryJoiner(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	alice.h.lease.verified.Store(nil)
	srv2, bob := leaseBatchWaiter(t, leases, leaseCovRun)
	_, release := leaseBatchGate(leases)
	t.Cleanup(release)

	answers := leaseBatchLive(alice.h.lease, 8, func() {
		leases.put(leaseCovRun, bob.h.id, "bob", attachSourceWeb)
		release()
	})

	for i, ok := range answers {
		if ok {
			t.Errorf("caller %d was admitted after the lease moved", i)
		}
	}
	if !alice.h.lease.suspect.Load() {
		t.Error("the lease was not marked suspect")
	}
	srv.WaitBackground()
	if reason := alice.displacedWith(t); reason != attachLeaseLostReason {
		t.Errorf("alice displaced with %q, want %q", reason, attachLeaseLostReason)
	}

	// Positive control: the new holder passes its own check.
	srv2.attachLeaseTick(t.Context())
	srv2.WaitBackground()
	if !bob.h.canWrite() {
		t.Error("bob, the new holder, cannot write")
	}
}

// A check begun in an older generation is never joined by a caller of a later one.
func TestLeaseFenceAJoinerOfAnOlderGenerationAsksAgain(t *testing.T) {
	leases := newLeaseCovLeases()
	_, alice, _ := leaseRecoveryWriter(t, leases)
	l := alice.h.lease
	l.verified.Store(nil)
	entered, release := leaseBatchGate(leases)
	t.Cleanup(release)
	before := leases.holdsCalls()

	first := make(chan bool, 1)
	go func() { first <- l.live() }()
	waitForCount(t, entered, 1)
	l.drop()
	l.take()
	l.verified.Store(nil)
	second := make(chan bool, 1)
	go func() { second <- l.live() }()
	waitForCount(t, entered, 2) // a joiner would never reach the store
	release()

	if old, now := <-first, <-second; old || !now {
		t.Errorf("older generation's check = %v, later = %v, want false and true", old, now)
	}
	if got := leases.holdsCalls() - before; got != 2 {
		t.Errorf("store reads = %d, want 2: the later generation asks for itself", got)
	}
}

// A joiner is refused once the lease is marked suspect while it waits, as a caller arriving after is.
func TestLeaseFenceAJoinerIsRefusedOnceTheLeaseIsSuspect(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, alice, _ := leaseRecoveryWriter(t, leases)
	l := alice.h.lease
	l.verified.Store(nil)
	gen, _ := l.state()
	entered, release := leaseBatchGate(leases)
	t.Cleanup(release)
	leader := make(chan bool, 1)
	go func() { leader <- l.live() }()
	waitForCount(t, entered, 1)
	joiner := make(chan bool, 1)
	go func() { joiner <- l.live() }()
	time.Sleep(100 * time.Millisecond)          // the joiner is waiting on the leader's check
	l.at(gen, func() { l.suspect.Store(true) }) // what a renew does for a key its statement left out
	release()
	<-leader
	if <-joiner {
		t.Error("a joiner was admitted after the lease was marked suspect")
	}
	srv.WaitBackground()
}

// waitForCount waits up to two seconds for n to reach want; it does not fail, so the caller's assertion names
// the outcome.
func waitForCount(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Only a confirmation younger than attachLeaseFenceEvery is trusted.
func TestLeaseFenceNeverTrustsAnOlderConfirmation(t *testing.T) {
	leases := newLeaseCovLeases()
	_, alice, _ := leaseRecoveryWriter(t, leases)
	l := alice.h.lease
	stale := time.Now().Add(-attachLeaseFenceEvery - time.Millisecond)
	l.verified.Store(&stale)
	before := leases.holdsCalls()

	if !l.live() {
		t.Fatal("the writer was refused")
	}
	if got := leases.holdsCalls() - before; got != 1 {
		t.Fatalf("store reads for an older confirmation = %d, want 1", got)
	}

	fresh := time.Now().Add(-100 * time.Millisecond)
	l.verified.Store(&fresh)
	before = leases.holdsCalls()
	if !l.live() {
		t.Fatal("the writer was refused")
	}
	if got := leases.holdsCalls() - before; got != 0 {
		t.Errorf("store reads for a confirmation 100ms old = %d, want 0", got)
	}
}

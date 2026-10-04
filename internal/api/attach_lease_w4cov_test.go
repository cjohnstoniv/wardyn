// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
)

var leaseCovRun = uuid.MustParse("00000000-0000-4000-8000-00000000c0de")

// leaseCovPromoteAudit is the data of a session.promote row.
type leaseCovPromoteAudit struct {
	Principal      string `json:"principal"`
	Source         string `json:"source"`
	PreviousHolder string `json:"previous_holder"`
}

// leaseCovExpectPromoteRow reads the next audit row and checks it is the promotion of principal over previous.
func leaseCovExpectPromoteRow(t *testing.T, a *leaseCovAudit, principal, previous string) {
	t.Helper()
	ev := a.next(t)
	if ev.Action != "session.promote" || ev.Outcome != "success" || ev.Actor != principal {
		t.Fatalf("audit row = %s/%s by %q, want a successful session.promote by %q", ev.Action, ev.Outcome, ev.Actor, principal)
	}
	if ev.RunID == nil || *ev.RunID != leaseCovRun {
		t.Fatalf("audit row run = %v, want %s", ev.RunID, leaseCovRun)
	}
	var d leaseCovPromoteAudit
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("audit data: %v", err)
	}
	if d.Principal != principal || d.PreviousHolder != previous || d.Source != attachSourceWeb {
		t.Fatalf("audit data = %+v, want %s promoted over %q via web", d, principal, previous)
	}
}

func TestLeaseCovAStoreWithNoLeaseTableLeavesTheLocalRegistryInCharge(t *testing.T) {
	srv := New(baseTestConfig(newHarness(t), newAuthzStore()))
	writer, observer := newLeaseCovClient("alice", attachSourceWeb), newLeaseCovClient("bob", attachSourceWeb)

	roW, releaseW := srv.registerAttachHolder(leaseCovRun, writer.h)
	roO, _ := srv.registerAttachHolder(leaseCovRun, observer.h)

	if roW || !roO {
		t.Fatalf("readOnly = %v for the first client and %v for the second, want false and true", roW, roO)
	}
	if srv.attachLeaseStore() != nil {
		t.Error("a store with no lease table was taken for one that keeps leases")
	}
	if writer.h.lease != nil || writer.h.id != uuid.Nil {
		t.Errorf("the writer carries a lease (%v) or id (%s) though nothing keeps leases", writer.h.lease, writer.h.id)
	}
	if !writer.h.canWrite() || observer.h.canWrite() {
		t.Errorf("canWrite = %v for the writer and %v for the observer, want true and false", writer.h.canWrite(), observer.h.canWrite())
	}
	if v := srv.remoteAttachHolderView(t.Context(), leaseCovRun); v != nil {
		t.Errorf("remoteAttachHolderView = %+v, want nil with no lease table", v)
	}
	if prev, announce := srv.evictRemoteAttachHolder(leaseCovRun, "bob"); prev != nil || announce != nil {
		t.Errorf("evictRemoteAttachHolder = %v, %v; want nothing to take over with no lease table", prev, announce != nil)
	}
	var none *attachLease
	if !none.live() {
		t.Error("a holder with no lease reports it does not hold one")
	}

	announce := releaseW()
	if announce == nil || !observer.h.writable.Load() {
		t.Fatalf("releasing the writer: announce=%v observerWritable=%v, want the observer promoted in place", announce != nil, observer.h.writable.Load())
	}
}

func TestLeaseCovFirstClientTakesTheLeaseAndASecondQueuesBehindIt(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice, bob := newLeaseCovClient("alice", attachSourceWeb), newLeaseCovClient("bob", attachSourceSSH)

	roA, _ := srv.registerAttachHolder(leaseCovRun, alice.h)
	if roA {
		t.Fatal("the first client was made read-only")
	}
	if alice.h.id == uuid.Nil || alice.h.lease == nil || alice.h.lease.h != alice.h || alice.h.lease.runID != leaseCovRun {
		t.Fatalf("the writer was not bound to a lease of its own: id=%s lease=%+v", alice.h.id, alice.h.lease)
	}
	row, ok := leases.row(leaseCovRun)
	if !ok || row.HolderID != alice.h.id || row.Principal != "alice" || row.Source != attachSourceWeb ||
		!row.Since.Equal(alice.h.since) || row.Replica != srv.replicaName() {
		t.Fatalf("lease row = %+v (found %v), want it held by alice's client for this replica", row, ok)
	}
	acq := leases.acquireCalls()
	if len(acq) != 1 || acq[0].From != uuid.Nil || acq[0].TTL != attachLeaseTTL {
		t.Fatalf("acquire calls = %+v, want one for a free lease with the standard ttl", acq)
	}
	if !alice.h.lease.held.Load() || !alice.h.canWrite() {
		t.Error("the writer that took the lease does not believe it holds it")
	}

	if roB, _ := srv.registerAttachHolder(leaseCovRun, bob.h); !roB {
		t.Fatal("the second client was not made an observer")
	}
	if got := len(leases.acquireCalls()); got != 1 {
		t.Errorf("acquire calls = %d, want the observer to ask for no lease", got)
	}
	if bob.h.id == uuid.Nil || bob.h.id == alice.h.id || bob.h.waitRemote.Load() || bob.h.lease.held.Load() {
		t.Errorf("the observer: id=%s waitRemote=%v held=%v, want its own id, not waiting for another replica and not holding", bob.h.id, bob.h.waitRemote.Load(), bob.h.lease.held.Load())
	}
}

func TestLeaseCovAClientRefusedTheLeaseIsAnObserverWaitingForIt(t *testing.T) {
	leases := newLeaseCovLeases()
	srvA, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	srvB, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice, carol := newLeaseCovClient("alice", attachSourceWeb), newLeaseCovClient("carol", attachSourceWeb)
	srvA.registerAttachHolder(leaseCovRun, alice.h)

	roC, _ := srvB.registerAttachHolder(leaseCovRun, carol.h)

	if !roC {
		t.Fatal("a client refused the lease was admitted as a writer")
	}
	writer, observers := leaseCovState(srvB, leaseCovRun)
	if writer != nil || len(observers) != 1 || observers[0] != carol.h {
		t.Fatalf("replica B registry = writer %v observers %v, want carol alone as an observer", writer, observers)
	}
	if carol.h.writable.Load() || !carol.h.waitRemote.Load() || carol.h.lease.held.Load() || carol.h.canWrite() {
		t.Errorf("carol: writable=%v waitRemote=%v held=%v canWrite=%v, want an observer queued for the lease",
			carol.h.writable.Load(), carol.h.waitRemote.Load(), carol.h.lease.held.Load(), carol.h.canWrite())
	}
	if row, _ := leases.row(leaseCovRun); row.HolderID != alice.h.id {
		t.Errorf("lease holder = %s, want alice's client %s to keep it", row.HolderID, alice.h.id)
	}
	acq := leases.acquireCalls()
	if len(acq) != 2 || acq[1].Lease.HolderID != carol.h.id || acq[1].Lease.Replica != srvB.replicaName() {
		t.Fatalf("acquire calls = %+v, want carol's attempt from replica B second", acq)
	}
	if srvA.replicaName() == srvB.replicaName() {
		t.Error("the two replicas share a name")
	}
}

func TestLeaseCovALeaseThatCannotBeAskedForLeavesTheClientReadOnlyAndLogsWhy(t *testing.T) {
	leases := newLeaseCovLeases()
	boom := errors.New("lease table unreachable")
	leases.fail("acquire", boom)
	logs := leaseCovCaptureLogs(t)
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice := newLeaseCovClient("alice", attachSourceWeb)

	ro, _ := srv.registerAttachHolder(leaseCovRun, alice.h)

	if !ro || alice.h.writable.Load() || alice.h.canWrite() {
		t.Fatalf("readOnly=%v writable=%v canWrite=%v, want a read-only client when the lease cannot be taken", ro, alice.h.writable.Load(), alice.h.canWrite())
	}
	if _, ok := leases.row(leaseCovRun); ok {
		t.Error("a lease row exists though the acquire failed")
	}
	recs := logs.with("wardynd: could not take a run's attach lease; the client is read-only")
	if len(recs) != 1 {
		t.Fatalf("%d log records for the failed acquire, want 1", len(recs))
	}
	if got := leaseCovAttr(recs[0], "run_id").String(); got != leaseCovRun.String() {
		t.Errorf("logged run_id = %q, want %s", got, leaseCovRun)
	}
	if err, _ := leaseCovAttr(recs[0], "err").Any().(error); !errors.Is(err, boom) {
		t.Errorf("logged err = %v, want the store's error", err)
	}
}

func TestLeaseCovLiveFencesAHolderAgainstTheStore(t *testing.T) {
	boom := errors.New("lease table unreachable")
	long := time.Hour
	cases := []struct {
		name      string
		prepare   func(l *leaseCovLeases, c *leaseCovClient)
		want      bool
		wantHolds int
		wantHeld  bool
		wantLost  bool
	}{
		{"a fresh confirmation is trusted without asking", func(_ *leaseCovLeases, c *leaseCovClient) {
			c.h.lease.held.Store(true)
			c.h.lease.verified.Store(time.Now().Add(time.Hour).UnixNano())
		}, true, 0, true, false},
		{"a lease this holder never took", func(_ *leaseCovLeases, c *leaseCovClient) { c.h.lease.held.Store(false) }, false, 0, false, false},
		{"an old confirmation the store still backs", func(_ *leaseCovLeases, c *leaseCovClient) {
			c.h.lease.verified.Store(time.Now().Add(-long).UnixNano())
		}, true, 1, true, false},
		{"an old confirmation and an unreachable store fails closed", func(l *leaseCovLeases, c *leaseCovClient) {
			c.h.lease.verified.Store(time.Now().Add(-long).UnixNano())
			l.fail("holds", boom)
		}, false, 1, true, false},
		{"an old confirmation the store no longer backs ends the holder", func(l *leaseCovLeases, c *leaseCovClient) {
			c.h.lease.verified.Store(time.Now().Add(-long).UnixNano())
			l.advance(attachLeaseTTL + time.Second)
		}, false, 1, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leases := newLeaseCovLeases()
			srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
			alice := newLeaseCovClient("alice", attachSourceWeb)
			srv.registerAttachHolder(leaseCovRun, alice.h)
			tc.prepare(leases, alice)
			before := alice.h.lease.verified.Load()

			got := alice.h.lease.live()

			if got != tc.want || alice.h.lease.held.Load() != tc.wantHeld || leases.holdsCalls() != tc.wantHolds {
				t.Fatalf("live=%v held=%v holdsCalls=%d, want %v/%v/%d", got, alice.h.lease.held.Load(), leases.holdsCalls(), tc.want, tc.wantHeld, tc.wantHolds)
			}
			if tc.want && tc.wantHolds == 1 && alice.h.lease.verified.Load() == before {
				t.Error("a confirmation the store backed did not refresh the holder's timestamp")
			}
			if !tc.want && tc.wantHolds == 1 && !tc.wantLost && alice.h.lease.verified.Load() != before {
				t.Error("a store error refreshed the confirmation")
			}
			if tc.wantLost {
				if reason := alice.displacedWith(t); reason != attachLeaseLostReason {
					t.Errorf("displaced with %q, want %q", reason, attachLeaseLostReason)
				}
				if w, _ := leaseCovState(srv, leaseCovRun); w != nil || !alice.h.evicted.Load() {
					t.Errorf("a holder whose lease was found gone still has the writer slot (writer=%v evicted=%v)", w, alice.h.evicted.Load())
				}
			} else if alice.wasDisplaced() {
				t.Error("a holder that kept its lease was displaced")
			}
		})
	}
}

func TestLeaseCovTickRenewsHeldLeasesAndEndsOnesThatLapsed(t *testing.T) {
	boom := errors.New("lease table unreachable")
	cases := []struct {
		name      string
		prepare   func(l *leaseCovLeases)
		wantLost  bool
		wantKinds []string
	}{
		{"a live lease is renewed", func(l *leaseCovLeases) { l.advance(attachLeaseRenewEvery) }, false, nil},
		{"a lapsed lease ends its holder and frees the slot", func(l *leaseCovLeases) { l.advance(attachLeaseTTL + time.Second) }, true, []string{livebus.KindAttachFree}},
		{"a renew that cannot be asked is not read as a loss", func(l *leaseCovLeases) { l.fail("renew", boom) }, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := leaseCovCaptureLogs(t)
			leases := newLeaseCovLeases()
			srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{bus: "replica-a"})
			alice := newLeaseCovClient("alice", attachSourceWeb)
			srv.registerAttachHolder(leaseCovRun, alice.h)
			alice.h.lease.verified.Store(0)
			tc.prepare(leases)
			expires := leaseCovEpoch.Add(attachLeaseTTL)

			srv.attachLeaseTick(t.Context())
			srv.WaitBackground()

			if calls := leases.renewCalls(); len(calls) != 1 || calls[0] != (leaseCovRenew{leaseCovRun, alice.h.id, attachLeaseTTL}) {
				t.Fatalf("renew calls = %+v, want one for alice's client with the standard ttl", calls)
			}
			if got := leaseCovNoticeKinds(logs); len(got) != len(tc.wantKinds) || (len(got) == 1 && got[0] != tc.wantKinds[0]) {
				t.Errorf("notices sent = %v, want %v", got, tc.wantKinds)
			}
			row, found := leases.row(leaseCovRun)
			if tc.wantLost {
				if reason := alice.displacedWith(t); reason != attachLeaseLostReason {
					t.Errorf("displaced with %q, want %q", reason, attachLeaseLostReason)
				}
				if found || alice.h.lease.held.Load() || !alice.h.evicted.Load() {
					t.Errorf("after the loss: row found=%v held=%v evicted=%v, want the row released and the holder evicted", found, alice.h.lease.held.Load(), alice.h.evicted.Load())
				}
				return
			}
			if alice.wasDisplaced() {
				t.Error("a holder whose renew did not report a loss was displaced")
			}
			if w, _ := leaseCovState(srv, leaseCovRun); w != alice.h || !alice.h.lease.held.Load() {
				t.Errorf("writer=%v held=%v, want alice to keep the slot", w, alice.h.lease.held.Load())
			}
			if tc.name == "a live lease is renewed" {
				if alice.h.lease.verified.Load() == 0 || !row.ExpiresAt.Equal(leases.now.Add(attachLeaseTTL)) {
					t.Errorf("renewed row expires %v (verified %d), want it pushed a full ttl past the store's clock", row.ExpiresAt, alice.h.lease.verified.Load())
				}
			} else if !row.ExpiresAt.Equal(expires) || alice.h.lease.verified.Load() != 0 {
				t.Errorf("a failed renew changed the row (expires %v) or the confirmation (%d)", row.ExpiresAt, alice.h.lease.verified.Load())
			}
		})
	}
}

func TestLeaseCovTickPromotesTheWaitingObserverOnceTheLeaseIsFree(t *testing.T) {
	leases := newLeaseCovLeases()
	srvA, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	srvB, audB, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice, carol := newLeaseCovClient("alice", attachSourceWeb), newLeaseCovClient("carol", attachSourceWeb)
	_, releaseA := srvA.registerAttachHolder(leaseCovRun, alice.h)
	srvB.registerAttachHolder(leaseCovRun, carol.h)

	srvB.attachLeaseTick(t.Context())

	if w, obs := leaseCovState(srvB, leaseCovRun); w != nil || len(obs) != 1 || !carol.h.waitRemote.Load() {
		t.Fatalf("while alice holds the lease: writer=%v observers=%d waitRemote=%v, want carol still waiting", w, len(obs), carol.h.waitRemote.Load())
	}
	if got := len(leases.acquireCalls()); got != 3 {
		t.Fatalf("acquire calls = %d, want alice's, carol's at registration and carol's at the tick", got)
	}
	if got := len(audB.snapshot()); got != 0 {
		t.Fatalf("%d audit rows while nothing was promoted", got)
	}

	if announce := releaseA(); announce != nil {
		t.Fatal("alice's departure promoted somebody on a replica that has no observer")
	}
	if _, ok := leases.row(leaseCovRun); ok {
		t.Fatal("alice's departure left her lease in place")
	}
	srvB.attachLeaseTick(t.Context())
	leaseCovExpectPromoteRow(t, audB, "carol", "")
	carol.promotedNotice(t)

	if w, obs := leaseCovState(srvB, leaseCovRun); w != carol.h || len(obs) != 0 {
		t.Fatalf("replica B registry = writer %v observers %d, want carol the writer", w, len(obs))
	}
	if !carol.h.writable.Load() || carol.h.waitRemote.Load() || !carol.h.lease.held.Load() {
		t.Errorf("carol: writable=%v waitRemote=%v held=%v, want a promoted holder of the lease", carol.h.writable.Load(), carol.h.waitRemote.Load(), carol.h.lease.held.Load())
	}
	row, _ := leases.row(leaseCovRun)
	last := leases.acquireCalls()[3]
	if row.HolderID != carol.h.id || row.Replica != srvB.replicaName() || last.From != uuid.Nil {
		t.Errorf("lease row = %+v, promoting acquire from %s; want carol's client on replica B taking a free lease", row, last.From)
	}
}

func TestLeaseCovTickSkipsWritersWithoutALeaseAndObserversNotWaitingOnAnotherReplica(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	// A run whose writer has no lease (it predates one) is not renewed.
	plainRun := uuid.MustParse("00000000-0000-4000-8000-00000000c0df")
	plain := newLeaseCovClient("plain", attachSourceWeb)
	plain.h.writable.Store(true)
	leaseCovSeed(srv, plainRun, plain.h)
	// An observer a local take-over left behind is not waiting on another replica, so the slot is the taker's.
	left := newLeaseCovClient("left-behind", attachSourceWeb)
	srv.bindAttachLease(leaseCovRun, left.h)
	leaseCovSeed(srv, leaseCovRun, nil, left.h)

	srv.attachLeaseTick(t.Context())
	srv.promoteFromLease(uuid.MustParse("00000000-0000-4000-8000-00000000c0e0"))
	srv.promoteFromLease(leaseCovRun)

	if got := leases.acquireCalls(); len(got) != 0 {
		t.Errorf("acquire calls = %+v, want none", got)
	}
	if got := leases.renewCalls(); len(got) != 0 {
		t.Errorf("renew calls = %+v, want none for a writer with no lease", got)
	}
	if w, obs := leaseCovState(srv, leaseCovRun); w != nil || len(obs) != 1 || left.h.writable.Load() {
		t.Errorf("writer=%v observers=%d writable=%v, want the left-behind observer untouched", w, len(obs), left.h.writable.Load())
	}
}

// leaseCovWaiting seeds run with one observer queued for a lease another replica held.
func leaseCovWaiting(srv *Server) *leaseCovClient {
	o := newLeaseCovClient("carol", attachSourceWeb)
	srv.bindAttachLease(leaseCovRun, o.h)
	o.h.waitRemote.Store(true)
	leaseCovSeed(srv, leaseCovRun, nil, o.h)
	return o
}

func TestLeaseCovPromoteFromLeaseGivesBackALeaseItCannotUse(t *testing.T) {
	cases := []struct {
		name  string
		hook  func(srv *Server, o *attachHolder)
		check func(t *testing.T, srv *Server)
	}{
		{"the observer left while the lease was being taken", func(srv *Server, _ *attachHolder) {
			leaseCovSeed(srv, leaseCovRun, nil)
		}, func(t *testing.T, srv *Server) {
			if w, obs := leaseCovState(srv, leaseCovRun); w != nil || len(obs) != 0 {
				t.Errorf("registry writer=%v observers=%d, want an empty run", w, len(obs))
			}
		}},
		{"a writer arrived while the lease was being taken", func(srv *Server, o *attachHolder) {
			leaseCovSeed(srv, leaseCovRun, newLeaseCovClient("dave", attachSourceWeb).h, o)
		}, func(t *testing.T, srv *Server) {
			if w, _ := leaseCovState(srv, leaseCovRun); w == nil || w.principal != "dave" {
				t.Errorf("writer = %v, want dave's, who arrived first", w)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leases := newLeaseCovLeases()
			srv, aud, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
			o := leaseCovWaiting(srv)
			leases.beforeAcquire = func() { tc.hook(srv, o.h) }

			srv.promoteFromLease(leaseCovRun)

			if rel := leases.releaseCalls(); len(rel) != 1 || rel[0] != o.h.id {
				t.Errorf("release calls = %v, want the observer's lease handed back once", rel)
			}
			if _, ok := leases.row(leaseCovRun); ok {
				t.Error("the lease the observer could not use was left in the store")
			}
			if o.h.lease.held.Load() || o.h.writable.Load() {
				t.Errorf("observer held=%v writable=%v, want neither", o.h.lease.held.Load(), o.h.writable.Load())
			}
			tc.check(t, srv)
			if got := len(aud.snapshot()); got != 0 {
				t.Errorf("%d audit rows for a promotion that did not happen", got)
			}
		})
	}
}

func TestLeaseCovPromoteFromLeaseKeepsTheObserverWhenTheLeaseIsStillHeld(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, aud, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	o := leaseCovWaiting(srv)
	leases.put(leaseCovRun, uuid.MustParse("00000000-0000-4000-8000-00000000f00d"), "alice", attachSourceWeb)

	srv.promoteFromLease(leaseCovRun)

	if w, obs := leaseCovState(srv, leaseCovRun); w != nil || len(obs) != 1 || obs[0] != o.h || !o.h.waitRemote.Load() {
		t.Errorf("registry writer=%v observers=%v waitRemote=%v, want carol still queued", w, obs, o.h.waitRemote.Load())
	}
	if rel := leases.releaseCalls(); len(rel) != 0 {
		t.Errorf("release calls = %v, want none for a lease never taken", rel)
	}
	if row, _ := leases.row(leaseCovRun); row.Principal != "alice" {
		t.Errorf("lease row = %+v, want the other holder's left alone", row)
	}
	if got := len(aud.snapshot()); got != 0 {
		t.Errorf("%d audit rows for a refused promotion", got)
	}
}

func TestLeaseCovDemoteWriterQueuesTheHolderAheadOfTheObservers(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	writer, obs1 := newLeaseCovClient("alice", attachSourceWeb), newLeaseCovClient("bob", attachSourceWeb)
	srv.bindAttachLease(leaseCovRun, writer.h)
	writer.h.writable.Store(true)
	writer.h.lease.held.Store(true)
	leaseCovSeed(srv, leaseCovRun, writer.h, obs1.h)

	srv.demoteWriter(leaseCovRun, writer.h)

	w, obs := leaseCovState(srv, leaseCovRun)
	if w != nil || len(obs) != 2 || obs[0] != writer.h || obs[1] != obs1.h {
		t.Fatalf("registry writer=%v observers=%v, want the demoted holder first, ahead of bob", w, obs)
	}
	if writer.h.writable.Load() || !writer.h.waitRemote.Load() || writer.h.lease.held.Load() {
		t.Errorf("demoted holder writable=%v waitRemote=%v held=%v, want an observer waiting for the lease", writer.h.writable.Load(), writer.h.waitRemote.Load(), writer.h.lease.held.Load())
	}

	// A holder that is no longer the writer is not re-queued a second time.
	srv.demoteWriter(leaseCovRun, obs1.h)
	if _, obs := leaseCovState(srv, leaseCovRun); len(obs) != 2 {
		t.Errorf("observers = %d after demoting a non-writer, want the queue unchanged", len(obs))
	}
	if !obs1.h.waitRemote.Load() {
		t.Error("a demoted holder was not marked as waiting for the lease")
	}
}

func TestLeaseCovKeeperStartedByTheFirstClientRenewsItsLease(t *testing.T) {
	leases := newLeaseCovLeases()
	leases.renewed = make(chan struct{}, 8)
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{keeper: true})
	alice := newLeaseCovClient("alice", attachSourceWeb)
	srv.registerAttachHolder(leaseCovRun, alice.h)

	select {
	case <-leases.renewed:
	case <-time.After(30 * time.Second):
		t.Fatal("the keeper never renewed the writer's lease")
	}

	if calls := leases.renewCalls(); calls[0] != (leaseCovRenew{leaseCovRun, alice.h.id, attachLeaseTTL}) {
		t.Errorf("first renew = %+v, want alice's client with the standard ttl", calls[0])
	}
}

func TestLeaseCovKeeperStopsWhenItsContextEnds(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice := newLeaseCovClient("alice", attachSourceWeb)
	srv.registerAttachHolder(leaseCovRun, alice.h)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})

	go func() {
		srv.attachLeaseKeeper(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the keeper kept running after its context ended")
	}
}

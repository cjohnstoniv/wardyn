// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// leaseCovPair is one replica with alice as the writer and bob queued behind her, and the logs a bus
// whose database is gone leaves of the notices it was asked to send.
type leaseCovPair struct {
	srv          *Server
	leases       *leaseCovLeases
	aud          *leaseCovAudit
	logs         *leaseCovLogs
	alice, bob   *leaseCovClient
	releaseAlice func() func()
}

func newLeaseCovPair(t *testing.T) *leaseCovPair {
	t.Helper()
	p := &leaseCovPair{logs: leaseCovCaptureLogs(t), leases: newLeaseCovLeases()}
	p.srv, p.aud, _ = leaseCovServer(t, p.leases, leaseCovServerOpts{bus: "replica-a"})
	p.alice, p.bob = newLeaseCovClient("alice", attachSourceWeb), newLeaseCovClient("bob", attachSourceWeb)
	_, p.releaseAlice = p.srv.registerAttachHolder(leaseCovRun, p.alice.h)
	return p
}

func (p *leaseCovPair) joinBob(t *testing.T) {
	t.Helper()
	if ro, _ := p.srv.registerAttachHolder(leaseCovRun, p.bob.h); !ro {
		t.Fatal("bob was admitted as a second writer")
	}
}

// notices is the kinds of the notices sent so far, once the background senders are done.
func (p *leaseCovPair) notices() []string {
	p.srv.WaitBackground()
	return leaseCovNoticeKinds(p.logs)
}

func leaseCovSameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestLeaseCovReleasingTheOnlyWriterFreesTheLeaseAndTellsTheOtherReplicas(t *testing.T) {
	p := newLeaseCovPair(t)

	announce := p.releaseAlice()

	if announce != nil {
		t.Error("a promotion was announced though nobody was queued")
	}
	if _, ok := p.leases.row(leaseCovRun); ok {
		t.Error("the departed writer's lease is still in the store")
	}
	if rel := p.leases.releaseCalls(); len(rel) != 1 || rel[0] != p.alice.h.id {
		t.Errorf("release calls = %v, want alice's client once", rel)
	}
	if got := p.notices(); !leaseCovSameStrings(got, []string{livebus.KindAttachFree}) {
		t.Errorf("notices = %v, want one attach_free so other replicas' observers can take the slot", got)
	}
}

func TestLeaseCovReleaseHandsTheLeaseToTheNextLocalObserver(t *testing.T) {
	p := newLeaseCovPair(t)
	p.joinBob(t)

	announce := p.releaseAlice()

	if announce == nil {
		t.Fatal("no promotion was announced")
	}
	acq := p.leases.acquireCalls()
	last := acq[len(acq)-1]
	if last.From != p.alice.h.id || last.Lease.HolderID != p.bob.h.id || last.Lease.Principal != "bob" {
		t.Errorf("handoff acquire = from %s for %s/%s, want bob's client taking alice's lease directly", last.From, last.Lease.HolderID, last.Lease.Principal)
	}
	if row, _ := p.leases.row(leaseCovRun); row.HolderID != p.bob.h.id {
		t.Errorf("lease holder = %s, want bob's client", row.HolderID)
	}
	if rel := p.leases.releaseCalls(); len(rel) != 0 {
		t.Errorf("release calls = %v, want none: the lease moved to bob in one step", rel)
	}
	if w, obs := leaseCovState(p.srv, leaseCovRun); w != p.bob.h || len(obs) != 0 || !p.bob.h.canWrite() {
		t.Errorf("registry writer=%v observers=%d canWrite=%v, want bob the writer", w, len(obs), p.bob.h.canWrite())
	}
	announce()
	leaseCovExpectPromoteRow(t, p.aud, "bob", "alice")
	p.bob.promotedNotice(t)
	if got := p.notices(); len(got) != 0 {
		t.Errorf("notices = %v, want none while the slot stayed taken", got)
	}
}

func TestLeaseCovAPromotionThatCannotTakeTheLeaseDemotesTheObserverAndSendsNoPromotion(t *testing.T) {
	boom := errors.New("lease table unreachable")
	cases := []struct {
		name      string
		prepare   func(p *leaseCovPair)
		wantKinds []string
		wantRow   bool
	}{
		{"the lease was taken over by someone else", func(p *leaseCovPair) {
			p.leases.put(leaseCovRun, uuid.MustParse("00000000-0000-4000-8000-00000000f00d"), "mallory", attachSourceWeb)
		}, nil, true},
		{"the acquire cannot be made", func(p *leaseCovPair) { p.leases.fail("acquire", boom) }, []string{livebus.KindAttachFree}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newLeaseCovPair(t)
			p.joinBob(t)
			tc.prepare(p)

			announce := p.releaseAlice()

			if announce != nil {
				t.Error("a promotion was announced though bob never got the lease")
			}
			w, obs := leaseCovState(p.srv, leaseCovRun)
			if w != nil || len(obs) != 1 || obs[0] != p.bob.h {
				t.Fatalf("registry writer=%v observers=%v, want bob put back in the queue and the slot empty", w, obs)
			}
			if p.bob.h.writable.Load() || !p.bob.h.waitRemote.Load() || p.bob.h.canWrite() {
				t.Errorf("bob writable=%v waitRemote=%v canWrite=%v, want an observer waiting for the lease", p.bob.h.writable.Load(), p.bob.h.waitRemote.Load(), p.bob.h.canWrite())
			}
			if rel := p.leases.releaseCalls(); len(rel) != 1 || rel[0] != p.alice.h.id {
				t.Errorf("release calls = %v, want alice's own lease released once", rel)
			}
			if _, found := p.leases.row(leaseCovRun); found != tc.wantRow {
				t.Errorf("lease row present = %v, want %v", found, tc.wantRow)
			}
			if got := p.notices(); !leaseCovSameStrings(got, tc.wantKinds) {
				t.Errorf("notices = %v, want %v", got, tc.wantKinds)
			}
		})
	}
}

func TestLeaseCovATakeOverPromotesTheTakersOwnObserverWithTheLease(t *testing.T) {
	p := newLeaseCovPair(t)
	p.joinBob(t)

	prev, announce := p.srv.evictAttachWriter(leaseCovRun, "bob", nil)

	if prev != p.alice.h || !p.alice.h.evicted.Load() {
		t.Fatalf("evicted %v (evicted flag %v), want alice's client", prev, p.alice.h.evicted.Load())
	}
	if res := p.leases.reserveCalls(); len(res) != 0 {
		t.Errorf("reserve calls = %+v, want none: the taker had a socket to take the slot in place", res)
	}
	if row, _ := p.leases.row(leaseCovRun); row.HolderID != p.bob.h.id {
		t.Errorf("lease holder = %s, want bob's client", row.HolderID)
	}
	if announce == nil {
		t.Fatal("no promotion was announced")
	}
	announce()
	leaseCovExpectPromoteRow(t, p.aud, "bob", "alice")
	p.bob.promotedNotice(t)
}

func TestLeaseCovATakeOverWithNoSocketOfTheTakersReservesTheSlotForTheirClient(t *testing.T) {
	p := newLeaseCovPair(t)
	carol, dave := newLeaseCovClient("carol", attachSourceWeb), newLeaseCovClient("dave", attachSourceWeb)
	p.srv.registerAttachHolder(leaseCovRun, carol.h)

	prev, announce := p.srv.evictAttachWriter(leaseCovRun, "bob", nil)

	if prev != p.alice.h || announce != nil {
		t.Fatalf("evicted %v with announce=%v, want alice's client and no promotion", prev, announce != nil)
	}
	want := leaseCovReserve{leaseCovRun, p.alice.h.id, "bob", p.srv.replicaName(), attachLeaseTTL}
	if res := p.leases.reserveCalls(); len(res) != 1 || res[0] != want {
		t.Fatalf("reserve calls = %+v, want %+v", res, want)
	}
	row, _ := p.leases.row(leaseCovRun)
	if row.Source != attachLeaseReservedSource || row.Principal != "bob" {
		t.Fatalf("lease row = %+v, want a reservation for bob", row)
	}
	if rel := p.leases.releaseCalls(); len(rel) != 0 {
		t.Errorf("release calls = %v, want none: the slot is kept for the taker", rel)
	}
	if w, obs := leaseCovState(p.srv, leaseCovRun); w != nil || len(obs) != 1 || obs[0] != carol.h || carol.h.writable.Load() {
		t.Errorf("registry writer=%v observers=%v, want the bystander carol still an observer", w, obs)
	}

	// Another person's client cannot take the reserved slot; the taker's own can.
	if ro, _ := p.srv.registerAttachHolder(leaseCovRun, dave.h); !ro || dave.h.canWrite() {
		t.Errorf("dave: readOnly=%v canWrite=%v, want a read-only client against bob's reservation", ro, dave.h.canWrite())
	}
	if ro, _ := p.srv.registerAttachHolder(leaseCovRun, p.bob.h); ro || !p.bob.h.canWrite() {
		t.Errorf("bob: readOnly=%v canWrite=%v, want the taker to get the slot reserved for them", ro, p.bob.h.canWrite())
	}
	if row, _ := p.leases.row(leaseCovRun); row.HolderID != p.bob.h.id || row.Source != attachSourceWeb {
		t.Errorf("lease row = %+v, want bob's client holding it", row)
	}
}

func TestLeaseCovATakeOverFallsBackToReleasingTheLeaseWhenItCannotReserve(t *testing.T) {
	boom := errors.New("lease table unreachable")
	cases := []struct {
		name      string
		prepare   func(p *leaseCovPair)
		wantKinds []string
		wantRow   bool
	}{
		{"the reservation errors", func(p *leaseCovPair) { p.leases.fail("reserve", boom) }, []string{livebus.KindAttachFree}, false},
		{"the lease had already lapsed", func(p *leaseCovPair) { p.leases.advance(attachLeaseTTL + time.Second) }, []string{livebus.KindAttachFree}, false},
		{"neither the reservation nor the release can be made", func(p *leaseCovPair) {
			p.leases.fail("reserve", boom)
			p.leases.fail("release", boom)
		}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newLeaseCovPair(t)
			tc.prepare(p)

			prev, announce := p.srv.evictAttachWriter(leaseCovRun, "bob", nil)

			if prev != p.alice.h || announce != nil || p.alice.h.lease.held.Load() {
				t.Fatalf("evicted %v announce=%v held=%v, want alice's client evicted, nothing promoted and her lease given up", prev, announce != nil, p.alice.h.lease.held.Load())
			}
			if rel := p.leases.releaseCalls(); len(rel) != 1 || rel[0] != p.alice.h.id {
				t.Errorf("release calls = %v, want alice's lease released once", rel)
			}
			if row, found := p.leases.row(leaseCovRun); found != tc.wantRow || (found && row.Source == attachLeaseReservedSource) {
				t.Errorf("lease row = %+v (present %v), want present=%v and never a reservation", row, found, tc.wantRow)
			}
			if got := p.notices(); !leaseCovSameStrings(got, tc.wantKinds) {
				t.Errorf("notices = %v, want %v", got, tc.wantKinds)
			}
		})
	}
}

func TestLeaseCovALostLeaseEndsTheClientThatStillHasTheSlot(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})

	// A holder that already left the registry has nobody to displace.
	gone := newLeaseCovClient("gone", attachSourceWeb)
	srv.bindAttachLease(leaseCovRun, gone.h)
	gone.h.lease.held.Store(true)
	srv.attachLeaseLost(leaseCovRun, gone.h, "reason")
	if gone.h.lease.held.Load() || gone.wasDisplaced() {
		t.Errorf("a holder not in the registry: held=%v displaced=%v, want the lease marked lost and nobody displaced", gone.h.lease.held.Load(), gone.wasDisplaced())
	}

	// A writer with no lease (a store without leases) is still ended and told why.
	plain := newLeaseCovClient("plain", attachSourceWeb)
	plain.h.writable.Store(true)
	leaseCovSeed(srv, leaseCovRun, plain.h)
	srv.attachLeaseLost(leaseCovRun, plain.h, "closing for maintenance")
	if reason := plain.displacedWith(t); reason != "closing for maintenance" {
		t.Errorf("displaced with %q, want the reason given", reason)
	}
	if w, _ := leaseCovState(srv, leaseCovRun); w != nil || !plain.h.evicted.Load() || plain.h.canWrite() {
		t.Errorf("writer=%v evicted=%v canWrite=%v, want the writer removed and unable to write", w, plain.h.evicted.Load(), plain.h.canWrite())
	}
}

func TestLeaseCovARemoteTakeOverNoticeEndsOnlyTheNamedLocalWriter(t *testing.T) {
	notice := func(h uuid.UUID) attachEvictNotice { return attachEvictNotice{Holder: h, Reason: "taken over by bob"} }
	leases := newLeaseCovLeases()
	srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	alice := newLeaseCovClient("alice", attachSourceWeb)
	srv.registerAttachHolder(leaseCovRun, alice.h)

	srv.attachEvictedRemotely(leaseCovRun, notice(uuid.MustParse("00000000-0000-4000-8000-00000000f00d")))
	srv.attachEvictedRemotely(leaseCovRun, notice(uuid.Nil))
	srv.attachEvictedRemotely(uuid.MustParse("00000000-0000-4000-8000-00000000c0e1"), notice(alice.h.id))
	if w, _ := leaseCovState(srv, leaseCovRun); w != alice.h || alice.wasDisplaced() || alice.h.evicted.Load() {
		t.Fatalf("a notice about someone else's holder ended alice (writer=%v displaced=%v)", w, alice.wasDisplaced())
	}

	srv.attachEvictedRemotely(leaseCovRun, notice(alice.h.id))

	if reason := alice.displacedWith(t); reason != "taken over by bob" {
		t.Errorf("displaced with %q, want the reason the other replica sent", reason)
	}
	if w, _ := leaseCovState(srv, leaseCovRun); w != nil || !alice.h.evicted.Load() || alice.h.canWrite() {
		t.Errorf("writer=%v evicted=%v canWrite=%v, want alice removed and unable to write", w, alice.h.evicted.Load(), alice.h.canWrite())
	}
	if alice.h.lease.held.Load() {
		t.Error("alice still believes she holds the lease")
	}

	// A writer with no id (a store with no leases) is never named by a notice.
	plain := newLeaseCovClient("plain", attachSourceWeb)
	plain.h.writable.Store(true)
	leaseCovSeed(srv, leaseCovRun, plain.h)
	srv.attachEvictedRemotely(leaseCovRun, notice(uuid.Nil))
	if w, _ := leaseCovState(srv, leaseCovRun); w != plain.h || plain.wasDisplaced() {
		t.Error("a notice with a nil holder id ended a writer that has no id")
	}
}

func TestLeaseCovPromoteTakerObserverPicksTheTakersOldestObserver(t *testing.T) {
	srv, _, _ := leaseCovServer(t, newLeaseCovLeases(), leaseCovServerOpts{})
	other, bob1, bob2 := newLeaseCovClient("carol", attachSourceWeb), newLeaseCovClient("bob", attachSourceWeb), newLeaseCovClient("bob", attachSourceWeb)
	bob1.h.waitRemote.Store(true)
	writer := newLeaseCovClient("alice", attachSourceWeb)
	otherRun := uuid.MustParse("00000000-0000-4000-8000-00000000c0e2")
	leaseCovSeed(srv, otherRun, writer.h, bob1.h)

	if got := srv.promoteTakerObserver(uuid.MustParse("00000000-0000-4000-8000-00000000c0e3"), "bob"); got != nil {
		t.Errorf("promoted %v for a run nobody is attached to", got)
	}
	if got := srv.promoteTakerObserver(otherRun, "bob"); got != nil || bob1.h.writable.Load() {
		t.Errorf("promoted %v while the slot was taken", got)
	}
	leaseCovSeed(srv, leaseCovRun, nil, newLeaseCovClient("", attachSourceWeb).h)
	if got := srv.promoteTakerObserver(leaseCovRun, ""); got != nil {
		t.Errorf("promoted %v for no taker", got)
	}
	leaseCovSeed(srv, leaseCovRun, nil, other.h, bob1.h, bob2.h)
	if got := srv.promoteTakerObserver(leaseCovRun, "erin"); got != nil {
		t.Errorf("promoted %v for a taker with no observer", got)
	}

	got := srv.promoteTakerObserver(leaseCovRun, "bob")

	if got != bob1.h || !bob1.h.writable.Load() || bob1.h.waitRemote.Load() {
		t.Fatalf("promoted %v (writable=%v waitRemote=%v), want bob's oldest observer promoted and no longer waiting", got, bob1.h.writable.Load(), bob1.h.waitRemote.Load())
	}
	w, obs := leaseCovState(srv, leaseCovRun)
	if w != bob1.h || len(obs) != 2 || obs[0] != other.h || obs[1] != bob2.h {
		t.Errorf("registry writer=%v observers=%v, want the bystander and bob's second socket left in order", w, obs)
	}
	if bob2.h.writable.Load() || other.h.writable.Load() {
		t.Error("an observer that was not picked was promoted")
	}
}

// leaseCovRemote is replica A, where alice holds the lease, and replica B, where a take-over by bob is served.
type leaseCovRemote struct {
	leases       *leaseCovLeases
	srvA, srvB   *Server
	audB         *leaseCovAudit
	logs         *leaseCovLogs
	alice, bob   *leaseCovClient
	reservedByID uuid.UUID
}

func newLeaseCovRemote(t *testing.T) *leaseCovRemote {
	t.Helper()
	r := &leaseCovRemote{logs: leaseCovCaptureLogs(t), leases: newLeaseCovLeases(), reservedByID: uuid.UUID{0: 0xEE, 15: 1}}
	r.srvA, _, _ = leaseCovServer(t, r.leases, leaseCovServerOpts{})
	r.srvB, r.audB, _ = leaseCovServer(t, r.leases, leaseCovServerOpts{bus: "replica-b"})
	r.alice, r.bob = newLeaseCovClient("alice", attachSourceSSH), newLeaseCovClient("bob", attachSourceWeb)
	r.srvA.registerAttachHolder(leaseCovRun, r.alice.h)
	return r
}

func (r *leaseCovRemote) bobQueuesOnB(t *testing.T) {
	t.Helper()
	if ro, _ := r.srvB.registerAttachHolder(leaseCovRun, r.bob.h); !ro {
		t.Fatal("bob was admitted as a writer on replica B")
	}
}

func TestLeaseCovARemoteTakeOverReplacesTheLeaseWithAReservationAndNamesTheDisplacedHolder(t *testing.T) {
	r := newLeaseCovRemote(t)

	prev, announce := r.srvB.evictRemoteAttachHolder(leaseCovRun, "bob")

	if prev == nil || prev.principal != "alice" || prev.source != attachSourceSSH || !prev.since.Equal(r.alice.h.since) {
		t.Fatalf("displaced holder = %+v, want a stand-in naming alice over ssh since she attached", prev)
	}
	if prev.displace == nil {
		t.Fatal("the stand-in has no displace to call")
	}
	if announce != nil {
		t.Error("a promotion was announced though bob has no socket on replica B")
	}
	row, _ := r.leases.row(leaseCovRun)
	if row.Source != attachLeaseReservedSource || row.Principal != "bob" || row.Replica != r.srvB.replicaName() {
		t.Errorf("lease row = %+v, want a reservation for bob served by replica B", row)
	}
	r.srvB.WaitBackground()
	if got := leaseCovNoticeKinds(r.logs); !leaseCovSameStrings(got, []string{livebus.KindAttachEvict}) {
		t.Errorf("notices = %v, want one attach_evict for the replica that serves alice", got)
	}

	// Alice's replica finds out on its next check, and her client can no longer write.
	r.alice.h.lease.verified.Store(0)
	if r.alice.h.canWrite() {
		t.Error("alice can still write after her lease was replaced")
	}
	if reason := r.alice.displacedWith(t); reason != attachLeaseLostReason {
		t.Errorf("alice was displaced with %q, want %q", reason, attachLeaseLostReason)
	}
}

func TestLeaseCovARemoteTakeOverPromotesTheTakersObserverOnThisReplica(t *testing.T) {
	r := newLeaseCovRemote(t)
	r.bobQueuesOnB(t)

	prev, announce := r.srvB.evictRemoteAttachHolder(leaseCovRun, "bob")

	if prev == nil || prev.principal != "alice" {
		t.Fatalf("displaced holder = %+v, want alice's stand-in", prev)
	}
	if w, obs := leaseCovState(r.srvB, leaseCovRun); w != r.bob.h || len(obs) != 0 || !r.bob.h.canWrite() {
		t.Fatalf("replica B registry writer=%v observers=%d canWrite=%v, want bob the writer", w, len(obs), r.bob.h.canWrite())
	}
	acq := r.leases.acquireCalls()
	last := acq[len(acq)-1]
	if last.From != r.reservedByID || last.Lease.HolderID != r.bob.h.id {
		t.Errorf("handoff acquire = from %s for %s, want bob's client taking the reservation %s", last.From, last.Lease.HolderID, r.reservedByID)
	}
	if row, _ := r.leases.row(leaseCovRun); row.HolderID != r.bob.h.id || row.Source != attachSourceWeb {
		t.Errorf("lease row = %+v, want bob's client holding it as a web client", row)
	}
	if announce == nil {
		t.Fatal("no promotion was announced")
	}
	announce()
	leaseCovExpectPromoteRow(t, r.audB, "bob", "alice")
	r.bob.promotedNotice(t)
}

func TestLeaseCovARemoteTakeOverDoesNotPromoteWhenTheReservationIsLost(t *testing.T) {
	r := newLeaseCovRemote(t)
	r.bobQueuesOnB(t)
	intruder := uuid.MustParse("00000000-0000-4000-8000-00000000f00d")
	r.leases.beforeAcquire = func() { r.leases.put(leaseCovRun, intruder, "mallory", attachSourceWeb) }

	prev, announce := r.srvB.evictRemoteAttachHolder(leaseCovRun, "bob")

	if prev == nil || announce != nil {
		t.Fatalf("prev=%v announce=%v, want the displaced holder named and no promotion", prev, announce != nil)
	}
	w, obs := leaseCovState(r.srvB, leaseCovRun)
	if w != nil || len(obs) != 1 || obs[0] != r.bob.h || r.bob.h.writable.Load() || !r.bob.h.waitRemote.Load() {
		t.Errorf("replica B registry writer=%v observers=%v writable=%v, want bob demoted to waiting", w, obs, r.bob.h.writable.Load())
	}
	if rel := r.leases.releaseCalls(); len(rel) != 1 || rel[0] != r.reservedByID {
		t.Errorf("release calls = %v, want only the lost reservation's id tried", rel)
	}
	if row, _ := r.leases.row(leaseCovRun); row.HolderID != intruder {
		t.Errorf("lease row = %+v, want the intruder's lease untouched", row)
	}
}

func TestLeaseCovARemoteTakeOverWithNoLiveLeaseTakesOverNothing(t *testing.T) {
	boom := errors.New("lease table unreachable")
	cases := []struct {
		name    string
		prepare func(r *leaseCovRemote)
	}{
		{"the store cannot reserve", func(r *leaseCovRemote) { r.leases.fail("reserve", boom) }},
		{"the lease lapsed", func(r *leaseCovRemote) { r.leases.advance(attachLeaseTTL + time.Second) }},
		{"nobody held a lease", func(r *leaseCovRemote) { r.leases.mu.Lock(); r.leases.rows = nil; r.leases.mu.Unlock() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLeaseCovRemote(t)
			tc.prepare(r)
			before, _ := r.leases.row(leaseCovRun)

			prev, announce := r.srvB.evictRemoteAttachHolder(leaseCovRun, "bob")

			if prev != nil || announce != nil {
				t.Errorf("prev=%v announce=%v, want nothing taken over", prev, announce != nil)
			}
			if after, _ := r.leases.row(leaseCovRun); after.Source == attachLeaseReservedSource || after.HolderID != before.HolderID {
				t.Errorf("lease row changed from %+v to %+v", before, after)
			}
			r.srvB.WaitBackground()
			if got := leaseCovNoticeKinds(r.logs); len(got) != 0 {
				t.Errorf("notices = %v, want none for a take-over that found nothing", got)
			}
		})
	}
}

func TestLeaseCovRemoteAttachHolderViewNamesOnlyAHolderThatHasTheTerminal(t *testing.T) {
	boom := errors.New("lease table unreachable")
	holder := uuid.MustParse("00000000-0000-4000-8000-00000000f00d")
	cases := []struct {
		name    string
		prepare func(l *leaseCovLeases)
		want    bool
	}{
		{"a live lease", func(l *leaseCovLeases) { l.put(leaseCovRun, holder, "alice", attachSourceSSH) }, true},
		{"no lease", func(*leaseCovLeases) {}, false},
		{"a store that cannot answer", func(l *leaseCovLeases) {
			l.put(leaseCovRun, holder, "alice", attachSourceSSH)
			l.fail("get", boom)
		}, false},
		{"a lapsed lease", func(l *leaseCovLeases) {
			l.put(leaseCovRun, holder, "alice", attachSourceSSH)
			l.advance(attachLeaseTTL + time.Second)
		}, false},
		{"a reservation for a taker, which nobody holds yet", func(l *leaseCovLeases) { l.put(leaseCovRun, holder, "bob", attachLeaseReservedSource) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leases := newLeaseCovLeases()
			srv, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
			tc.prepare(leases)

			v := srv.remoteAttachHolderView(t.Context(), leaseCovRun)

			if !tc.want {
				if v != nil {
					t.Errorf("view = %+v, want nobody", v)
				}
				return
			}
			if v == nil || !v.Held || v.Principal != "alice" || v.Source != attachSourceSSH || v.Since == nil || !v.Since.Equal(leaseCovEpoch) || v.Cols != 0 || v.Rows != 0 {
				t.Errorf("view = %+v, want alice over ssh since her lease began, with no geometry", v)
			}
		})
	}
}

// leaseCovHTTP is a replica with an operator-owned running run, for the two routes that read the lease.
func leaseCovHTTP(t *testing.T) (*Server, *leaseCovLeases, *leaseCovAudit) {
	t.Helper()
	leases := newLeaseCovLeases()
	srv, aud, be := leaseCovServer(t, leases, leaseCovServerOpts{})
	be.authzStore.mu.Lock()
	be.authzStore.runs[leaseCovRun] = types.AgentRun{ID: leaseCovRun, CreatedBy: "alice@example.com", OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-1"}
	be.authzStore.mu.Unlock()
	return srv, leases, aud
}

func TestLeaseCovHolderRouteNamesTheRemoteHolderOnlyWhileTheyHoldTheTerminal(t *testing.T) {
	srv, leases, _ := leaseCovHTTP(t)
	get := func() attachHolderView {
		w := do(t, srv, http.MethodGet, "/api/v1/runs/"+leaseCovRun.String()+"/attach/holder", adminToken, "")
		var v attachHolderView
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("GET holder = %d %s", w.Code, w.Body)
		}
		return v
	}
	if v := get(); v.Held {
		t.Errorf("view = %+v with no lease, want nobody", v)
	}
	holder := uuid.MustParse("00000000-0000-4000-8000-00000000f00d")
	leases.put(leaseCovRun, holder, "alice", attachSourceSSH)
	if v := get(); !v.Held || v.Principal != "alice" || v.Source != attachSourceSSH {
		t.Errorf("view = %+v, want alice over ssh through the lease", v)
	}
	leases.put(leaseCovRun, holder, "bob", attachLeaseReservedSource)
	if v := get(); v.Held {
		t.Errorf("view = %+v for a reservation, want nobody", v)
	}
}

func TestLeaseCovTakeOverRouteDisplacesARemoteHolderAndRefusesWhenNobodyHoldsTheLease(t *testing.T) {
	srv, leases, aud := leaseCovHTTP(t)
	post := func() (int, map[string]any, string) {
		w := do(t, srv, http.MethodPost, "/api/v1/runs/"+leaseCovRun.String()+"/attach/takeover", adminToken, "")
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body, errorReason(w)
	}

	code, _, reason := post()
	if code != http.StatusConflict || reason != reasonAttachTakeoverNoHolder {
		t.Fatalf("take-over with no holder = %d %q, want 409 %q", code, reason, reasonAttachTakeoverNoHolder)
	}
	if got := len(aud.snapshot()); got != 0 {
		t.Fatalf("%d audit rows for a take-over that took nothing", got)
	}

	holder := uuid.MustParse("00000000-0000-4000-8000-00000000f00d")
	leases.put(leaseCovRun, holder, "alice", attachSourceSSH)
	code, body, _ := post()
	if code != http.StatusOK || body["taken_over"] != true || body["previous_holder"] != "alice" || body["previous_source"] != attachSourceSSH || body["promoted"] != false {
		t.Fatalf("take-over of a remote holder = %d %v, want 200 naming alice over ssh, nobody promoted", code, body)
	}
	res := leases.reserveCalls()
	row, _ := leases.row(leaseCovRun)
	if len(res) != 2 || res[1].Taker == "" || row.Source != attachLeaseReservedSource || row.Principal != res[1].Taker {
		t.Fatalf("reserve calls = %+v, row = %+v, want the slot reserved for the person who took it over", res, row)
	}
	ev := aud.next(t)
	var d struct {
		PreviousHolder string `json:"previous_holder"`
		PreviousSource string `json:"previous_source"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil || ev.Action != "session.takeover" || ev.Actor != res[1].Taker || d.PreviousHolder != "alice" || d.PreviousSource != attachSourceSSH {
		t.Errorf("audit row = %s by %q data %s (%v), want session.takeover by the taker of alice's ssh terminal", ev.Action, ev.Actor, ev.Data, err)
	}
}

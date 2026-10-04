// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The attach lease (ha-l2.4). A run's terminal is shared, and the registry in attach_holder.go
// decides in this process who may type into it. With more than one replica that decision has to
// be shared: the writer slot is a lease row in Postgres (run_attach_leases), and a client that
// is the writer here is the writer everywhere or not at all.
//
// Everything local stays as it was: the queue of observers, in-place promotion, take-over and the
// stale-writer probe. The lease is layered on the writer slot:
//
//   - a client becomes a writer here only once it has taken the lease (acquireAttachLease); a
//     client that cannot, because another replica's holder has it, is an observer, and the
//     keeper promotes the oldest one when the lease is released or lapses;
//   - a promotion, a take-over or a release hands the lease on in the same step
//     (handoffAttachLease), so there is never a writer here without the lease;
//   - input and resize are fenced by the lease: canWrite asks the lease (attachLease.live), which
//     is true only while this holder's id is the row's holder, re-checked against Postgres at
//     least every attachLeaseFenceEvery. A holder whose lease was taken over, or that cannot reach
//     the database to prove it still has it, writes nothing;
//   - the keeper renews the leases this process holds, so a replica that dies lets its leases
//     lapse after attachLeaseTTL.
//
// A store with no lease table (a test double) keeps every holder's lease nil, and all of this is
// skipped.

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

const (
	// attachLeaseTTL is how long a lease lives unrenewed.
	attachLeaseTTL = 6 * time.Second
	// attachLeaseRenewEvery is the keeper's tick: it renews this process's leases and promotes
	// an observer when a lease is free.
	attachLeaseRenewEvery = 2 * time.Second
	// attachLeaseFenceEvery bounds how long a holder trusts its last confirmation of the lease.
	attachLeaseFenceEvery = time.Second
	// attachLeaseCallTimeout bounds one lease statement.
	attachLeaseCallTimeout = 2 * time.Second
	// attachLeaseReservedSource is the source of a lease row that reserves the slot for a take-over's taker.
	attachLeaseReservedSource = "reserved"
	// attachLeaseLostReason is the close reason of a client whose lease ended without a take-over.
	attachLeaseLostReason = "attach lease lost"
)

// attachLease is one holder's claim on the run's lease. The zero holder (nil lease) has none to
// check.
type attachLease struct {
	s     *Server
	st    store.AttachLeaseStore
	runID uuid.UUID
	h     *attachHolder
	// held: this holder took the lease and has not seen it end. verified is when it was last
	// confirmed, unix nanoseconds.
	held     atomic.Bool
	verified atomic.Int64
}

func (l *attachLease) confirm() {
	l.verified.Store(time.Now().UnixNano())
	l.held.Store(true)
}

// live reports whether the holder still has the lease, asking Postgres when the last
// confirmation is older than attachLeaseFenceEvery. It fails closed: a database that does not
// answer is false for this call, and a lease found gone ends the holder.
func (l *attachLease) live() bool {
	if l == nil {
		return true
	}
	if !l.held.Load() {
		return false
	}
	if time.Since(time.Unix(0, l.verified.Load())) < attachLeaseFenceEvery {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachLeaseCallTimeout)
	defer cancel()
	ok, err := l.st.HoldsAttachLease(ctx, l.runID, l.h.id)
	switch {
	case err != nil:
		return false
	case !ok:
		l.held.Store(false)
		go l.s.attachLeaseLost(l.runID, l.h, attachLeaseLostReason)
		return false
	}
	l.verified.Store(time.Now().UnixNano())
	return true
}

// attachLeaseStore is the store's lease record, nil when it keeps none.
func (s *Server) attachLeaseStore() store.AttachLeaseStore {
	st, _ := s.cfg.Store.(store.AttachLeaseStore)
	return st
}

// bindAttachLease gives h a lease for runID when the store keeps them, and starts the keeper.
func (s *Server) bindAttachLease(runID uuid.UUID, h *attachHolder) {
	st := s.attachLeaseStore()
	if st == nil {
		return
	}
	h.id = uuid.New()
	h.lease = &attachLease{s: s, st: st, runID: runID, h: h}
	s.leaseKeeperOnce.Do(func() { go s.attachLeaseKeeper(s.cfg.BaseCtx) })
}

// acquireAttachLease takes runID's lease for h when it is free, has lapsed or is held by from.
func (s *Server) acquireAttachLease(runID uuid.UUID, h *attachHolder, from uuid.UUID) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), attachLeaseCallTimeout)
	defer cancel()
	ok, err := h.lease.st.AcquireAttachLease(ctx, store.AttachLease{
		RunID: runID, HolderID: h.id, Replica: s.replicaName(), Principal: h.principal, Source: h.source, Since: h.since,
	}, from, attachLeaseTTL)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not take a run's attach lease; the client is read-only",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return false
	}
	if ok {
		h.lease.confirm()
	}
	return ok
}

// demoteWriter puts h, which holds the writer slot here without the lease, back at the front of
// the observers.
func (s *Server) demoteWriter(runID uuid.UUID, h *attachHolder) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	h.writable.Store(false)
	h.waitRemote.Store(true)
	if h.lease != nil {
		h.lease.held.Store(false)
	}
	if ra := reg.attaches[runID]; ra != nil && ra.writer == h {
		ra.writer = nil
		ra.observers = append([]*attachHolder{h}, ra.observers...)
	}
}

// handoffAttachLease moves runID's lease from prev, the writer that left or was evicted (nil for
// none), to promoted, the observer that took its slot (nil for none), and reports whether
// promoted keeps the slot. Without a promoted writer the lease is released and the other replicas
// told, so their observers may take it, unless taker (a take-over's taker, "" for none) is owed
// the slot: it is then reserved for taker's own client, as a local take-over leaves it free for
// them. It runs after the registry lock is dropped.
func (s *Server) handoffAttachLease(runID uuid.UUID, prev, promoted *attachHolder, taker string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), attachLeaseCallTimeout)
	defer cancel()
	if promoted != nil && promoted.lease != nil {
		from := uuid.Nil
		if prev != nil {
			from = prev.id
		}
		if s.acquireAttachLease(runID, promoted, from) {
			return true
		}
		s.demoteWriter(runID, promoted)
	}
	if prev != nil && prev.lease != nil {
		prev.lease.held.Store(false)
		if taker != "" {
			if _, _, found, err := prev.lease.st.ReserveAttachLease(ctx, runID, prev.id, taker, s.replicaName(), attachLeaseTTL); err == nil && found {
				return false
			}
		}
		if released, err := prev.lease.st.ReleaseAttachLease(ctx, runID, prev.id); err == nil && released {
			s.publishNotice(livebus.KindAttachFree, runID, nil)
		}
	}
	return false
}

// attachLeaseLost ends h, whose lease was taken over or lapsed: it loses the writer slot here
// and its client is displaced with reason.
func (s *Server) attachLeaseLost(runID uuid.UUID, h *attachHolder, reason string) {
	if h.lease != nil {
		h.lease.held.Store(false)
	}
	prev, _ := s.evictAttachWriter(runID, "", h)
	if prev == nil {
		return
	}
	prev.displace(reason)
}

// attachLeaseKeeper renews this process's leases and promotes a waiting observer when a lease is
// free, until ctx ends.
func (s *Server) attachLeaseKeeper(ctx context.Context) {
	t := time.NewTicker(attachLeaseRenewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.attachLeaseTick(ctx)
		}
	}
}

func (s *Server) attachLeaseTick(ctx context.Context) {
	type slot struct {
		run    uuid.UUID
		writer *attachHolder
	}
	var held []slot
	var waiting []uuid.UUID
	reg := s.attachRegistry()
	reg.mu.Lock()
	for run, ra := range reg.attaches {
		switch {
		case ra.writer != nil && ra.writer.lease != nil:
			held = append(held, slot{run, ra.writer})
		case ra.writer == nil && firstWaiting(ra) != nil:
			waiting = append(waiting, run)
		}
	}
	reg.mu.Unlock()
	for _, sl := range held {
		cctx, cancel := context.WithTimeout(ctx, attachLeaseCallTimeout)
		ok, err := sl.writer.lease.st.RenewAttachLease(cctx, sl.run, sl.writer.id, attachLeaseTTL)
		cancel()
		switch {
		case err != nil:
		case !ok:
			s.attachLeaseLost(sl.run, sl.writer, attachLeaseLostReason)
		default:
			sl.writer.lease.confirm()
		}
	}
	for _, run := range waiting {
		s.promoteFromLease(run)
	}
}

// firstWaiting is the oldest observer queued behind a lease another replica held: the one a free
// lease goes to. An observer left behind by a local take-over is not waiting for it, so the slot
// stays free for the taker.
func firstWaiting(ra *runAttach) *attachHolder {
	for _, o := range ra.observers {
		if o.lease != nil && o.waitRemote.Load() {
			return o
		}
	}
	return nil
}

// promoteFromLease gives a free lease to runID's oldest waiting observer here, promoting it in place.
func (s *Server) promoteFromLease(runID uuid.UUID) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	ra := reg.attaches[runID]
	var o *attachHolder
	if ra != nil && ra.writer == nil {
		o = firstWaiting(ra)
	}
	reg.mu.Unlock()
	if o == nil || !s.acquireAttachLease(runID, o, uuid.Nil) {
		return
	}
	reg.mu.Lock()
	ra = reg.attaches[runID]
	i := -1
	if ra != nil && ra.writer == nil {
		i = slices.Index(ra.observers, o)
	}
	if i < 0 {
		reg.mu.Unlock()
		o.lease.held.Store(false)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), attachLeaseCallTimeout)
		defer cancel()
		_, _ = o.lease.st.ReleaseAttachLease(ctx, runID, o.id)
		return
	}
	ra.observers, ra.writer = slices.Delete(ra.observers, i, i+1), o
	o.waitRemote.Store(false)
	o.promote()
	reg.mu.Unlock()
	if announce := s.announceAttachPromotion(runID, o, ""); announce != nil {
		go announce()
	}
}

// registerAttachLeaseNotices handles the two notices about leases other replicas send.
func (s *Server) registerAttachLeaseNotices(b *livebus.Bus) {
	b.Handle(livebus.KindAttachEvict, func(m livebus.Message) {
		var d attachEvictNotice
		if err := unmarshalNotice(m, &d); err != nil {
			return
		}
		s.goBackground(func() { s.attachEvictedRemotely(m.Run, d) })
	})
	b.Handle(livebus.KindAttachFree, func(m livebus.Message) {
		if m.Origin != b.Origin() {
			s.goBackground(func() { s.promoteFromLease(m.Run) })
		}
	})
}

// attachEvictNotice names the holder another replica took over and what its client is told.
type attachEvictNotice struct {
	Holder uuid.UUID `json:"h"`
	Reason string    `json:"why"`
}

// attachEvictedRemotely ends the local writer a take-over elsewhere named, when this process has it.
func (s *Server) attachEvictedRemotely(runID uuid.UUID, d attachEvictNotice) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	var w *attachHolder
	if ra := reg.attaches[runID]; ra != nil && ra.writer != nil && ra.writer.id == d.Holder && d.Holder != uuid.Nil {
		w = ra.writer
	}
	reg.mu.Unlock()
	if w != nil {
		s.attachLeaseLost(runID, w, d.Reason)
	}
}

// evictRemoteAttachHolder is the take-over of a writer another replica serves: it replaces the
// lease with a reservation for taker, tells that replica to displace the client, and promotes
// taker's own observer here, if it has one, exactly as a local take-over does. prev is a stand-in
// naming the displaced holder for the audit row. nil when no live lease is held.
func (s *Server) evictRemoteAttachHolder(runID uuid.UUID, taker string) (prev *attachHolder, announce func()) {
	st := s.attachLeaseStore()
	if st == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), attachLeaseCallTimeout)
	defer cancel()
	l, reservation, found, err := st.ReserveAttachLease(ctx, runID, uuid.Nil, taker, s.replicaName(), attachLeaseTTL)
	if err != nil || !found {
		return nil, nil
	}
	s.publishNotice(livebus.KindAttachEvict, runID, attachEvictNotice{Holder: l.HolderID, Reason: attachTakeoverReason(taker)})
	prev = &attachHolder{principal: l.Principal, source: l.Source, since: l.Since, displace: func(string) {}}
	promoted := s.promoteTakerObserver(runID, taker)
	if promoted != nil {
		held := &attachHolder{id: reservation, lease: &attachLease{st: st, runID: runID}}
		if !s.handoffAttachLease(runID, held, promoted, "") {
			promoted = nil
		}
	}
	return prev, s.announceAttachPromotion(runID, promoted, l.Principal)
}

// promoteTakerObserver makes taker's own observer here the writer, in place, when the slot is free.
func (s *Server) promoteTakerObserver(runID uuid.UUID, taker string) *attachHolder {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	ra := reg.attaches[runID]
	if ra == nil || ra.writer != nil || taker == "" {
		return nil
	}
	for i, o := range ra.observers {
		if o.principal == taker {
			ra.observers, ra.writer = slices.Delete(ra.observers, i, i+1), o
			o.waitRemote.Store(false)
			o.promote()
			return o
		}
	}
	return nil
}

// remoteAttachHolderView is the holder another replica's live lease names, nil when none.
func (s *Server) remoteAttachHolderView(ctx context.Context, runID uuid.UUID) *attachHolderView {
	st := s.attachLeaseStore()
	if st == nil {
		return nil
	}
	l, found, err := st.GetAttachLease(ctx, runID)
	if err != nil || !found || !l.Live || l.Source == attachLeaseReservedSource {
		return nil // a reservation is a slot held for the taker: nobody holds the terminal yet
	}
	since := l.Since
	return &attachHolderView{Held: true, Principal: l.Principal, Since: &since, Source: l.Source}
}

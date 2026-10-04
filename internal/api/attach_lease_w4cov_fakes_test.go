// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// leaseCovLeases models run_attach_leases the way store_attach_leases.go defines it, with a clock the
// test moves by hand, so a lapse is an explicit step. One instance shared by two servers is two
// replicas over one database.
type leaseCovLeases struct {
	mu   sync.Mutex
	now  time.Time
	rows map[uuid.UUID]store.AttachLease
	seq  byte
	errs map[string]error

	acquires []leaseCovAcquire
	renews   []leaseCovRenew
	releases []uuid.UUID
	reserves []leaseCovReserve
	holds    int

	// beforeAcquire runs, with no lock held, ahead of each AcquireAttachLease is evaluated.
	beforeAcquire func()
	// renewed gets a value each time RenewAttachLease is called, when set.
	renewed chan struct{}
}

type leaseCovAcquire struct {
	Lease store.AttachLease
	From  uuid.UUID
	TTL   time.Duration
}

type leaseCovRenew struct {
	Run, Holder uuid.UUID
	TTL         time.Duration
}

type leaseCovReserve struct {
	Run, From      uuid.UUID
	Taker, Replica string
	TTL            time.Duration
}

var leaseCovEpoch = time.Now().UTC().Truncate(time.Second)

func newLeaseCovLeases() *leaseCovLeases {
	return &leaseCovLeases{now: leaseCovEpoch, rows: map[uuid.UUID]store.AttachLease{}, errs: map[string]error{}}
}

func (l *leaseCovLeases) fail(method string, err error) {
	l.mu.Lock()
	l.errs[method] = err
	l.mu.Unlock()
}

func (l *leaseCovLeases) advance(d time.Duration) {
	l.mu.Lock()
	l.now = l.now.Add(d)
	l.mu.Unlock()
}

// put writes a live lease row held by holder, as another replica's client would have left it.
func (l *leaseCovLeases) put(run, holder uuid.UUID, principal, source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows[run] = store.AttachLease{
		RunID: run, HolderID: holder, Replica: "replica-x", Principal: principal, Source: source,
		Since: leaseCovEpoch, ExpiresAt: l.now.Add(attachLeaseTTL),
	}
}

func (l *leaseCovLeases) row(run uuid.UUID) (store.AttachLease, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.rows[run]
	return r, ok
}

func (l *leaseCovLeases) acquireCalls() []leaseCovAcquire {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]leaseCovAcquire(nil), l.acquires...)
}

func (l *leaseCovLeases) releaseCalls() []uuid.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]uuid.UUID(nil), l.releases...)
}

func (l *leaseCovLeases) reserveCalls() []leaseCovReserve {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]leaseCovReserve(nil), l.reserves...)
}

func (l *leaseCovLeases) renewCalls() []leaseCovRenew {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]leaseCovRenew(nil), l.renews...)
}

func (l *leaseCovLeases) holdsCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holds
}

func (l *leaseCovLeases) AcquireAttachLease(_ context.Context, lease store.AttachLease, from uuid.UUID, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	l.acquires = append(l.acquires, leaseCovAcquire{lease, from, ttl})
	hook := l.beforeAcquire
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.errs["acquire"]; err != nil {
		return false, err
	}
	cur, held := l.rows[lease.RunID]
	if held {
		free := !cur.ExpiresAt.After(l.now) || cur.HolderID == from || cur.HolderID == lease.HolderID ||
			(cur.Source == attachLeaseReservedSource && cur.Principal == lease.Principal)
		if !free {
			return false, nil
		}
		lease.Epoch = cur.Epoch + 1
	}
	lease.ExpiresAt = l.now.Add(ttl)
	l.rows[lease.RunID] = lease
	return true, nil
}

func (l *leaseCovLeases) RenewAttachLease(_ context.Context, run, holder uuid.UUID, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	l.renews = append(l.renews, leaseCovRenew{run, holder, ttl})
	ch := l.renewed
	err := l.errs["renew"]
	cur, held := l.rows[run]
	ok := err == nil && held && cur.HolderID == holder && cur.ExpiresAt.After(l.now)
	if ok {
		cur.ExpiresAt = l.now.Add(ttl)
		l.rows[run] = cur
	}
	l.mu.Unlock()
	if ch != nil {
		ch <- struct{}{}
	}
	return ok, err
}

func (l *leaseCovLeases) HoldsAttachLease(_ context.Context, run, holder uuid.UUID) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holds++
	if err := l.errs["holds"]; err != nil {
		return false, err
	}
	cur, held := l.rows[run]
	return held && cur.HolderID == holder && cur.ExpiresAt.After(l.now), nil
}

func (l *leaseCovLeases) ReleaseAttachLease(_ context.Context, run, holder uuid.UUID) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releases = append(l.releases, holder)
	if err := l.errs["release"]; err != nil {
		return false, err
	}
	if cur, held := l.rows[run]; held && cur.HolderID == holder {
		delete(l.rows, run)
		return true, nil
	}
	return false, nil
}

func (l *leaseCovLeases) ReserveAttachLease(_ context.Context, run, from uuid.UUID, taker, replica string, ttl time.Duration) (store.AttachLease, uuid.UUID, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserves = append(l.reserves, leaseCovReserve{run, from, taker, replica, ttl})
	if err := l.errs["reserve"]; err != nil {
		return store.AttachLease{}, uuid.Nil, false, err
	}
	cur, held := l.rows[run]
	if !held || !cur.ExpiresAt.After(l.now) || (from != uuid.Nil && cur.HolderID != from) {
		return store.AttachLease{}, uuid.Nil, false, nil
	}
	l.seq++
	reservation := uuid.UUID{0: 0xEE, 15: l.seq}
	l.rows[run] = store.AttachLease{
		RunID: run, HolderID: reservation, Replica: replica, Principal: taker, Source: attachLeaseReservedSource,
		Since: l.now, ExpiresAt: l.now.Add(ttl), Epoch: cur.Epoch + 1,
	}
	cur.Live = true
	return cur, reservation, true, nil
}

func (l *leaseCovLeases) GetAttachLease(_ context.Context, run uuid.UUID) (store.AttachLease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.errs["get"]; err != nil {
		return store.AttachLease{}, false, err
	}
	cur, held := l.rows[run]
	cur.Live = cur.ExpiresAt.After(l.now)
	return cur, held, nil
}

var _ store.AttachLeaseStore = (*leaseCovLeases)(nil)

// leaseCovBackend is a working store for the run lookups the attach handlers make, plus the lease table.
type leaseCovBackend struct {
	*authzStore
	*leaseCovLeases
}

// leaseCovAudit records audit rows and signals each one, so a test waits on the row, not on the clock.
type leaseCovAudit struct {
	mu     sync.Mutex
	events []types.AuditEvent
	ch     chan types.AuditEvent
}

func newLeaseCovAudit() *leaseCovAudit { return &leaseCovAudit{ch: make(chan types.AuditEvent, 64)} }

func (a *leaseCovAudit) Record(_ context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	a.events = append(a.events, ev)
	a.mu.Unlock()
	a.ch <- ev
	return nil
}

func (a *leaseCovAudit) snapshot() []types.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]types.AuditEvent(nil), a.events...)
}

// next is the next audit row, failing the test when none arrives.
func (a *leaseCovAudit) next(t *testing.T) types.AuditEvent {
	t.Helper()
	select {
	case ev := <-a.ch:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no audit row was recorded")
		return types.AuditEvent{}
	}
}

// leaseCovClient is one attach client: a holder that remembers how it was told about its mode.
type leaseCovClient struct {
	h         *attachHolder
	displaced chan string
	notified  chan bool
}

func newLeaseCovClient(principal, source string) *leaseCovClient {
	c := &leaseCovClient{displaced: make(chan string, 4), notified: make(chan bool, 4)}
	c.h = &attachHolder{
		principal: principal, actorType: types.ActorHuman, since: leaseCovEpoch, source: source,
		displace: func(reason string) { c.displaced <- reason },
		notify:   func(readOnly bool, _ *attachHolder) { c.notified <- readOnly },
	}
	return c
}

// displacedWith is the reason the client was displaced with, failing the test when it was not.
func (c *leaseCovClient) displacedWith(t *testing.T) string {
	t.Helper()
	select {
	case r := <-c.displaced:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was never displaced", c.h.principal)
		return ""
	}
}

func (c *leaseCovClient) wasDisplaced() bool { return len(c.displaced) > 0 }

// promotedNotice waits for the client's "you were promoted" notice.
func (c *leaseCovClient) promotedNotice(t *testing.T) {
	t.Helper()
	select {
	case readOnly := <-c.notified:
		if readOnly {
			t.Fatalf("%s was sent a read-only notice, want the promotion", c.h.principal)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was never told it was promoted", c.h.principal)
	}
}

// leaseCovLogs records the slog records taken while a test runs.
type leaseCovLogs struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *leaseCovLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *leaseCovLogs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *leaseCovLogs) WithGroup(string) slog.Handler            { return l }
func (l *leaseCovLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
	return nil
}

func leaseCovCaptureLogs(t *testing.T) *leaseCovLogs {
	t.Helper()
	l := &leaseCovLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// with is every record carrying message msg.
func (l *leaseCovLogs) with(msg string) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, r := range l.recs {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

func leaseCovAttr(r slog.Record, key string) slog.Value {
	var v slog.Value
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value
			return false
		}
		return true
	})
	return v
}

const leaseCovNoticeLost = "wardynd: a notice to the other replicas was not sent"

// leaseCovNoticeKinds is the kind of every notice a bus whose database is gone failed to send, in order.
func leaseCovNoticeKinds(l *leaseCovLogs) []string {
	var kinds []string
	for _, r := range l.with(leaseCovNoticeLost) {
		kinds = append(kinds, leaseCovAttr(r, "kind").String())
	}
	return kinds
}

// leaseCovServerOpts picks what a replica is wired with.
type leaseCovServerOpts struct {
	bus    string // a non-empty origin gives the replica a live bus whose database is gone
	keeper bool   // let the first registration start the keeper goroutine
}

// leaseCovServer is one replica over leases, with its own run store holding run (when not zero).
func leaseCovServer(t *testing.T, leases *leaseCovLeases, o leaseCovServerOpts) (*Server, *leaseCovAudit, *leaseCovBackend) {
	t.Helper()
	aud := newLeaseCovAudit()
	h := newHarness(t)
	be := &leaseCovBackend{authzStore: newAuthzStore(), leaseCovLeases: leases}
	cfg := baseTestConfig(h, be)
	cfg.Audit = aud
	if o.bus != "" {
		pool, err := pgxpool.New(t.Context(), "postgres://wardyn@127.0.0.1:1/wardyn?sslmode=disable")
		if err != nil {
			t.Fatalf("build the pool: %v", err)
		}
		pool.Close()
		cfg.LiveBus = livebus.New(pool, o.bus)
	}
	srv := New(cfg)
	// A notice still being sent must not log into the next test's capture.
	t.Cleanup(srv.WaitBackground)
	if !o.keeper {
		srv.leaseKeeperOnce.Do(func() {})
	}
	return srv, aud, be
}

// leaseCovState is run's writer and observers in the registry.
func leaseCovState(s *Server, run uuid.UUID) (writer *attachHolder, observers []*attachHolder) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if ra := reg.attaches[run]; ra != nil {
		return ra.writer, append([]*attachHolder(nil), ra.observers...)
	}
	return nil, nil
}

// leaseCovSeed puts clients in run's registry directly: writer (may be nil) and observers in order.
func leaseCovSeed(s *Server, run uuid.UUID, writer *attachHolder, observers ...*attachHolder) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.attaches == nil {
		reg.attaches = map[uuid.UUID]*runAttach{}
	}
	reg.attaches[run] = &runAttach{writer: writer, observers: observers}
}

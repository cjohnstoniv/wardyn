// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// WithIdentityShared dials its own connection: the dials are capped like the lock connections and
// its close is bounded. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A burst of guarded writes holds at most db.LockPoolMaxConns connections at once; the excess waits
// db.LockPoolAcquireWait and is refused as the lock pool refuses (db.ErrLockNoCapacity).
func TestPG_WithIdentitySharedCapsItsDials(t *testing.T) {
	oldMax, oldWait := db.LockPoolMaxConns, db.LockPoolAcquireWait
	db.LockPoolMaxConns, db.LockPoolAcquireWait = 2, 200*time.Millisecond
	t.Cleanup(func() { db.LockPoolMaxConns, db.LockPoolAcquireWait = oldMax, oldWait })

	// A fresh pool, so the slots are sized by the caps above.
	pool, err := pgxpool.NewWithConfig(context.Background(), runsPGPoolIsolated(t).Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st := store.NewPG(pool)
	signedIn(t, st, "sub-cap", "cap@corp.example", "")
	g := store.IdentityGuard{Principal: "sub-cap", Epoch: -1}

	var inFn, peak atomic.Int32
	release := make(chan struct{})
	hold := func() error {
		n := inFn.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-release
		inFn.Add(-1)
		return nil
	}
	const burst = 6
	errs := make(chan error, burst)
	var wg sync.WaitGroup
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			errs <- st.WithIdentityShared(ctx, g, hold)
		}()
	}
	for range burst - 2 {
		if err := <-errs; !errors.Is(err, db.ErrLockNoCapacity) {
			t.Fatalf("an excess call = %v, want db.ErrLockNoCapacity", err)
		}
	}
	close(release)
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a call within the cap: %v", err)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrent guarded connections = %d, want exactly the cap, 2", peak.Load())
	}
	// The slots are released: the next call is served.
	if err := st.WithIdentityShared(context.Background(), g, func() error { return nil }); err != nil {
		t.Fatalf("after the burst: %v", err)
	}
}

// stallConn is a server connection that stops taking writes once stalled is set, until its
// deadline passes or it is closed: a server that never answers a Terminate.
type stallConn struct {
	net.Conn
	stalled *atomic.Bool
	freed   chan struct{}
	once    sync.Once
}

func (c *stallConn) free() { c.once.Do(func() { close(c.freed) }) }

func (c *stallConn) SetDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		c.free()
	}
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		c.free()
	}
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) Close() error { c.free(); return c.Conn.Close() }

func (c *stallConn) Write(b []byte) (int, error) {
	if c.stalled.Load() {
		<-c.freed
		return 0, errors.New("stalled write abandoned")
	}
	return c.Conn.Write(b)
}

// Once fn is done, a server that never answers cannot hold the call: the close is abandoned at 2 s.
func TestPG_WithIdentitySharedAbandonsAStuckClose(t *testing.T) {
	cfg := runsPGPoolIsolated(t).Config()
	var stalled atomic.Bool
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallConn{Conn: c, stalled: &stalled, freed: make(chan struct{})}, nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() { stalled.Store(false) }) // lets the pool close its own connections
	st := store.NewPG(pool)
	signedIn(t, st, "sub-stuck", "stuck@corp.example", "")

	errFn := errors.New("fn refused")
	start := time.Now()
	err = st.WithIdentityShared(context.Background(), store.IdentityGuard{Principal: "sub-stuck", Epoch: -1}, func() error {
		stalled.Store(true)
		return errFn
	})
	elapsed := time.Since(start)
	if !errors.Is(err, errFn) {
		t.Fatalf("WithIdentityShared = %v, want fn's error", err)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("returned after %v, want the 2 s close budget", elapsed)
	}
}

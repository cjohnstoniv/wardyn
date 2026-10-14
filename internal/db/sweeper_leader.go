// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// SweeperLeader: the one replica that runs the sweepers which must run once.
//
// Election is SweeperLeaderLockKey, held for the process lifetime on a
// connection of its own, outside the pool. An advisory lock is not a fence: a Postgres failover releases it
// under a still-running leader while a follower takes over. So each term has a
// durable epoch (sweeper_leader.epoch, bumped on every acquisition), the leader
// watches its own lock connection and the epoch, and on loss it cancels and
// joins everything it started before it lets go. Work that writes in several
// steps carries its epoch and asks Current before each write that matters.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// sweeperLeaderRetry is how long a follower waits between attempts to take
	// the lock: a leader lost to a stopped process is replaced within one retry.
	sweeperLeaderRetry = 15 * time.Second
	// sweeperLeaderMonitor is how often the leader checks its lock connection
	// and epoch, so a lost lock stops its sweeps within a few seconds.
	sweeperLeaderMonitor = 5 * time.Second
	// sweeperLeaderPoll is how often a gated sweeper looks for a new term.
	sweeperLeaderPoll = time.Second
)

// SweeperLeader elects and monitors the sweeper leader. Build one with
// NewSweeperLeader, start Run once, and gate each sweeper with Go or Join.
type SweeperLeader struct {
	pool   *pgxpool.Pool
	holder string

	retry, monitor, poll time.Duration

	mu   sync.Mutex
	term *sweeperTerm
}

// sweeperTerm is one acquisition: its context ends when the lock is lost, and
// wg counts the sweeps that have to finish before the lock is released.
type sweeperTerm struct {
	ctx    context.Context
	cancel context.CancelFunc
	epoch  int64
	wg     sync.WaitGroup
}

// SweeperLeaderInfo is the durable record of the current leader, for the
// status reporting that tells a follower who leads.
type SweeperLeaderInfo struct {
	Epoch      int64
	Holder     string
	AcquiredAt time.Time
	// Self is true when this process holds the lease right now.
	Self bool
}

// NewSweeperLeader returns a leader for pool. holder names this process in
// sweeper_leader.holder (forensics and status only; the lock decides).
func NewSweeperLeader(pool *pgxpool.Pool, holder string) *SweeperLeader {
	return &SweeperLeader{pool: pool, holder: holder,
		retry: sweeperLeaderRetry, monitor: sweeperLeaderMonitor, poll: sweeperLeaderPoll}
}

// Run elects until ctx ends: take the lock, bump the epoch, lead, and on loss
// stop the sweeps and try again.
func (l *SweeperLeader) Run(ctx context.Context) {
	logged := ""
	for {
		switch outcome := l.lead(ctx); {
		case outcome == "" || outcome == "led":
			logged = ""
		case outcome != logged:
			logged = outcome
			slog.InfoContext(ctx, "wardynd: sweeper leader: not leading", slog.String("reason", outcome))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.retry):
		}
	}
}

// lead makes one attempt. It returns "led" after a term that ended, or why the
// attempt did not lead.
func (l *SweeperLeader) lead(ctx context.Context) string {
	// Probe on the pool first: a follower must not dial a dedicated session
	// every retry just to be told the leader has it. A probe that errors falls
	// through to the dedicated try, which reports the real fault — the probe
	// can only ever save the dial, never turn a working election into silence.
	if held, err := AdvisoryLockHeld(ctx, l.pool, SweeperLeaderLockKey); err == nil && held {
		return "another replica holds the lock"
	}
	conn, release, ok, err := TryAdvisoryLockDedicated(ctx, l.pool, SweeperLeaderLockKey)
	if err != nil {
		return "lock unavailable: " + err.Error()
	}
	if !ok {
		return "another replica holds the lock"
	}
	defer release()
	var epoch int64
	err = conn.QueryRow(ctx, `UPDATE sweeper_leader SET epoch = epoch + 1, holder = $1, acquired_at = now()
		WHERE id RETURNING epoch`, l.holder).Scan(&epoch)
	if err != nil {
		return "epoch bump failed: " + err.Error()
	}
	t := l.open(ctx, epoch)
	slog.InfoContext(ctx, "wardynd: sweeper leader: acquired the lease", slog.Int64("epoch", epoch))
	l.watch(ctx, conn, epoch)
	l.close(t)
	if ctx.Err() == nil {
		slog.WarnContext(ctx, "wardynd: sweeper leader: lost the lease; sweeps stopped", slog.Int64("epoch", epoch))
	}
	return "led"
}

// watch blocks until the lock connection fails, another term has bumped the
// epoch, or ctx ends. A hung connection counts as lost: each probe has a
// deadline of two monitor periods.
func (l *SweeperLeader) watch(ctx context.Context, conn *pgx.Conn, epoch int64) {
	tick := time.NewTicker(l.monitor)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		pctx, cancel := context.WithTimeout(ctx, 2*l.monitor)
		var cur int64
		err := conn.QueryRow(pctx, `SELECT epoch FROM sweeper_leader WHERE id`).Scan(&cur)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil || cur != epoch {
			return
		}
	}
}

func (l *SweeperLeader) open(ctx context.Context, epoch int64) *sweeperTerm {
	tctx, cancel := context.WithCancel(ctx)
	t := &sweeperTerm{ctx: tctx, cancel: cancel, epoch: epoch}
	l.mu.Lock()
	l.term = t
	l.mu.Unlock()
	return t
}

// close ends a term: no new sweep may join, the running ones are cancelled and
// waited for, and only then does the caller release the lock.
func (l *SweeperLeader) close(t *sweeperTerm) {
	l.mu.Lock()
	if l.term == t {
		l.term = nil
	}
	l.mu.Unlock()
	t.cancel()
	t.wg.Wait()
}

// Join joins the current term as one unit of leader work. ok is false on a
// follower. ctx ends when the lease is lost, and end must be called when the
// work is done: the leader waits for every end before it lets go of the lock.
// epoch is the term's, for Current.
func (l *SweeperLeader) Join() (ctx context.Context, epoch int64, end func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.term
	if t == nil {
		return nil, 0, nil, false
	}
	t.wg.Add(1)
	return t.ctx, t.epoch, sync.OnceFunc(t.wg.Done), true
}

// Go runs fn once per term this process leads, with the term's context, and
// returns when ctx ends. fn is expected to loop until its context is cancelled
// (the periodic sweepers) or to do its work once and return (a boot reconcile):
// either way it is not started again inside the same term.
func (l *SweeperLeader) Go(ctx context.Context, fn func(context.Context)) {
	for {
		tctx, _, end, ok := l.Join()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(l.poll):
			}
			continue
		}
		func() {
			defer end()
			fn(tctx)
			<-tctx.Done()
		}()
		if ctx.Err() != nil {
			return
		}
	}
}

// Current reports whether epoch is still the durable one: false once another
// term has acquired the lease, which is when a stale leader must stop writing.
func (l *SweeperLeader) Current(ctx context.Context, epoch int64) (bool, error) {
	var cur int64
	err := l.pool.QueryRow(ctx, `SELECT epoch FROM sweeper_leader WHERE id`).Scan(&cur)
	if err != nil {
		return false, fmt.Errorf("db: read sweeper epoch: %w", err)
	}
	return cur == epoch, nil
}

// Info reads the durable leader record. The row is seeded by 0110, so a missing
// row is an error.
func (l *SweeperLeader) Info(ctx context.Context) (SweeperLeaderInfo, error) {
	var info SweeperLeaderInfo
	var at *time.Time
	err := l.pool.QueryRow(ctx, `SELECT epoch, holder, acquired_at FROM sweeper_leader WHERE id`).
		Scan(&info.Epoch, &info.Holder, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, errors.New("db: sweeper_leader row is missing")
	}
	if err != nil {
		return info, fmt.Errorf("db: read sweeper leader: %w", err)
	}
	if at != nil {
		info.AcquiredAt = *at
	}
	l.mu.Lock()
	info.Self = l.term != nil && l.term.epoch == info.Epoch
	l.mu.Unlock()
	return info, nil
}

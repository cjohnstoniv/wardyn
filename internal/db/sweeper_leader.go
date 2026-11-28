// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// SweeperLeader: the replicas' share of the sweeps that must run once.
//
// The sweeps are split into four families (SweeperFamilies), and each family has a leader of its own, so replicas
// share the sweeps and a family that stalls is given up without stopping the others. A family is elected by a two-key
// advisory lock (SweeperFamilyLockClass, crc32 of its name), and every family a replica leads is held on the one
// connection the replica dials for the election, outside the pool. An advisory lock is not a fence: a Postgres
// failover releases it under a still-running leader while another replica takes over. So each family has a durable
// epoch (sweeper_families.epoch, bumped on every acquisition), the leader watches its connection and every epoch it
// holds, and on loss it cancels and joins that family's work before it lets go. The join is bounded
// (familyResignJoin): a unit that outlives it keeps its family's lock, and the family reads as stalled, until the unit
// returns. Work that writes in several steps carries its family's epoch and asks Current before each write that
// matters.
//
// An older replica elects one leader for every sweep on SweeperLeaderLockKey, taken exclusively. Every replica that
// leads a family holds that key in shared mode, so an older replica never leads beside a family leader, and every
// family acquisition bumps sweeper_leader.epoch too, so a write an older leader still has in flight is fenced.
//
// The watchdog in cmd/wardynd gives up a family (Resign) whose watched sweeps have all stopped succeeding. Join,
// Current and Go on the leader are the runs family's.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// sweeperLeaderRetry is how often a replica tries the families it does not lead, and the unit of the spreading
	// wait: a replica leading k families takes another only k retries after its last acquisition.
	sweeperLeaderRetry = 15 * time.Second
	// sweeperLeaderMonitor is how often the leader checks its election connection and every epoch it holds, so a
	// lost lock stops its sweeps within a few seconds.
	sweeperLeaderMonitor = 5 * time.Second
	// sweeperLeaderPoll is how often a gated sweeper looks for a new term.
	sweeperLeaderPoll = time.Second
)

// SweeperLeader elects and monitors this replica's sweep families. Build one with NewSweeperLeader, start Run once,
// and gate each sweeper with its family's Go or Join.
type SweeperLeader struct {
	pool   *pgxpool.Pool
	holder string

	retry, monitor, poll time.Duration
	resignJoin, cooldown time.Duration

	fams []*SweeperFamily // in SweeperFamilies order

	// mu guards every family's term state. It is never held while waiting for connMu.
	mu sync.Mutex

	// connMu serializes the statements on the election connection (Run's rounds and checks, and Resign's unlock),
	// and guards the fields below.
	connMu sync.Mutex
	conn   *pgx.Conn
	// broken: a statement on conn failed. No family is taken on it, and it is closed once every term has joined.
	broken bool
	// shared: conn holds SweeperLeaderLockKey in shared mode (the legacy rule).
	shared      bool
	lastAcquire time.Time
	runCtx      context.Context
	noted       string
}

// sweeperTerm is one acquisition of one family: its context ends when the term ends, wg counts the units that have
// to finish before the family's lock is released, and done is closed once they have.
type sweeperTerm struct {
	ctx    context.Context
	cancel context.CancelFunc
	epoch  int64
	wg     sync.WaitGroup
	done   chan struct{}
}

// SweeperLeaderInfo is the durable record of the runs family's leader, for the status reporting that tells a
// follower who leads.
type SweeperLeaderInfo struct {
	Epoch      int64
	Holder     string
	AcquiredAt time.Time
	// Self is true when this process holds the lease right now.
	Self bool
}

// NewSweeperLeader returns a leader for pool. holder names this process in sweeper_families.holder (forensics and
// status only; the lock decides).
func NewSweeperLeader(pool *pgxpool.Pool, holder string) *SweeperLeader {
	l := &SweeperLeader{pool: pool, holder: holder,
		retry: sweeperLeaderRetry, monitor: sweeperLeaderMonitor, poll: sweeperLeaderPoll,
		resignJoin: familyResignJoin, cooldown: familyResignCooldown}
	for _, name := range SweeperFamilies {
		l.fams = append(l.fams, newSweeperFamily(l, name))
	}
	return l
}

// Run elects until ctx ends: each round tries the families this replica may take, and between rounds the leader
// watches its connection and epochs. When ctx ends every term is ended and joined, bounded, and the connection is
// closed once every term on it has joined.
func (l *SweeperLeader) Run(ctx context.Context) {
	l.connMu.Lock()
	l.runCtx = ctx
	l.connMu.Unlock()
	defer l.shutdown()
	round := time.NewTimer(0)
	defer round.Stop()
	monitor := time.NewTicker(l.monitor)
	defer monitor.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-round.C:
			if l.round() {
				l.lose()
			}
			round.Reset(l.retry)
		case <-monitor.C:
			if l.check() {
				l.lose()
			}
		}
		l.logStalls()
	}
}

// Join joins the runs family's current term as one unit of leader work (SweeperFamily.Join).
func (l *SweeperLeader) Join() (ctx context.Context, epoch int64, end func(), ok bool) {
	return l.Family(SweeperFamilyRuns).Join()
}

// Go runs fn once per runs family term this process leads (SweeperFamily.Go).
func (l *SweeperLeader) Go(ctx context.Context, fn func(context.Context)) {
	l.Family(SweeperFamilyRuns).Go(ctx, fn)
}

// Current reports whether epoch is still the runs family's durable one (SweeperFamily.Current).
func (l *SweeperLeader) Current(ctx context.Context, epoch int64) (bool, error) {
	return l.Family(SweeperFamilyRuns).Current(ctx, epoch)
}

// Info reads the runs family's durable leader record. The row is seeded by its migration, so a missing row is an
// error.
func (l *SweeperLeader) Info(ctx context.Context) (SweeperLeaderInfo, error) {
	var info SweeperLeaderInfo
	var at *time.Time
	err := l.pool.QueryRow(ctx, `SELECT epoch, holder, acquired_at FROM sweeper_families WHERE family = $1`,
		SweeperFamilyRuns).Scan(&info.Epoch, &info.Holder, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, errors.New("db: the runs sweeper family row is missing")
	}
	if err != nil {
		return info, fmt.Errorf("db: read sweeper leader: %w", err)
	}
	if at != nil {
		info.AcquiredAt = *at
	}
	runs := l.Family(SweeperFamilyRuns)
	l.mu.Lock()
	info.Self = runs.term != nil && runs.term.epoch == info.Epoch
	l.mu.Unlock()
	return info, nil
}

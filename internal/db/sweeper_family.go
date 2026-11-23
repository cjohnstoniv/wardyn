// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The sweep families (SweeperLeader). Each family's sweeps run on one replica at a time, under the family's own lock
// and fencing epoch; cmd/wardynd names the family of every sweep it starts.
const (
	SweeperFamilyRuns        = "runs"
	SweeperFamilyCredentials = "credentials"
	SweeperFamilyAudit       = "audit"
	SweeperFamilyRecords     = "records"
)

// SweeperFamilies lists every family, in the order a round tries them.
var SweeperFamilies = []string{SweeperFamilyRuns, SweeperFamilyCredentials, SweeperFamilyAudit, SweeperFamilyRecords}

const (
	// familyResignJoin bounds the wait for a family's units once its term ends. A unit that outlives it keeps the
	// family's lock until it returns.
	familyResignJoin = 30 * time.Second
	// familyResignCooldown is how long a replica leaves a family it gave up, so another replica can take it.
	familyResignCooldown = 45 * time.Second
	// familyStallLogEvery is how often a stalled family is logged again while the stall lasts.
	familyStallLogEvery = time.Minute
	// electionDialWait bounds the dial of the election connection.
	electionDialWait = 10 * time.Second
	// electionStmtFloor is the shortest deadline a statement on the election connection gets.
	electionStmtFloor = time.Second
)

// SweeperFamily is one family of sweeps that must run once. Build it with SweeperLeader.Family; Run elects it.
type SweeperFamily struct {
	l    *SweeperLeader
	name string
	obj  int32 // crc32 of name: the object half of the family's lock

	// Guarded by l.mu.
	term       *sweeperTerm // the live term: nil when this process does not lead the family, or it is stalled
	last       *sweeperTerm // the latest term, whose units may still be returning
	locked     bool         // the election session holds the family's lock
	stalled    bool         // a term ended with a unit still running: the lock is kept until it returns
	stallLog   time.Time
	acquiredAt time.Time
	// prompt: the family is tried at the next round with no spreading wait, because the last try found it held or
	// this replica held it when its election session was lost.
	prompt   bool
	cooldown time.Time // not tried before this, after a resign
}

func newSweeperFamily(l *SweeperLeader, name string) *SweeperFamily {
	return &SweeperFamily{l: l, name: name, obj: NewLockKey(SweeperFamilyLockClass, name).Obj}
}

// Family returns the family named name. A name not in SweeperFamilies is a programming error and panics.
func (l *SweeperLeader) Family(name string) *SweeperFamily {
	for _, f := range l.fams {
		if f.name == name {
			return f
		}
	}
	panic("db: no sweeper family " + strconv.Quote(name))
}

// Stalled names the families whose sweep did not stop when their term ended, so their lock is kept.
func (l *SweeperLeader) Stalled() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, f := range l.fams {
		if f.stalled {
			out = append(out, f.name)
		}
	}
	return out
}

// Join joins the family's current term as one unit of leader work. ok is false when this process does not lead the
// family, or the family is stalled. ctx ends when the term ends, and end must be called when the work is done: the
// family's lock is not released before every end. epoch is the term's, for Current.
func (f *SweeperFamily) Join() (ctx context.Context, epoch int64, end func(), ok bool) {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()
	t := f.term
	if t == nil {
		return nil, 0, nil, false
	}
	t.wg.Add(1)
	return t.ctx, t.epoch, sync.OnceFunc(t.wg.Done), true
}

// Go runs fn once per term of the family this process leads, with the term's context, and returns when ctx ends.
// fn is expected to loop until its context is cancelled (the periodic sweepers) or to do its work once and return (a
// boot reconcile): either way it is not started again inside the same term.
func (f *SweeperFamily) Go(ctx context.Context, fn func(context.Context)) {
	for {
		tctx, _, end, ok := f.Join()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(f.l.poll):
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

// Current reports whether epoch is still the family's durable one: false once another term has acquired the family,
// which is when a stale leader must stop writing.
func (f *SweeperFamily) Current(ctx context.Context, epoch int64) (bool, error) {
	var cur int64
	err := f.l.pool.QueryRow(ctx, `SELECT epoch FROM sweeper_families WHERE family = $1`, f.name).Scan(&cur)
	if err != nil {
		return false, fmt.Errorf("db: read sweeper family epoch: %w", err)
	}
	return cur == epoch, nil
}

// Held reports whether this process leads the family with a live term, and when it acquired it.
func (f *SweeperFamily) Held() (acquiredAt time.Time, ok bool) {
	f.l.mu.Lock()
	defer f.l.mu.Unlock()
	if f.term == nil {
		return time.Time{}, false
	}
	return f.acquiredAt, true
}

// Resign gives the family up. It cancels the term and waits up to familyResignJoin for every unit that joined it.
// If they end in time, the family's lock is released and this replica does not try the family again for
// familyResignCooldown, so another replica can take it. If one does not, the lock is kept and the family is stalled
// (Join answers false for it, so Go starts nothing in it, and Stalled names it) until the unit returns; the next round
// then releases it. A family this process does not lead, or one already stalled, is left as it is.
func (f *SweeperFamily) Resign() {
	f.l.endFamilies([]*SweeperFamily{f}, true)
}

// end stops the live term taking new units, cancels it and returns it (nil when there is none). done is closed once
// its units have returned. l.mu held.
func (f *SweeperFamily) end() *sweeperTerm {
	t := f.term
	if t == nil {
		return nil
	}
	f.term = nil
	t.cancel()
	// Every Add happened under l.mu while f.term was t, so before this Wait.
	go func() { t.wg.Wait(); close(t.done) }()
	return t
}

// stall marks a family whose term t ended with a unit still running. l.mu held.
func (f *SweeperFamily) stall(t *sweeperTerm, bound time.Duration) {
	if !f.locked {
		return
	}
	f.stalled, f.stallLog = true, time.Now()
	slog.Error("wardynd: sweeper family stalled; its sweep did not stop, so its lock is kept",
		slog.String("family", f.name), slog.Int64("epoch", t.epoch), slog.Duration("bound", bound))
}

// joined reports whether t's units have all returned (no term has none).
func (t *sweeperTerm) joined() bool {
	if t == nil {
		return true
	}
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// join waits until deadline for t's units, and reports whether they all returned.
func (t *sweeperTerm) join(deadline time.Time) bool {
	if t.joined() {
		return true
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-t.done:
		return true
	case <-timer.C:
		return false
	}
}

// round is one election round. lost reports that the election connection failed, so Run ends every term (lose).
func (l *SweeperLeader) round() (lost bool) {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	if l.conn != nil && l.broken {
		if !l.allJoined() {
			l.note("a family's sweep is still stopping on a lost election connection")
			return false
		}
		l.closeConn()
	}
	if l.conn == nil {
		dctx, cancel := context.WithTimeout(l.runCtx, electionDialWait)
		conn, err := dialDedicated(dctx, l.pool)
		cancel()
		if err != nil {
			l.note("election connection unavailable: " + err.Error())
			return false
		}
		l.conn = conn
	}
	l.unlockJoinedStalls()
	if l.broken {
		return true
	}
	if !l.shared {
		var ok bool
		if err := l.query(&ok, `SELECT pg_try_advisory_lock_shared($1)`, SweeperLeaderLockKey); err != nil {
			l.broken = true
			return true
		}
		if !ok {
			// Only a replica of an older version takes the key exclusively. This replica leads no family while it
			// does, and it holds none now: shared is released only once it leads none.
			l.note("a replica of an older version holds the sweeper lock")
			return false
		}
		l.shared = true
	}
	k := l.lockedCount()
	for _, f := range l.fams {
		if !l.mayTry(f, k) {
			continue
		}
		var got bool
		if err := l.query(&got, `SELECT pg_try_advisory_lock($1, $2)`, SweeperFamilyLockClass, f.obj); err != nil {
			l.broken = true
			return true
		}
		if !got {
			l.mu.Lock()
			f.prompt = true
			l.mu.Unlock()
			continue
		}
		epoch, err := l.bump(f)
		if err != nil {
			slog.Warn("wardynd: sweeper leader: could not bump a family's epoch; letting the family go",
				slog.String("family", f.name), slog.Any("err", err))
			var freed bool
			if err := l.query(&freed, `SELECT pg_advisory_unlock($1, $2)`, SweeperFamilyLockClass, f.obj); err != nil {
				l.broken = true
				return true
			}
			continue
		}
		l.open(f, epoch)
		k++
		l.lastAcquire = time.Now()
	}
	if k == 0 {
		var freed bool
		if err := l.query(&freed, `SELECT pg_advisory_unlock_shared($1)`, SweeperLeaderLockKey); err != nil {
			l.broken = true
			return true
		}
		l.shared = false
		l.note("every sweep family this replica may take is led by another replica")
		return false
	}
	l.note("")
	return false
}

// mayTry reports whether a round tries f: free of this replica, not stalled, past its cooldown, its previous term
// joined, and either prompt or past the spreading wait (k families led, k retries since the last acquisition).
func (l *SweeperLeader) mayTry(f *SweeperFamily, k int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if f.locked || f.stalled || now.Before(f.cooldown) || !f.last.joined() {
		return false
	}
	return f.prompt || now.Sub(l.lastAcquire) >= time.Duration(k)*l.retry
}

// lockedCount is how many families the election session holds, stalled ones included.
func (l *SweeperLeader) lockedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, f := range l.fams {
		if f.locked {
			n++
		}
	}
	return n
}

// allJoined reports whether every family's latest term has joined.
func (l *SweeperLeader) allJoined() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.fams {
		if !f.last.joined() {
			return false
		}
	}
	return true
}

// stmtWait is the deadline of one statement on the election connection.
func (l *SweeperLeader) stmtWait() time.Duration { return max(2*l.monitor, electionStmtFloor) }

// query runs one single-value statement on the election connection under a deadline of its own, never a caller's
// context. connMu held.
func (l *SweeperLeader) query(dst any, sql string, args ...any) error {
	sctx, cancel := context.WithTimeout(context.Background(), l.stmtWait())
	defer cancel()
	return l.conn.QueryRow(sctx, sql, args...).Scan(dst)
}

// bump moves f's epoch, and the older replicas' sweeper_leader epoch, on in one transaction. connMu held.
func (l *SweeperLeader) bump(f *SweeperFamily) (int64, error) {
	sctx, cancel := context.WithTimeout(context.Background(), l.stmtWait())
	defer cancel()
	var epoch int64
	err := pgx.BeginFunc(sctx, l.conn, func(tx pgx.Tx) error {
		if err := tx.QueryRow(sctx, `UPDATE sweeper_families SET epoch = epoch + 1, holder = $1, acquired_at = now()
			WHERE family = $2 RETURNING epoch`, l.holder, f.name).Scan(&epoch); err != nil {
			return err
		}
		_, err := tx.Exec(sctx, `UPDATE sweeper_leader SET epoch = epoch + 1, holder = $1, acquired_at = now() WHERE id`, l.holder)
		return err
	})
	return epoch, err
}

// open starts a term of f at epoch. connMu held.
func (l *SweeperLeader) open(f *SweeperFamily, epoch int64) {
	tctx, cancel := context.WithCancel(l.runCtx)
	t := &sweeperTerm{ctx: tctx, cancel: cancel, epoch: epoch, done: make(chan struct{})}
	l.mu.Lock()
	f.term, f.last, f.locked, f.prompt, f.acquiredAt = t, t, true, false, time.Now()
	l.mu.Unlock()
	slog.Info("wardynd: sweeper leader: leads a sweep family", slog.String("family", f.name), slog.Int64("epoch", epoch))
}

// check reads every held family's epoch in one query. A failed query reports lost; a family whose epoch moved on
// has its term ended, joined and its lock released.
func (l *SweeperLeader) check() (lost bool) {
	l.connMu.Lock()
	if l.conn == nil || l.broken {
		// A connection Resign's unlock found broken still has live terms to end.
		lost := l.conn != nil && l.anyLive()
		l.connMu.Unlock()
		return lost
	}
	l.mu.Lock()
	var names []string
	terms := map[*SweeperFamily]*sweeperTerm{}
	for _, f := range l.fams {
		if f.locked {
			names = append(names, f.name)
			if f.term != nil {
				terms[f] = f.term
			}
		}
	}
	l.mu.Unlock()
	if len(names) == 0 {
		l.connMu.Unlock()
		return false
	}
	got := map[string]int64{}
	sctx, cancel := context.WithTimeout(context.Background(), l.stmtWait())
	rows, err := l.conn.Query(sctx, `SELECT family, epoch FROM sweeper_families WHERE family = ANY($1)`, names)
	if err == nil {
		var fam string
		var epoch int64
		_, err = pgx.ForEachRow(rows, []any{&fam, &epoch}, func() error { got[fam] = epoch; return nil })
	}
	cancel()
	if err != nil {
		l.broken = true
		l.connMu.Unlock()
		return true
	}
	l.connMu.Unlock()
	var moved []*SweeperFamily
	for f, t := range terms {
		if got[f.name] != t.epoch {
			slog.Warn("wardynd: sweeper leader: a family's epoch moved on; its sweeps are stopping",
				slog.String("family", f.name), slog.Int64("epoch", t.epoch))
			moved = append(moved, f)
		}
	}
	l.endFamilies(moved, false)
	return false
}

// anyLive reports whether any family has a live term.
func (l *SweeperLeader) anyLive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.fams {
		if f.term != nil {
			return true
		}
	}
	return false
}

// lose ends every term once the election connection failed. Its session's locks are gone or go when it closes, so a
// family held at the loss is tried at the next round with no spreading wait. The connection is closed only once
// every term on it has joined.
func (l *SweeperLeader) lose() {
	l.mu.Lock()
	var live []*SweeperFamily
	for _, f := range l.fams {
		if f.locked {
			f.prompt = true
		}
		if f.term != nil {
			live = append(live, f)
		}
	}
	l.mu.Unlock()
	if len(live) > 0 {
		slog.Warn("wardynd: sweeper leader: lost the election connection; its family sweeps are stopping",
			slog.Int("families", len(live)))
	}
	l.endFamilies(live, false)
	l.connMu.Lock()
	if l.conn != nil && l.allJoined() {
		l.closeConn()
	}
	l.connMu.Unlock()
}

// endFamilies ends the live term of each of fams, waits up to resignJoin for their units, and releases the lock of
// each family whose units all returned. A family with a unit still running keeps its lock and is stalled. resigned
// starts a released family's cooldown. Called without connMu or mu.
func (l *SweeperLeader) endFamilies(fams []*SweeperFamily, resigned bool) {
	type ended struct {
		f *SweeperFamily
		t *sweeperTerm
	}
	var all []ended
	l.mu.Lock()
	for _, f := range fams {
		if t := f.end(); t != nil {
			all = append(all, ended{f, t})
		}
	}
	l.mu.Unlock()
	if len(all) == 0 {
		return
	}
	deadline := time.Now().Add(l.resignJoin)
	var stuck []ended
	l.connMu.Lock()
	for _, e := range all {
		// connMu is released for the wait, so a round or a check is held up by at most one statement.
		l.connMu.Unlock()
		joined := e.t.join(deadline)
		l.connMu.Lock()
		if joined {
			l.unlockFamily(e.f, resigned)
		} else {
			stuck = append(stuck, e)
		}
	}
	l.connMu.Unlock()
	l.mu.Lock()
	for _, e := range stuck {
		e.f.stall(e.t, l.resignJoin)
	}
	l.mu.Unlock()
}

// unlockFamily releases f's lock on the election connection once its units have returned. On a broken connection
// the lock goes when the connection closes. connMu held.
func (l *SweeperLeader) unlockFamily(f *SweeperFamily, cooldown bool) {
	l.mu.Lock()
	locked := f.locked
	l.mu.Unlock()
	if !locked || l.conn == nil || l.broken {
		return
	}
	var freed bool
	if err := l.query(&freed, `SELECT pg_advisory_unlock($1, $2)`, SweeperFamilyLockClass, f.obj); err != nil {
		l.broken = true
		return
	}
	if !freed {
		slog.Warn("wardynd: sweeper leader: the election session no longer held a family's lock", slog.String("family", f.name))
	}
	l.mu.Lock()
	f.locked, f.stalled = false, false
	if cooldown {
		f.cooldown = time.Now().Add(l.cooldown)
	}
	l.mu.Unlock()
}

// unlockJoinedStalls releases each stalled family whose units have now returned. connMu held.
func (l *SweeperLeader) unlockJoinedStalls() {
	for _, f := range l.fams {
		l.mu.Lock()
		ready := f.stalled && f.last.joined()
		l.mu.Unlock()
		if ready {
			slog.Info("wardynd: sweeper leader: a stalled family's sweep returned; releasing its lock", slog.String("family", f.name))
			l.unlockFamily(f, true)
		}
	}
}

// closeConn closes the election connection, which drops every lock its session holds. Only once every term has
// joined. connMu held.
func (l *SweeperLeader) closeConn() {
	bg, cancel := context.WithTimeout(context.Background(), dedicatedLockReleaseWait)
	defer cancel()
	l.conn.Exec(bg, `SELECT pg_advisory_unlock_all()`) //nolint:errcheck // best-effort; closing the session drops them anyway
	l.conn.Close(bg)                                   //nolint:errcheck // best-effort
	l.conn, l.broken, l.shared = nil, false, false
	l.mu.Lock()
	for _, f := range l.fams {
		f.locked, f.stalled = false, false
	}
	l.mu.Unlock()
}

// shutdown ends every term when Run's context ends, bounded, and closes the connection once every term has joined:
// at once when they have, else when the last unit still running returns.
func (l *SweeperLeader) shutdown() {
	l.mu.Lock()
	var live []*SweeperFamily
	for _, f := range l.fams {
		if f.term != nil {
			live = append(live, f)
		}
	}
	l.mu.Unlock()
	l.endFamilies(live, false)
	l.connMu.Lock()
	defer l.connMu.Unlock()
	if l.conn == nil {
		return
	}
	if l.allJoined() {
		l.closeConn()
		return
	}
	l.mu.Lock()
	var lasts []*sweeperTerm
	for _, f := range l.fams {
		if f.last != nil {
			lasts = append(lasts, f.last)
		}
	}
	l.mu.Unlock()
	go func() {
		for _, t := range lasts {
			<-t.done
		}
		l.connMu.Lock()
		defer l.connMu.Unlock()
		if l.conn != nil {
			l.closeConn()
		}
	}()
}

// logStalls logs each stalled family again once familyStallLogEvery has passed since its last log.
func (l *SweeperLeader) logStalls() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for _, f := range l.fams {
		if f.stalled && now.Sub(f.stallLog) >= familyStallLogEvery {
			f.stallLog = now
			epoch := int64(0)
			if f.last != nil {
				epoch = f.last.epoch
			}
			slog.Error("wardynd: sweeper family stalled; its sweep did not stop, so its lock is kept",
				slog.String("family", f.name), slog.Int64("epoch", epoch), slog.Duration("bound", l.resignJoin))
		}
	}
}

// note logs why this replica leads no family, once per change of reason; "" clears it. connMu held.
func (l *SweeperLeader) note(reason string) {
	if reason == l.noted {
		return
	}
	l.noted = reason
	if reason != "" {
		slog.Info("wardynd: sweeper leader: leads no family", slog.String("reason", reason))
	}
}

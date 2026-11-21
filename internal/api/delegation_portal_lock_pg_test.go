// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: a change to a portal that starts while an exchange is inside one of its locked transactions.
// A session on a pool of its own holds a table in SHARE mode, so the exchange's next insert into it waits
// after its lock on the binding (FOR KEY SHARE) and on the portal row (FOR SHARE) is taken: portal_grants
// parks the grant write, principal_identities parks a first-ever person's identity write. The change runs on
// another pool and must wait for the exchange; once the hold is released the exchange's transaction commits
// first, and the outcome is the plan's for that order. The other order, the change committing first, is
// TestPortalExchange_AdmissionOrRevokeRacesWrite_PG.

// lockWait bounds every wait in these tests.
const lockWait = 15 * time.Second

// lockRig owns what one lock test opens beside the harness: side pools, holding transactions and goroutines.
// Its cleanup is registered before any of them, so it runs before the harness's own. It releases every hold
// first, then joins the goroutines, cancelling their statements if they have not ended within lockWait,
// and closes the pools only once every goroutine has ended. A failure at any step therefore never leaves a
// pool's Close waiting on a connection whose statement waits on a hold that is still queued to release.
type lockRig struct {
	t      *testing.T
	dsn    string
	ctx    context.Context // bounds every statement the rig's goroutines and observations run
	cancel context.CancelFunc
	pools  []*pgxpool.Pool
	holds  []*lockHold
	wg     sync.WaitGroup
	joined bool
}

// lockHold is a transaction on a pool of its own that holds one table in SHARE mode.
type lockHold struct {
	tx       pgx.Tx
	pid      int32
	released bool
}

func (e *exchPG) newLockRig(t *testing.T) *lockRig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*lockWait)
	r := &lockRig{t: t, dsn: e.pool.Config().ConnString(), ctx: ctx, cancel: cancel}
	t.Cleanup(r.close)
	return r
}

func (r *lockRig) close() {
	for i := len(r.holds) - 1; i >= 0; i-- {
		r.holds[i].release()
	}
	joined := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(lockWait):
		r.cancel()
		select {
		case <-joined:
		case <-time.After(lockWait):
			// The pools stay open: closing them would wait on that goroutine's connection.
			r.t.Errorf("a goroutine of the lock test did not end within %v of its statements' cancel", lockWait)
			return
		}
	}
	r.cancel()
	r.joined = true
	for _, p := range r.pools {
		p.Close()
	}
}

func (h *lockHold) release() {
	if h.released {
		return
	}
	h.released = true
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	_ = h.tx.Rollback(ctx)
}

// pool is another pool on the harness's database, closed by the rig's cleanup.
func (r *lockRig) pool() *pgxpool.Pool {
	r.t.Helper()
	p, err := db.Connect(r.ctx, r.dsn)
	if err != nil {
		r.t.Fatal(err)
	}
	r.pools = append(r.pools, p)
	return p
}

// hold takes table in SHARE mode in a transaction on a pool of its own, so the next insert into it waits.
func (r *lockRig) hold(table string) *lockHold {
	r.t.Helper()
	tx, err := r.pool().BeginTx(r.ctx, pgx.TxOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	h := &lockHold{tx: tx}
	r.holds = append(r.holds, h)
	if err := tx.QueryRow(r.ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
		r.t.Fatal(err)
	}
	if _, err := tx.Exec(r.ctx, `LOCK TABLE `+table+` IN SHARE MODE`); err != nil {
		r.t.Fatal(err)
	}
	return h
}

// countOn counts through h's transaction, so the test goroutine never takes a second connection of one pool.
func (r *lockRig) countOn(h *lockHold, q string, args ...any) int {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, lockWait)
	defer cancel()
	var n int
	if err := h.tx.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// waiterOn reads through h until a backend waits on pid, and returns the first such backend. It gives up,
// with false, when done closes or bound passes first; every read has a deadline of its own. It reads
// pg_locks, which is live on every read, not pg_stat_activity, whose snapshot a transaction keeps.
func (r *lockRig) waiterOn(h *lockHold, pid int32, done <-chan struct{}, bound time.Duration) (int32, bool) {
	r.t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(r.ctx, time.Second)
		var waiter int32
		err := h.tx.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE NOT granted AND pid IS NOT NULL AND $1 = ANY(pg_blocking_pids(pid)) ORDER BY pid LIMIT 1`, pid).Scan(&waiter)
		cancel()
		if err == nil {
			return waiter, true
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			r.t.Fatalf("read the lock waiters: %v", err)
		}
		select {
		case <-done:
			return 0, false
		case <-time.After(20 * time.Millisecond):
		}
	}
	return 0, false
}

// runOn runs f on a goroutine the rig joins, under the rig's context, and returns its result and a channel
// closed when it ends.
func runOn[T any](r *lockRig, f func(ctx context.Context) T) (<-chan T, <-chan struct{}) {
	out := make(chan T, 1)
	done := make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(done)
		out <- f(r.ctx)
	}()
	return out, done
}

// parkExchange starts an exchange through portal d and returns once its backend waits on h, with that
// backend, the exchange's result and a channel closed when it ends.
func (r *lockRig) parkExchange(e *exchPG, h *lockHold, d types.Delegate, cred string) (int32, <-chan *httptest.ResponseRecorder, <-chan struct{}) {
	r.t.Helper()
	out, done := runOn(r, func(ctx context.Context) *httptest.ResponseRecorder {
		form := url.Values{"grant_type": {grantTypeTokenExchange}, "subject_token": {"subject-token"}, "subject_token_type": {tokenTypeAccessToken}}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(d.ID.String(), cred)
		w := httptest.NewRecorder()
		e.h.srv.Handler().ServeHTTP(w, req)
		return w
	})
	pid, ok := r.waiterOn(h, h.pid, done, lockWait)
	if !ok {
		select {
		case w := <-out:
			r.t.Fatalf("the exchange never waited on the hold: it answered %d %s", w.Code, w.Body)
		default:
			r.t.Fatalf("the exchange never waited on the hold within %v", lockWait)
		}
	}
	return pid, out, done
}

// changeWaits starts change on a goroutine and fails unless it waits on the exchange's backend.
func (r *lockRig) changeWaits(h *lockHold, exchPID int32, change func(ctx context.Context) error) <-chan error {
	r.t.Helper()
	res, done := runOn(r, change)
	if _, ok := r.waiterOn(h, exchPID, done, lockWait); !ok {
		select {
		case err := <-res:
			r.t.Fatalf("the change did not wait for the exchange inside its locked transaction (it returned %v)", err)
		default:
			r.t.Fatalf("no wait of the change on the exchange was seen within %v", lockWait)
		}
	}
	return res
}

func withinPortal[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(lockWait):
		t.Fatalf("%s did not finish within %v", what, lockWait)
		var zero T
		return zero
	}
}

// portalRetire retires the portal's issuer binding as an operator's retirement does, under the registry lock.
func portalRetire(ctx context.Context, side *pgxpool.Pool, key string) error {
	tx, err := side.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
		return err
	}
	if err := store.NewPG(side).RetirePortalIssuerQ(ctx, tx, key, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestPortalExchange_ChangeWaitsForLockedGrantWrite_PG(t *testing.T) {
	t.Run("revoke waits on the portal row and ends the grant", func(t *testing.T) {
		e := newExchPG(t)
		d, cred := e.portal(t, portalTestIssuer, nil)
		r := e.newLockRig(t)
		h := r.hold("portal_grants")
		pid, result, _ := r.parkExchange(e, h, d, cred)
		side := r.pool()
		changed := r.changeWaits(h, pid, func(ctx context.Context) error {
			_, _, err := store.NewPG(side).RevokeDelegate(ctx, d.ID, time.Now(), false)
			return err
		})
		h.release()
		w := withinPortal(t, result, "the exchange")
		tok, _ := portalToken(t, w)
		if err := withinPortal(t, changed, "the revoke"); err != nil {
			t.Fatalf("revoke after the exchange: %v", err)
		}
		if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("GET /me with a grant of the portal revoked after it = %d, want 401", w.Code)
		}
	})
	t.Run("narrowing waits on the portal row and the grant runs to its end", func(t *testing.T) {
		e := newExchPG(t)
		d, cred := e.portal(t, portalTestIssuer, nil)
		r := e.newLockRig(t)
		h := r.hold("portal_grants")
		pid, result, _ := r.parkExchange(e, h, d, cred)
		side := r.pool()
		changed := r.changeWaits(h, pid, func(ctx context.Context) error {
			return store.NewPG(side).UpdateDelegateAdmissionQ(ctx, side, d.ID,
				types.PortalAdmission{PortalManaged: true}, types.PortalAdmission{Group: "portal-users"})
		})
		h.release()
		tok, _ := portalToken(t, withinPortal(t, result, "the exchange"))
		if err := withinPortal(t, changed, "the narrowing"); err != nil {
			t.Fatalf("narrow after the exchange: %v", err)
		}
		if n := e.count(t, `SELECT count(*) FROM delegates WHERE id = $1 AND admission_rev = $2`, d.ID, d.AdmissionRev+1); n != 1 {
			t.Fatalf("the narrowing did not land after the exchange")
		}
		if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
			t.Fatalf("GET /me with the grant minted before the narrowing = %d, want 200 to its own end", w.Code)
		}
	})
	t.Run("retirement waits on the binding and is refused", func(t *testing.T) {
		e := newExchPG(t)
		d, cred := e.portal(t, portalTestIssuer, nil)
		r := e.newLockRig(t)
		h := r.hold("portal_grants")
		pid, result, _ := r.parkExchange(e, h, d, cred)
		side := r.pool()
		changed := r.changeWaits(h, pid, func(ctx context.Context) error { return portalRetire(ctx, side, d.IssuerKey) })
		h.release()
		portalToken(t, withinPortal(t, result, "the exchange"))
		if err := withinPortal(t, changed, "the retirement"); !errors.Is(err, store.ErrIssuerInUse) {
			t.Fatalf("retire under a committed exchange: %v, want ErrIssuerInUse", err)
		}
		if n := e.count(t, `SELECT count(*) FROM portal_issuers WHERE issuer_key = $1`, d.IssuerKey); n != 1 {
			t.Fatalf("binding rows after a refused retirement = %d, want 1", n)
		}
	})
}

// TestPortalExchange_ChangeWaitsForLockedIdentityWrite_PG parks a first-ever person's exchange after its
// locked portal read and before its identity insert. A second hold, on principal_groups, then parks the
// exchange between its identity commit and its grant transaction, so the change commits in that gap and
// what exists there is read before the grant transaction runs.
func TestPortalExchange_ChangeWaitsForLockedIdentityWrite_PG(t *testing.T) {
	cases := []struct {
		name   string
		change func(ctx context.Context, side *pgxpool.Pool, d types.Delegate) error
		landed string // counts 1 once the change has committed; empty for the refused retirement
		status int
		code   string
		grants int
	}{
		{name: "revoke waits on the portal row; the identity stays and grants nothing",
			change: func(ctx context.Context, side *pgxpool.Pool, d types.Delegate) error {
				_, _, err := store.NewPG(side).RevokeDelegate(ctx, d.ID, time.Now(), false)
				return err
			},
			landed: `SELECT count(*) FROM delegates WHERE id = $1 AND revoked_at IS NOT NULL AND admission_rev >= $2`,
			status: http.StatusUnauthorized, code: "invalid_client", grants: 0},
		{name: "narrowing waits on the portal row; the grant transaction sees the new revision",
			change: func(ctx context.Context, side *pgxpool.Pool, d types.Delegate) error {
				return store.NewPG(side).UpdateDelegateAdmissionQ(ctx, side, d.ID,
					types.PortalAdmission{PortalManaged: true}, types.PortalAdmission{Group: "portal-users"})
			},
			landed: `SELECT count(*) FROM delegates WHERE id = $1 AND admission_rev > $2`,
			status: http.StatusServiceUnavailable, code: "temporarily_unavailable", grants: 0},
		{name: "retirement waits on the binding and is refused once the identity exists",
			change: func(ctx context.Context, side *pgxpool.Pool, d types.Delegate) error {
				return portalRetire(ctx, side, d.IssuerKey)
			},
			status: http.StatusOK, code: "access_token", grants: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newExchPG(t)
			d, cred := e.portal(t, portalTestIssuer, nil)
			r := e.newLockRig(t)
			idHold := r.hold("principal_identities")
			groupsHold := r.hold("principal_groups")
			pid, result, exchDone := r.parkExchange(e, idHold, d, cred)
			if n := r.countOn(groupsHold, `SELECT count(*) FROM principal_identities WHERE introduced_by = $1`, d.ID); n != 0 {
				t.Fatalf("identity rows while the first identity write waits = %d, want 0", n)
			}
			side := r.pool()
			changed := r.changeWaits(idHold, pid, func(ctx context.Context) error { return tc.change(ctx, side, d) })
			idHold.release()
			err := withinPortal(t, changed, "the change")
			if tc.landed == "" {
				if !errors.Is(err, store.ErrIssuerInUse) {
					t.Fatalf("retire after the identity committed: %v, want ErrIssuerInUse", err)
				}
			} else if err != nil {
				t.Fatalf("change after the identity committed: %v", err)
			}
			if _, ok := r.waiterOn(groupsHold, groupsHold.pid, exchDone, lockWait); !ok {
				t.Fatal("the exchange did not reach its sign-in record write after the identity committed")
			}
			// Between the two transactions: the identity is there, no grant is, and the change has landed.
			if n := r.countOn(groupsHold, `SELECT count(*) FROM principal_identities WHERE introduced_by = $1`, d.ID); n != 1 {
				t.Fatalf("identity rows after the identity commit = %d, want 1", n)
			}
			if n := r.countOn(groupsHold, `SELECT count(*) FROM portal_grants WHERE delegate_id = $1`, d.ID); n != 0 {
				t.Fatalf("grant rows before the grant transaction = %d, want 0", n)
			}
			if tc.landed != "" {
				if n := r.countOn(groupsHold, tc.landed, d.ID, d.AdmissionRev); n != 1 {
					t.Fatal("the change did not commit while the exchange waited between its transactions")
				}
			}
			groupsHold.release()
			w := withinPortal(t, result, "the exchange")
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("exchange after the change = %d %s, want %d %s", w.Code, w.Body, tc.status, tc.code)
			}
			if n := e.count(t, `SELECT count(*) FROM principal_identities WHERE introduced_by = $1`, d.ID); n != 1 {
				t.Fatalf("identity rows at the end = %d, want 1", n)
			}
			if n := e.count(t, `SELECT count(*) FROM portal_grants WHERE delegate_id = $1`, d.ID); n != tc.grants {
				t.Fatalf("grant rows at the end = %d, want %d", n, tc.grants)
			}
			if n := e.count(t, `SELECT count(*) FROM portal_issuers WHERE issuer_key = $1`, d.IssuerKey); n != 1 {
				t.Fatalf("binding rows at the end = %d, want 1", n)
			}
		})
	}
}

// TestPortalExchange_FailedLockObservationCleansUp_PG fails an observation while a change still waits on a
// parked exchange, then leaves the subtest. Its cleanup must release the hold before it closes a pool: the
// exchange then commits, the change after it, both goroutines end and every side pool closes, all within
// the tests' bounds.
func TestPortalExchange_FailedLockObservationCleansUp_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, nil)
	var rig *lockRig
	var result <-chan *httptest.ResponseRecorder
	var changed <-chan error
	start := time.Now()
	t.Run("observation fails while the change waits", func(t *testing.T) {
		rig = e.newLockRig(t)
		h := rig.hold("portal_grants")
		pid, out, _ := rig.parkExchange(e, h, d, cred)
		result = out
		side := rig.pool()
		res, done := runOn(rig, func(ctx context.Context) error {
			_, _, err := store.NewPG(side).RevokeDelegate(ctx, d.ID, time.Now(), false)
			return err
		})
		changed = res
		if _, ok := rig.waiterOn(h, pid, done, lockWait); !ok {
			t.Fatal("the revoke did not wait on the parked exchange")
		}
		// No backend has pid 0, so this observation runs out its bound while the revoke still waits.
		if _, ok := rig.waiterOn(h, 0, done, 200*time.Millisecond); ok {
			t.Fatal("an observation of pid 0 saw a waiter")
		}
		select {
		case err := <-res:
			t.Fatalf("the revoke ended while the exchange held the portal row: %v", err)
		default:
		}
	})
	if rig == nil || !rig.joined {
		t.Fatal("the rig's cleanup did not join its goroutines")
	}
	if took := time.Since(start); took > 3*lockWait {
		t.Fatalf("the subtest and its cleanup took %v, want under %v", took, 3*lockWait)
	}
	select {
	case w := <-result:
		portalToken(t, w)
	default:
		t.Fatal("the exchange had not answered when the cleanup returned")
	}
	select {
	case err := <-changed:
		if err != nil {
			t.Fatalf("the revoke after the release: %v", err)
		}
	default:
		t.Fatal("the revoke had not returned when the cleanup returned")
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	for i, p := range rig.pools {
		if err := p.Ping(ctx); err == nil {
			t.Fatalf("side pool %d still open after the cleanup", i)
		}
	}
}

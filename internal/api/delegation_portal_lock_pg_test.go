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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: a change to a portal that starts while an exchange is inside its locked grant transaction. A
// session on a pool of its own holds portal_grants in SHARE mode, so the exchange's grant INSERT waits after
// its lock on the binding (FOR KEY SHARE) and on the portal row (FOR SHARE) is taken. The change runs on a
// third pool and must wait for the exchange; once the hold is released the exchange commits first, and the
// outcome is the plan's for that order. The other order, the change committing first, is
// TestPortalExchange_AdmissionOrRevokeRacesWrite_PG.

// lockWait bounds every wait in these tests.
const lockWait = 15 * time.Second

// sidePool is another pool on the harness's database, for a transaction held apart from the server's.
func (e *exchPG) sidePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), e.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// exchangeLockWaiter waits, reading through q, until a backend is waiting on pid, and returns the first such backend.
// It stops early when done closes. It reads pg_locks, which is live on every read, not pg_stat_activity,
// whose snapshot a transaction keeps.
func exchangeLockWaiter(t *testing.T, q pgx.Tx, pid int32, done <-chan struct{}) (int32, bool) {
	t.Helper()
	deadline := time.Now().Add(lockWait)
	for time.Now().Before(deadline) {
		var waiter int32
		err := q.QueryRow(context.Background(), `SELECT pid FROM pg_locks WHERE NOT granted AND pid IS NOT NULL AND $1 = ANY(pg_blocking_pids(pid)) ORDER BY pid LIMIT 1`, pid).Scan(&waiter)
		if err == nil {
			return waiter, true
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-done:
			return 0, false
		case <-time.After(20 * time.Millisecond):
		}
	}
	return 0, false
}

// parkedExchange starts an exchange through portal d and returns once it waits inside its grant transaction,
// with the exchange's backend, its result channel, the holding transaction (every later wait is read through
// it, so the test goroutine never takes a second connection of one pool) and the release of the hold.
func (e *exchPG) parkedExchange(t *testing.T, d types.Delegate, cred string) (exchPID int32, result <-chan *httptest.ResponseRecorder, hold pgx.Tx, release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.sidePool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	var holdPID int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holdPID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE portal_grants IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	out := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		form := url.Values{"grant_type": {grantTypeTokenExchange}, "subject_token": {"subject-token"}, "subject_token_type": {tokenTypeAccessToken}}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth(d.ID.String(), cred)
		w := httptest.NewRecorder()
		e.h.srv.Handler().ServeHTTP(w, r)
		out <- w
	}()
	exchPID, ok := exchangeLockWaiter(t, tx, holdPID, done)
	if !ok {
		release()
		t.Fatalf("the exchange never waited inside its grant transaction (it answered %d)", withinPortal(t, out, "the exchange").Code)
	}
	return exchPID, out, tx, release
}

// waitsOnExchange starts change on its own goroutine and fails unless it waits on the parked exchange.
func waitsOnExchange(t *testing.T, hold pgx.Tx, exchPID int32, change func() error) <-chan error {
	t.Helper()
	res := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res <- change()
	}()
	if _, ok := exchangeLockWaiter(t, hold, exchPID, done); !ok {
		t.Fatalf("the change did not wait for the exchange inside its locked transaction (it returned %v)", withinPortal(t, res, "the change"))
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

func TestPortalExchange_ChangeWaitsForLockedGrantWrite_PG(t *testing.T) {
	ctx := context.Background()
	t.Run("revoke waits on the portal row and ends the grant", func(t *testing.T) {
		e := newExchPG(t)
		d, cred := e.portal(t, portalTestIssuer, nil)
		pid, result, hold, release := e.parkedExchange(t, d, cred)
		side := e.sidePool(t)
		changed := waitsOnExchange(t, hold, pid, func() error {
			_, _, err := store.NewPG(side).RevokeDelegate(ctx, d.ID, time.Now(), false)
			return err
		})
		release()
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
		pid, result, hold, release := e.parkedExchange(t, d, cred)
		side := e.sidePool(t)
		changed := waitsOnExchange(t, hold, pid, func() error {
			return store.NewPG(side).UpdateDelegateAdmissionQ(ctx, side, d.ID,
				types.PortalAdmission{PortalManaged: true}, types.PortalAdmission{Group: "portal-users"})
		})
		release()
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
		pid, result, hold, release := e.parkedExchange(t, d, cred)
		side := e.sidePool(t)
		changed := waitsOnExchange(t, hold, pid, func() error {
			tx, err := side.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
				return err
			}
			if err := store.NewPG(side).RetirePortalIssuerQ(ctx, tx, d.IssuerKey, nil); err != nil {
				return err
			}
			return tx.Commit(ctx)
		})
		release()
		portalToken(t, withinPortal(t, result, "the exchange"))
		if err := withinPortal(t, changed, "the retirement"); !errors.Is(err, store.ErrIssuerInUse) {
			t.Fatalf("retire under a committed exchange: %v, want ErrIssuerInUse", err)
		}
		if n := e.count(t, `SELECT count(*) FROM portal_issuers WHERE issuer_key = $1`, d.IssuerKey); n != 1 {
			t.Fatalf("binding rows after a refused retirement = %d, want 1", n)
		}
	})
}

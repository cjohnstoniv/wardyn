// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A Put under WithExpiry records expires_at; a Put without it clears it, so a
// replace always describes the value it wrote.
func TestPG_PutRecordsAndClearsExpiry(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	owner, name := "exp-"+uuid.NewString(), "sign-in"
	t.Cleanup(func() { _ = s.For(owner).Delete(ctx, name) })
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	expiry := func() *time.Time {
		var got *time.Time
		if err := pool.QueryRow(ctx, `SELECT expires_at FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if err := s.For(owner).Put(secretstore.WithExpiry(ctx, at), name, []byte("blob-v1")); err != nil {
		t.Fatal(err)
	}
	if got := expiry(); got == nil || !got.Equal(at) {
		t.Fatalf("expires_at = %v, want %v", got, at)
	}
	if err := s.For(owner).Put(ctx, name, []byte("blob-v2")); err != nil {
		t.Fatal(err)
	}
	if got := expiry(); got != nil {
		t.Fatalf("expires_at after a Put without an expiry = %v, want NULL", got)
	}
}

// DeleteExpired deletes a row whose expiry has passed and reports it, and
// keeps rows with no expiry, with one still ahead, and one renewed past it.
func TestPG_DeleteExpiredDeletesOnlyLapsedRows(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	owner := "exp-" + uuid.NewString()
	st := s.For(owner)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	seed := []struct {
		name string
		ctx  context.Context
	}{
		{"lapsed", secretstore.WithExpiry(ctx, past)},
		{"renewed", secretstore.WithExpiry(ctx, past)},
		{"ahead", secretstore.WithExpiry(ctx, future)},
		{"no-expiry", ctx},
	}
	for _, w := range seed {
		if err := st.Put(w.ctx, w.name, []byte("value-"+w.name)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Delete(ctx, w.name) })
	}
	// A renewal replaces the expiry it was stored with.
	if err := st.Put(secretstore.WithExpiry(ctx, future), "renewed", []byte("value-renewed-2")); err != nil {
		t.Fatal(err)
	}

	gone, err := s.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	var mine []string
	for _, e := range gone {
		if e.Owner == owner {
			mine = append(mine, e.Name)
			if e.ExpiresAt.IsZero() {
				t.Errorf("expired row %q reported without its expiry", e.Name)
			}
		}
	}
	if len(mine) != 1 || mine[0] != "lapsed" {
		t.Fatalf("DeleteExpired removed %v of this owner's rows, want [lapsed]", mine)
	}
	if _, err := st.Get(ctx, "lapsed"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get(lapsed) after the sweep = %v, want ErrNotFound", err)
	}
	for _, keep := range []string{"renewed", "ahead", "no-expiry"} {
		if _, err := st.Get(ctx, keep); err != nil {
			t.Fatalf("Get(%s) after the sweep = %v, want it kept", keep, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE owned_by=$1`, owner).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows left = %d (%v), want 3", n, err)
	}
}

// holdRowLock takes (owner, name)'s row lock in its own transaction, as a
// concurrent writer would, and returns the release.
func holdRowLock(t *testing.T, pool *pgxpool.Pool, owner, name string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockRow(ctx, tx, owner, name); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback(ctx) }) }
	t.Cleanup(release)
	return release
}

// waitForRowLockWaiter returns once another session is queued on (owner,
// name)'s row lock: the sweep has scanned the row and is about to re-check it.
func waitForRowLockWaiter(t *testing.T, pool *pgxpool.Pool, owner, name string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted AND classid::bigint = $1 AND objid::bigint = $2`,
			int64(uint32(db.SecretRowLockClass)), int64(uint32(rowLockKey(owner, name)))).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing queued on %s's row lock: the sweep never reached the row", rowRef(owner, name))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// keptRows is the (owner, name) pairs a sweep's error names as kept.
func keptRows(err error) map[string]bool {
	out := map[string]bool{}
	var list []error
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		list = j.Unwrap()
	}
	for _, e := range list {
		var k *secretstore.ExpiredKept
		if errors.As(e, &k) {
			out[k.Owner+"/"+k.Name] = true
		}
	}
	return out
}

// The sweep re-checks a row's expiry under its lock. The schedule is forced:
// the sweep scans the lapsed row and queues on its lock, which this test
// holds; the row is renewed; only then is the lock let go. The renewed row
// must be kept.
func TestPG_DeleteExpiredRechecksRenewalAfterScan(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	owner, name := "exp-"+uuid.NewString(), "sign-in"
	st := s.For(owner)
	if err := st.Put(secretstore.WithExpiry(ctx, time.Now().Add(-time.Hour)), name, []byte("value-lapsed")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Delete(ctx, name) })

	release := holdRowLock(t, pool, owner, name)
	type result struct {
		gone []secretstore.Expired
		err  error
	}
	done := make(chan result, 1)
	go func() {
		gone, err := s.DeleteExpired(ctx)
		done <- result{gone, err}
	}()
	waitForRowLockWaiter(t, pool, owner, name)
	if err := st.Put(secretstore.WithExpiry(ctx, time.Now().Add(time.Hour)), name, []byte("value-renewed")); err != nil {
		t.Fatal(err)
	}
	release()

	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the sweep did not finish once the lock was let go")
	}
	for _, e := range r.gone {
		if e.Owner == owner {
			t.Fatalf("the sweep deleted %s, renewed after its scan", rowRef(owner, name))
		}
	}
	if keptRows(r.err)[owner+"/"+name] {
		t.Fatalf("the sweep reported the renewed row as a failure: %v", r.err)
	}
	got, err := st.Get(ctx, name)
	if err != nil || string(got) != "value-renewed" {
		t.Fatalf("Get after the sweep = %q, %v; want the renewed value", got, err)
	}
}

// Each scan reads at most one page, starting after the cursor, and gives up
// within the store's bound when the table cannot be read.
func TestPG_DeleteExpiredScanIsBounded(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	owner := "exp-" + uuid.NewString()
	st := s.For(owner)
	names := []string{"n0", "n1", "n2", "n3", "n4"}
	for _, n := range names {
		if err := st.Put(secretstore.WithExpiry(ctx, time.Now().Add(-time.Hour)), n, []byte("value-"+n)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Delete(ctx, n) })
	}

	t.Run("a page", func(t *testing.T) {
		for _, c := range []struct {
			after string
			want  []string
		}{{"", names[:2]}, {"n1", names[2:4]}} {
			due, err := s.scanExpired(ctx, owner, c.after, 2)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range due {
				got = append(got, d.Owner+"/"+d.Name)
			}
			var want []string
			for _, n := range c.want {
				want = append(want, owner+"/"+n)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("scan after %q = %v, want %v", c.after, got, want)
			}
		}
	})

	t.Run("a table it cannot read", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `LOCK TABLE secrets IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		short := *s
		short.extTimeout = 50 * time.Millisecond
		done := make(chan error, 1)
		go func() {
			_, err := short.scanExpired(ctx, owner, "", 2)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a scan of a locked table succeeded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a scan of a locked table was still waiting after 10s: it has no bound of its own")
		}
	})
}

// A row the store keeps refusing is named in the error and kept, and the
// sweep still reaches the rows after it, past the first page.
func TestPG_DeleteExpiredFailureDoesNotStarveLaterRows(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()
	prev := expiredScanBatch
	expiredScanBatch = 2
	t.Cleanup(func() { expiredScanBatch = prev })
	owner := "exp-" + uuid.NewString()
	st := s.For(owner)
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := st.Put(secretstore.WithExpiry(ctx, time.Now().Add(-time.Hour)), n, []byte("value-"+n)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Delete(ctx, n) })
	}
	// a and b fill the first page and cannot be locked within the bound.
	holdRowLock(t, pool, owner, "a")
	holdRowLock(t, pool, owner, "b")
	short := *s
	short.extTimeout = 50 * time.Millisecond

	_, err := short.DeleteExpired(ctx)
	kept := keptRows(err)
	for _, n := range []string{"a", "b"} {
		if !kept[owner+"/"+n] {
			t.Errorf("the sweep's error does not name %s as kept: %v", rowRef(owner, n), err)
		}
		if _, gerr := st.Get(ctx, n); gerr != nil {
			t.Errorf("Get(%s) = %v, want it kept", n, gerr)
		}
	}
	for _, n := range []string{"c", "d"} {
		if _, gerr := st.Get(ctx, n); !errors.Is(gerr, secretstore.ErrNotFound) {
			t.Errorf("Get(%s) = %v, want it swept: the refused rows starved it", n, gerr)
		}
	}
}

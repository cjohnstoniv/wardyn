// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The cleanup of a replaced store-mode object runs under the row's lock.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// rotExt is an external store that rotates object names like Azure Key Vault:
// a Put with no previous object writes "A", one that already has "A" moves to
// "B", and any other moves back to "A". Its first Delete of "A" after pause()
// stops until resume() — the cleanup of a replaced object in flight. A Delete
// on an ended context fails, as a real store's call does.
type rotExt struct {
	mu      sync.Mutex
	vals    map[string][]byte
	gated   bool
	entered chan struct{}
	release chan struct{}
}

func newRotExt() *rotExt { return &rotExt{vals: map[string][]byte{}} }

func (r *rotExt) pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gated, r.entered, r.release = true, make(chan struct{}), make(chan struct{})
}

func (r *rotExt) Name() string     { return "vaultkv" }
func (r *rotExt) Describe() string { return "vaultkv at test" }
func (r *rotExt) Put(_ context.Context, _, _, prev string, v []byte, _ bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	obj := "A"
	if secretstore.RefObject(prev) == "A" {
		obj = "B"
	}
	r.vals[obj] = append([]byte(nil), v...)
	return obj, nil
}
func (r *rotExt) Get(_ context.Context, _, _, ref string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vals[secretstore.RefObject(ref)]
	if !ok {
		return nil, fmt.Errorf("refused: nothing at %s", ref)
	}
	return append([]byte(nil), v...), nil
}
func (r *rotExt) Ref(_, _, ref string) (string, error) { return ref, nil }
func (r *rotExt) Check(ctx context.Context, owner, name, ref string) error {
	_, err := r.Get(ctx, owner, name, ref)
	return err
}
func (r *rotExt) Delete(ctx context.Context, _, _, ref string) error {
	r.mu.Lock()
	obj := secretstore.RefObject(ref)
	gate := r.gated && obj == "A"
	entered, release := r.entered, r.release
	if gate {
		r.gated = false
	}
	r.mu.Unlock()
	if gate {
		close(entered)
		<-release
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.vals, obj)
	return nil
}
func (r *rotExt) Walk(context.Context) ([]secretstore.ExternalEntry, error) { return nil, nil }

// A Put's cleanup of the object it replaced waits for the row's lock, so a
// delete and re-add that reuse that object while the cleanup is in flight land
// after it, and the row keeps its value.
func TestPG_PutExternal_ReplacedObjectCleanupHoldsTheRowLock(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRotExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	mustPut(t, s, "bob", "tok", "v1") // object A
	ext.pause()
	replaced := make(chan error, 1)
	go func() { replaced <- s.For("bob").Put(t.Context(), "tok", []byte("v2")) }() // object B; A's cleanup stops
	select {
	case <-ext.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the cleanup of the replaced object never started")
	}
	readded := make(chan error, 1)
	go func() {
		if err := s.For("bob").Delete(t.Context(), "tok"); err != nil {
			readded <- err
			return
		}
		readded <- s.For("bob").Put(t.Context(), "tok", []byte("v3")) // no previous object: A again
	}()
	select {
	case err := <-readded:
		t.Fatalf("the delete and re-add finished while the cleanup still held the old object (err %v)", err)
	case <-time.After(time.Second): // held behind the cleanup's lock
	}
	close(ext.release)
	if err := <-replaced; err != nil {
		t.Fatalf("replacing Put: %v", err)
	}
	select {
	case err := <-readded:
		if err != nil {
			t.Fatalf("delete and re-add: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the re-add never finished")
	}
	var kekID string
	if err := pool.QueryRow(t.Context(), `SELECT kek_id FROM secrets WHERE owned_by='bob' AND name='tok'`).Scan(&kekID); err != nil || kekID != "vaultkv:A" {
		t.Fatalf("row pointer = %q, %v; want vaultkv:A", kekID, err)
	}
	mustGetOwned(t, s, "bob", "tok", "v3")
}

// A Put answers once its row is written. The removal of the object it replaced
// runs after it returns, on a context of its own: a lock the caller holds
// around the Put is free while that removal is held, and ending the caller's
// context does not stop it.
func TestPG_PutExternal_AnswersBeforeTheReplacedObjectsRemoval(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRotExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	mustPut(t, s, "bob", "tok", "v1") // object A
	ext.pause()
	resume := sync.OnceFunc(func() { close(ext.release) })
	t.Cleanup(resume)
	var held sync.Mutex // the caller's lock around its Put, as the sign-in lock is held around a renewal's store
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		held.Lock()
		err := s.For("bob").Put(ctx, "tok", []byte("v2")) // object B; the removal of A stops in its Delete
		held.Unlock()
		done <- err
	}()
	select {
	case <-ext.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the removal of the replaced object never started")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Put waited for the removal of the object it replaced")
	}
	if !held.TryLock() {
		t.Fatal("the caller's lock is still held while the removal runs")
	}
	held.Unlock()
	if got := rowOf(t, pool, "bob", "tok"); got.kekID != "vaultkv:B" {
		t.Fatalf("row = %+v while the removal is held; want it pointing at object B", got)
	}
	cancel()
	resume()
	WaitForCleanUps()
	if _, err := ext.Get(t.Context(), "bob", "tok", "A"); err == nil {
		t.Fatal("the replaced object is still there: its removal ended with the caller's context")
	}
	mustGetOwned(t, s, "bob", "tok", "v2")
}

// A Put whose row was not written removes its value after it returns, under
// the row's lock and only if the row does not name that object then: a later
// Put that reused the object and pointed the row at it first keeps its value.
func TestPG_PutExternal_RemovalOfAMissedValueSparesALaterPutOfIt(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRotExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	// The first row write holds the row's lock until the test lets it go on,
	// then fails; a sequence, which the rollback does not undo, lets every later
	// write through.
	release := holdTestLock(t, pool)
	if _, err := pool.Exec(t.Context(), `
		CREATE SEQUENCE tok_writes;
		CREATE FUNCTION refuse_first() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF nextval('tok_writes') = 1 THEN PERFORM pg_advisory_xact_lock(7357, 1); RAISE EXCEPTION 'row refused'; END IF;
			RETURN NEW; END $$;
		CREATE TRIGGER refuse_first BEFORE INSERT ON secrets FOR EACH ROW WHEN (NEW.name = 'tok') EXECUTE FUNCTION refuse_first();`); err != nil {
		t.Fatal(err)
	}
	ext.pause() // a Delete of A would stop here until the later Put has landed
	resume := sync.OnceFunc(func() { close(ext.release) })
	t.Cleanup(resume)
	first := make(chan error, 1)
	go func() { first <- s.For("bob").Put(t.Context(), "tok", []byte("v1")) }() // object A; its row is refused
	waitRowLock(t, pool, "bob", "tok", true)
	later := make(chan error, 1)
	go func() { later <- s.For("bob").Put(t.Context(), "tok", []byte("v2")) }() // no row yet: object A again
	// Queued behind the first Put, so ahead of the removal that Put leaves.
	waitRowLock(t, pool, "bob", "tok", false)
	release()
	select {
	case err := <-first:
		if !errors.Is(err, secretstore.ErrRowNotWritten) {
			t.Fatalf("Put with its row refused = %v; want ErrRowNotWritten", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refused Put waited for the removal of its value")
	}
	select {
	case err := <-later:
		if err != nil {
			t.Fatalf("the later Put: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the later Put did not land")
	}
	resume()
	WaitForCleanUps()
	if got := rowOf(t, pool, "bob", "tok"); got.kekID != "vaultkv:A" {
		t.Fatalf("row = %+v; want it pointing at object A", got)
	}
	mustGetOwned(t, s, "bob", "tok", "v2")
}

// A rollover whose commit lands after its caller gave up leaves a clean-up of
// both objects. A Delete queued on the row's lock ahead of that clean-up takes
// the row and the new object first; the clean-up still removes the old object,
// which no row names.
func TestPG_PutExternal_ACleanUpBehindADeleteStillRemovesTheOldObject(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRotExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	mustPut(t, s, "bob", "tok", "v1") // object A
	// The rollover's commit waits for the test's lock, and lands even after its
	// caller has given up: a cancel request is caught and the wait resumed.
	release := holdTestLock(t, pool)
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION held_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			BEGIN PERFORM pg_advisory_xact_lock(7357, 1); EXCEPTION WHEN query_canceled THEN PERFORM pg_advisory_xact_lock(7357, 1); END;
			RETURN NULL; END $$;
		CREATE CONSTRAINT TRIGGER held_commit AFTER UPDATE ON secrets DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW WHEN (NEW.kek_id = 'vaultkv:B') EXECUTE FUNCTION held_commit();`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	put := make(chan error, 1)
	go func() { put <- s.For("bob").Put(ctx, "tok", []byte("v2")) }() // object B
	// Its commit waits on the test's lock, holding the row's.
	waitLock(t, pool, 7357, 1, false)
	deleted := make(chan error, 1)
	go func() { deleted <- s.For("bob").Delete(t.Context(), "tok") }()
	waitRowLock(t, pool, "bob", "tok", false) // the Delete is queued on the row's lock
	cancel()
	select {
	case err := <-put:
		if !errors.Is(err, secretstore.ErrRowNotWritten) {
			t.Fatalf("the rollover Put whose caller gave up = %v; want ErrRowNotWritten", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rollover Put did not answer once its caller gave up")
	}
	release() // the commit lands, the Delete runs, then the clean-up
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Delete never ran")
	}
	WaitForCleanUps()
	for _, obj := range []string{"A", "B"} {
		if _, err := ext.Get(t.Context(), "bob", "tok", obj); err == nil {
			t.Errorf("object %s is still in the store; want it removed, as no row names it", obj)
		}
	}
}

// holdTestLock holds the advisory lock (7357, 1), which a test's trigger waits
// on, until the returned func runs or the test ends. A goroutine of its own
// holds the connection, so the test's own queries are not nested acquires.
func holdTestLock(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	held, done := make(chan error, 1), make(chan struct{})
	go func() {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			held <- err
			return
		}
		defer conn.Release()
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(7357, 1)`); err != nil {
			held <- err
			return
		}
		held <- nil
		<-done
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7357, 1)`)
	}()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(func() { close(done) })
	t.Cleanup(release)
	return release
}

// waitRowLock returns once a session in this database holds (held) or waits
// on (!held) the row lock of (owner, name).
func waitRowLock(t *testing.T, pool *pgxpool.Pool, owner, name string, held bool) {
	t.Helper()
	waitLock(t, pool, int64(uint32(db.SecretRowLockClass)), int64(uint32(rowLockKey(owner, name))), held)
}

// waitLock returns once a session in this database holds (held) or waits on
// (!held) the two-key advisory lock (class, key).
func waitLock(t *testing.T, pool *pgxpool.Pool, class, key int64, held bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted=$3
			AND classid::bigint=$1 AND objid::bigint=$2 AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`,
			class, key, held).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session held=%v on the advisory lock (%d, %d)", held, class, key)
		}
	}
}

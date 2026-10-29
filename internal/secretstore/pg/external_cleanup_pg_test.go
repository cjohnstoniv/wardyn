// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The cleanup of a replaced store-mode object runs under the row's lock.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// rotExt is an external store that rotates object names like Azure Key Vault:
// a Put with no previous object writes "A", one that already has "A" moves to
// "B", and any other moves back to "A". Its first Delete of "A" after pause()
// stops until resume() — the cleanup of a replaced object in flight.
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
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
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
	release := sync.OnceFunc(func() { close(ext.release) })
	t.Cleanup(release) // a Fatal below still lets the paused cleanup and the Puts behind it finish before the pool closes
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
	release()
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

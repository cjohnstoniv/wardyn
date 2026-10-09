// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
)

type erasureKeys struct {
	Keys
	current, prepared, key bool
	entered, release       chan struct{}
}

func (k *erasureKeys) wait(ctx context.Context) error {
	close(k.entered)
	select {
	case <-k.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (k *erasureKeys) Current(ctx context.Context, owner, purpose string) (int, []byte, error) {
	if k.current {
		if err := k.wait(ctx); err != nil {
			return 0, nil, err
		}
	}
	version, value, err := k.Keys.Current(ctx, owner, purpose)
	if err == nil && k.prepared {
		if err := k.wait(ctx); err != nil {
			clear(value)
			return 0, nil, err
		}
	}
	return version, value, err
}

func (k *erasureKeys) Key(ctx context.Context, owner, purpose string, version int) ([]byte, error) {
	if k.key {
		if err := k.wait(ctx); err != nil {
			return nil, err
		}
	}
	return k.Keys.Key(ctx, owner, purpose, version)
}

func erasurePG(t *testing.T) (*pgxpool.Pool, Keys) {
	t.Helper()
	migrated := subjectkeytest.ThrowawayDB(t)
	cfg := migrated.Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	return pool, subjectkeytest.Manager(pool, k)
}

func erasureGeneration(t *testing.T, reg *secretmask.Registry) int64 {
	t.Helper()
	gen, err := reg.GlobalGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return gen
}

func erasureAwait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("credential work did not finish")
		return nil
	}
}

func erasureEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("credential work did not reach barrier")
	}
}

func TestPG_MaskErasureEmptyOwnerFencesPreparedWrites(t *testing.T) {
	pool, keys := erasurePG(t)
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)
	blocked := &erasureKeys{Keys: keys, current: true, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-blocked.release:
		default:
			close(blocked.release)
		}
	})
	reg := secretmask.NewRegistry()
	writer := New(pool, blocked, reg)
	eraserReg := secretmask.NewRegistry()
	eraser := New(single, keys, eraserReg)
	gen := erasureGeneration(t, reg)
	done := make(chan error, 1)
	go func() { done <- reg.MergeGlobal(gen, "alice", "sso", []byte("stale-prepared-credential")) }()
	erasureEntered(t, blocked.entered)
	// Erasure must finish while the external key call is held, even on a one-connection writer.
	if left, err := eraser.EraseOwner(t.Context(), "alice"); err != nil || left != 0 {
		t.Fatalf("empty erase = %d, %v", left, err)
	}
	close(blocked.release)
	if err := erasureAwait(t, done); !errors.Is(err, secretmask.ErrErased) {
		t.Fatalf("stale write = %v", err)
	}
	if got := reg.Snapshot(uuid.Nil); len(got) != 0 {
		t.Fatalf("stale cache has %d values", len(got))
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM mask_values WHERE NOT tombstone`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("live rows=%d err=%v", n, err)
	}
	// A new process keeps the old fence after row retention, but a new snapshot is usable.
	if _, err := eraser.SweepGlobals(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	restartReg := secretmask.NewRegistry()
	New(pool, keys, restartReg)
	if err := restartReg.MergeGlobal(gen, "alice", "sso", []byte("stale-after-restart")); !errors.Is(err, secretmask.ErrErased) {
		t.Fatalf("restart accepted old work: %v", err)
	}
	fresh := erasureGeneration(t, restartReg)
	if fresh <= gen {
		t.Fatalf("erase did not advance empty owner's generation: %d -> %d", gen, fresh)
	}
	if err := restartReg.MergeGlobal(fresh, "alice", "sso", []byte("deliberate-new-signin")); err != nil {
		t.Fatal(err)
	}
	if err := restartReg.MergeGlobal(gen, "bob", "sso", []byte("unrelated-owner-value")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Fresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(reg.Snapshot(uuid.Nil)) != 2 {
		t.Fatalf("fresh sign-in or unrelated owner missing: %d", len(reg.Snapshot(uuid.Nil)))
	}
	if _, err := eraser.EraseOwner(t.Context(), ""); err == nil {
		t.Fatal("operator namespace erased")
	}
	pool.Close()
	if err := reg.MergeGlobal(gen, "", "operator", []byte("operator-local-value")); err != nil {
		t.Fatalf("operator write needed PostgreSQL: %v", err)
	}
}

func TestPG_MaskErasureConcurrentReadCannotRestoreCache(t *testing.T) {
	pool, keys := erasurePG(t)
	seed := secretmask.NewRegistry()
	New(pool, keys, seed)
	gen := erasureGeneration(t, seed)
	if err := seed.MergeGlobal(gen, "alice", "sso", []byte("value-erased-during-read")); err != nil {
		t.Fatal(err)
	}
	gate := &erasureKeys{Keys: keys, key: true, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	reg := secretmask.NewRegistry()
	st := New(pool, gate, reg)
	readDone, eraseDone := make(chan error, 1), make(chan error, 1)
	go func() { readDone <- st.Fresh(t.Context(), time.Now()) }()
	erasureEntered(t, gate.entered)
	go func() { _, err := st.EraseOwner(t.Context(), "alice"); eraseDone <- err }()
	// A competing connection observes the committed erase before allowing the old snapshot to decrypt.
	observer := testutil.PGConn(t, pool)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var exists bool
		if err := observer.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mask_owner_erasures WHERE owner='alice')`).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			break
		}
	}
	close(gate.release)
	if err := erasureAwait(t, readDone); err != nil {
		t.Fatal(err)
	}
	if err := erasureAwait(t, eraseDone); err != nil {
		t.Fatal(err)
	}
	if len(reg.Snapshot(uuid.Nil)) != 0 {
		t.Fatal("completed erase left old read's cache value")
	}
	if err := st.Fresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(reg.Snapshot(uuid.Nil)) != 0 {
		t.Fatal("subsequent read restored erased value")
	}
}

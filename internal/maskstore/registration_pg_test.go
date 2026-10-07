// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

func TestPG_MaskErasureDestroyedKeyRefusesRegistration(t *testing.T) {
	for _, beforeCommit := range []bool{true, false} {
		t.Run(map[bool]string{true: "before_commit", false: "during_readback"}[beforeCommit], func(t *testing.T) {
			pool, keys := erasurePG(t)
			reg := secretmask.NewRegistry()
			held := &erasureKeys{Keys: keys, prepared: beforeCommit, key: !beforeCommit, entered: make(chan struct{}), release: make(chan struct{})}
			st := New(pool, held, reg)
			t.Cleanup(func() {
				select {
				case <-held.release:
				default:
					close(held.release)
				}
			})
			gen := erasureGeneration(t, reg)
			done := make(chan error, 1)
			go func() { done <- reg.MergeGlobal(gen, "alice", "sso", []byte("key-destroyed-during-registration")) }()
			erasureEntered(t, held.entered)
			// Credential erasure retires masks before destroying their key; it does not advance the mask-copy fence.
			if err := st.RetireOwnerGlobals(t.Context(), "alice", time.Now()); err != nil {
				t.Fatal(err)
			}
			if gens, err := keys.(*subjectkey.Manager).Destroy(t.Context(), "alice", subjectkey.PurposeCred); err != nil || len(gens) != 1 {
				t.Fatalf("destroyed generations=%v, err=%v", gens, err)
			}
			close(held.release)
			if err := erasureAwait(t, done); err == nil {
				t.Fatal("registration succeeded without a readable mask")
			}
			if n := len(reg.Snapshot(uuid.Nil)); n != 0 {
				t.Fatalf("failed registration cached %d values", n)
			}
			var live, retired, fences int
			err := pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE NOT tombstone), count(*) FILTER (WHERE NOT tombstone AND retired_at IS NOT NULL), (SELECT count(*) FROM mask_owner_erasures) FROM mask_values`).Scan(&live, &retired, &fences)
			want := 0
			if !beforeCommit {
				want = 1 // The unreadable retired row is skipped, so NOT tombstone is insufficient proof.
			}
			if err != nil || live != want || retired != want || fences != 0 {
				t.Fatalf("live=%d retired=%d fences=%d err=%v", live, retired, fences, err)
			}
			freshReg := secretmask.NewRegistry()
			New(pool, keys, freshReg)
			const fresh = "deliberate-signin-after-key-destruction"
			if err := freshReg.MergeGlobal(erasureGeneration(t, freshReg), "alice", "sso", []byte(fresh)); err != nil {
				t.Fatalf("new credential generation refused: %v", err)
			}
			if bytes.Contains(freshReg.Masker(uuid.Nil).Mask([]byte(fresh)), []byte(fresh)) {
				t.Fatal("new credential generation is not masked")
			}
		})
	}
}

func TestPG_MaskErasureRegistrationAcceptsUnchangedDuplicateRows(t *testing.T) {
	pool, keys := erasurePG(t)
	reg := secretmask.NewRegistry()
	New(pool, keys, reg)
	gen := erasureGeneration(t, reg)
	value := []byte("idempotent-registration-value")
	if err := reg.MergeGlobal(gen, "alice", "sso", value); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := pool.QueryRow(t.Context(), `SELECT gen FROM mask_values WHERE NOT tombstone`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := reg.MergeGlobal(gen, "alice", "sso", value, value); err != nil {
		t.Fatalf("duplicate unchanged registration refused: %v", err)
	}
	var count int
	var after int64
	if err := pool.QueryRow(t.Context(), `SELECT count(*), max(gen) FROM mask_values WHERE NOT tombstone`).Scan(&count, &after); err != nil || count != 1 || after != before {
		t.Fatalf("rows=%d generation=%d->%d err=%v", count, before, after, err)
	}
	if bytes.Contains(reg.Masker(uuid.Nil).Mask(value), value) {
		t.Fatal("unchanged credential is not masked")
	}
}

func TestPG_MaskErasureRegistrationRefusesMissingCacheValue(t *testing.T) {
	pool, keys := erasurePG(t)
	reg := secretmask.NewRegistry()
	New(pool, keys, reg)
	gen := erasureGeneration(t, reg)
	expiry := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	value := []byte("expired-registration-value")
	if err := reg.MergeGlobalUntil(gen, "alice", "sso", expiry, value); err != nil {
		t.Fatal(err)
	}
	if n := reg.SweepGlobals(time.Now()); n != 1 {
		t.Fatalf("local sweep dropped %d values", n)
	}
	// The durable row and its applied ref remain, but an unchanged write cannot vouch for the swept cache.
	if err := reg.MergeGlobalUntil(gen, "alice", "sso", expiry, value); err == nil {
		t.Fatal("registration succeeded with an applied row absent from the cache")
	}
	if n := len(reg.Snapshot(uuid.Nil)); n != 0 {
		t.Fatalf("unchanged write restored %d swept values", n)
	}
}

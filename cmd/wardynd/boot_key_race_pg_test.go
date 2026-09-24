// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Postgres-backed test for the first-boot race in loadOrCreateSecret (#754):
// two -allow-multi-instance replicas booting at once against an empty store
// must end on ONE stored key, not each on its own.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
// Run: WARDYN_TEST_PG="postgres://..." go test ./cmd/wardynd/... -run TestBootKeyCreate

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

func TestBootKeyCreate_TwoConcurrentBootsEndOnOneKey(t *testing.T) {
	poolA := pgPool(t) // migrates the DB once

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	poolB, err := db.Connect(ctx, os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect poolB: %v", err)
	}
	defer poolB.Close()

	// One age identity for both replicas, as a real deployment shares WARDYN_AGE_KEY.
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("age identity: %v", err)
	}
	storeA, err := buildSecretStore(ctx, poolA, id.String(), "pg", nil, 0, &capturingRecorder{})
	if err != nil {
		t.Fatalf("store A: %v", err)
	}
	storeB, err := buildSecretStore(ctx, poolB, id.String(), "pg", nil, 0, &capturingRecorder{})
	if err != nil {
		t.Fatalf("store B: %v", err)
	}
	name := "wardyn-test-boot-key-" + uuid.NewString()
	t.Cleanup(func() { _ = storeA.Delete(context.Background(), name) })

	// Each generate waits (up to 1s) for the other boot to reach generate too,
	// so without serialization both always do and the race is certain, not lucky.
	var gens atomic.Int32
	both := make(chan struct{})
	generate := func() ([]byte, error) {
		if gens.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(time.Second):
		}
		key := make([]byte, 32)
		_, err := rand.Read(key)
		return key, err
	}
	valid := func(b []byte) bool { return len(b) == 32 }

	keys := make([][]byte, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, s := range []bootKeyStore{
		newBootKeyStore(storeA, poolA, true),
		newBootKeyStore(storeB, poolB, true),
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = loadOrCreateSecret(ctx, s, name, valid, generate)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
	}
	stored, err := storeA.Get(secretstore.WithPurpose(ctx, secretstore.PurposeBoot), name)
	if err != nil {
		t.Fatalf("read stored key: %v", err)
	}
	for i, k := range keys {
		if !bytes.Equal(k, stored) {
			t.Errorf("boot %d serves a key that is not the stored one — it signs with a key no other replica holds", i)
		}
	}
	if n := gens.Load(); n != 1 {
		t.Errorf("generated %d keys, want 1: the second boot must read the first one's key", n)
	}
}

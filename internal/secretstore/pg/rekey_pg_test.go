// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// Postgres-backed tests for Rekey — the whole-table re-encryption behind
// `wardynd -rotate-age-key`. Both halves of its contract need a real server: the
// round trip (every row readable under the NEW key and none under the old) and
// the all-or-nothing abort (one undecryptable row leaves EVERY row untouched),
// which only a real transaction can demonstrate.
//
// These use their own throwaway CREATE DATABASE rather than the shared
// newPGStore harness, because Rekey rewrites EVERY row of `secrets`: run against
// the shared test database it would re-encrypt rows belonging to concurrently
// running sibling tests and break them. Guarded by WARDYN_TEST_PG (skips cleanly
// when unset), like every other _pg_test.go here.

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// rekeyDatabase creates a fresh database on the WARDYN_TEST_PG server, migrates
// it to HEAD (so `secrets` exists) and returns a pool for it, dropped on
// cleanup. Mirrors throwawayDatabase in internal/store's migration tests; the
// isolation is what lets a whole-table Rekey be asserted at all.
func rekeyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed rekey test")
	}
	ctx := context.Background()

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_rekey_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	// Registered first so LIFO closes the target pool BEFORE the drop: a
	// database with a live client cannot be dropped.
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = admin.Exec(cctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(cctx, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to throwaway database %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate throwaway database: %v", err)
	}
	return pool
}

func mustIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id
}

// seeded is the fixture both tests share: several secrets of different sizes so
// the round trip is not proven by a single row that happens to work.
var seeded = map[string]string{
	"github-app-key":     "-----BEGIN PRIVATE KEY-----\nmultiline\n-----END PRIVATE KEY-----\n",
	"wardyn-signing-key": "es256-pem-bytes",
	"anthropic-api-key":  "sk-ant-not-a-real-key-000000000000",
}

// TestRekeyRoundTripsEveryRow: after a rekey, every stored secret reads back
// verbatim under the NEW identity, the OLD identity reads none of them, and the
// count returned matches the rows actually rotated.
func TestRekeyRoundTripsEveryRow(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	oldID, newID := mustIdentity(t), mustIdentity(t)

	oldStore, err := New(pool, oldID)
	if err != nil {
		t.Fatalf("New(old): %v", err)
	}
	for name, val := range seeded {
		if err := oldStore.Put(ctx, name, []byte(val)); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}

	n, err := Rekey(ctx, pool, oldID, newID)
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if n != len(seeded) {
		t.Errorf("Rekey re-encrypted %d rows, want %d", n, len(seeded))
	}

	newStore, err := New(pool, newID)
	if err != nil {
		t.Fatalf("New(new): %v", err)
	}
	for name, val := range seeded {
		got, gerr := newStore.Get(ctx, name)
		if gerr != nil {
			t.Fatalf("Get %s under the NEW key: %v", name, gerr)
		}
		if !bytes.Equal(got, []byte(val)) {
			t.Errorf("Get %s = %q, want %q — the rekey corrupted the plaintext", name, got, val)
		}
		// The rotation is only real if the OLD key is now useless: a rekey that
		// left rows readable under both keys has rotated nothing.
		if _, oerr := oldStore.Get(ctx, name); oerr == nil {
			t.Errorf("Get %s still succeeds under the OLD key; the row was not re-encrypted", name)
		}
	}
}

// TestRekeyAbortsWholeTransactionOnUndecryptableRow is the failure-honesty half:
// one row the old key cannot read (here, a row written under a THIRD key — what
// a partially-restored backup or an earlier half-finished migration looks like)
// must abort the entire transaction. Nothing is committed, so every OTHER row is
// still readable under the old key and NONE is readable under the new one — there
// is no half-rotated store to reason about.
func TestRekeyAbortsWholeTransactionOnUndecryptableRow(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	oldID, newID, strayID := mustIdentity(t), mustIdentity(t), mustIdentity(t)

	oldStore, err := New(pool, oldID)
	if err != nil {
		t.Fatalf("New(old): %v", err)
	}
	for name, val := range seeded {
		if err := oldStore.Put(ctx, name, []byte(val)); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	// "aaa-stray" sorts FIRST under the ORDER BY, so the abort happens on row 0
	// — proving nothing at all was committed, not merely that the tail was.
	strayStore, err := New(pool, strayID)
	if err != nil {
		t.Fatalf("New(stray): %v", err)
	}
	if err := strayStore.Put(ctx, "aaa-stray", []byte("written under a third key")); err != nil {
		t.Fatalf("Put stray: %v", err)
	}

	n, err := Rekey(ctx, pool, oldID, newID)
	if err == nil {
		t.Fatal("Rekey succeeded over a row the old key cannot decrypt; a partial rekey was committed")
	}
	if n != 0 {
		t.Errorf("Rekey returned count %d alongside an error; an aborted rekey rotated nothing", n)
	}
	// The error has to be actionable: which row, how far it got, and that
	// nothing was committed.
	for _, want := range []string{"ABORTED", "aaa-stray", "0 of 4", "OLD key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort error %q does not mention %q", err, want)
		}
	}

	// The store is untouched: every original row still reads under the OLD key,
	// and the new key reads nothing.
	newStore, err := New(pool, newID)
	if err != nil {
		t.Fatalf("New(new): %v", err)
	}
	for name, val := range seeded {
		got, gerr := oldStore.Get(ctx, name)
		if gerr != nil {
			t.Errorf("after the abort, %s is no longer readable under the OLD key: %v", name, gerr)
			continue
		}
		if !bytes.Equal(got, []byte(val)) {
			t.Errorf("after the abort, %s = %q, want %q", name, got, val)
		}
		if _, nerr := newStore.Get(ctx, name); nerr == nil {
			t.Errorf("after the abort, %s is readable under the NEW key — the transaction was not rolled back", name)
		}
	}
}

// TestRekeyOnEmptyStore: a store with no secrets rotates cleanly to zero rows
// rather than erroring, so a fresh deployment can still run the runbook.
func TestRekeyOnEmptyStore(t *testing.T) {
	pool := rekeyDatabase(t)
	n, err := Rekey(context.Background(), pool, mustIdentity(t), mustIdentity(t))
	if err != nil {
		t.Fatalf("Rekey on an empty store: %v", err)
	}
	if n != 0 {
		t.Errorf("Rekey on an empty store re-encrypted %d rows, want 0", n)
	}
}

// TestRekey_TwoNamespacesKeepDistinctPlaintexts is 0050's negative control on
// Rekey: an operator row and a member row sharing the SAME secret name must
// each keep their OWN plaintext across a rekey. Keying the UPDATE on name
// alone (rather than (owned_by, name)) would let processing the second row
// overwrite BOTH rows with its own re-encrypted ciphertext — silently
// clobbering whichever owner was NOT the last one processed. See the doc
// comment on Rekey's per-row loop for why the fix is keying on both columns.
func TestRekey_TwoNamespacesKeepDistinctPlaintexts(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	oldID, newID := mustIdentity(t), mustIdentity(t)

	oldStore, err := New(pool, oldID)
	if err != nil {
		t.Fatalf("New(old): %v", err)
	}
	const name = "anthropic-api-key"
	operator := oldStore.For("")
	alice := oldStore.For("alice")
	if err := operator.Put(ctx, name, []byte("operator-value")); err != nil {
		t.Fatalf("Put operator row: %v", err)
	}
	if err := alice.Put(ctx, name, []byte("alice-value")); err != nil {
		t.Fatalf("Put alice row: %v", err)
	}

	n, err := Rekey(ctx, pool, oldID, newID)
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if n != 2 {
		t.Fatalf("Rekey re-encrypted %d rows, want 2 (one operator, one alice)", n)
	}

	newStore, err := New(pool, newID)
	if err != nil {
		t.Fatalf("New(new): %v", err)
	}
	gotOp, err := newStore.For("").Get(ctx, name)
	if err != nil {
		t.Fatalf("Get operator row under the new key: %v", err)
	}
	if string(gotOp) != "operator-value" {
		t.Errorf("operator row = %q, want %q — the rekey clobbered it with alice's value", gotOp, "operator-value")
	}
	gotAlice, err := newStore.For("alice").Get(ctx, name)
	if err != nil {
		t.Fatalf("Get alice row under the new key: %v", err)
	}
	if string(gotAlice) != "alice-value" {
		t.Errorf("alice row = %q, want %q — the rekey clobbered it with the operator's value", gotAlice, "alice-value")
	}
}

// TestRekeyRejectsANonX25519Identity keeps the constructor's contract visible at
// this seam: New derives the recipient from the identity, so an identity that
// cannot produce one must fail here rather than mid-transaction.
func TestRekeyRejectsANonX25519Identity(t *testing.T) {
	pool := rekeyDatabase(t)
	_, err := Rekey(context.Background(), pool, scryptOnlyIdentity{}, mustIdentity(t))
	if err == nil {
		t.Fatal("Rekey accepted an identity with no Recipient()")
	}
	if !strings.Contains(err.Error(), "old identity") {
		t.Errorf("error %q does not say which identity was rejected", err)
	}
}

// scryptOnlyIdentity is an age.Identity with no Recipient() method — the shape
// New refuses.
type scryptOnlyIdentity struct{}

func (scryptOnlyIdentity) Unwrap([]*age.Stanza) ([]byte, error) {
	return nil, errors.New("not implemented")
}

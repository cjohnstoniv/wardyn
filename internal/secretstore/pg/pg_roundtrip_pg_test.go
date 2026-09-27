// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// Postgres-backed integration tests for the envelope-encrypted secret store.
// These exercise the REAL backend end-to-end against a live Postgres (Put/Get/
// List/Delete) AND assert the at-rest invariant directly from the raw `secrets`
// columns: the stored bytes are a v1 envelope, never the plaintext. They also
// pin the boot-key fail-closed distinction the HIGH fix depends on — a decrypt
// failure must be distinguishable from a genuine not-found.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set
// against the lane's live substrate. Mirrors the db.Connect -> db.Migrate
// harness convention in internal/api/grants_confinement_test.go.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// newPGStore connects to the live Postgres named by WARDYN_TEST_PG, ensures the
// schema is migrated (so the `secrets` table exists), and returns a Store backed
// by a freshly generated age identity together with the underlying pool (for raw
// at-rest column reads). Skips (the ONLY allowed skip) when the env is absent.
func newPGStore(t *testing.T) (*Store, *pgxpool.Pool, age.Identity) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed secretstore test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := migrateTolerant(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	s, err := New(pool, id)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, pool, id
}

// migrateTolerant runs db.Migrate, retrying once on the transient
// unique-violation that can occur when this package's test binary and the
// internal/db one race to apply the SAME migration to a fresh shared DB for the
// first time (go test runs the two packages in parallel). db.Migrate is
// idempotent; the race loser just needs to re-read schema_migrations and no-op.
// This hardens the test harness only.
func migrateTolerant(ctx context.Context, pool *pgxpool.Pool) error {
	err := db.Migrate(ctx, pool)
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return db.Migrate(ctx, pool) // second pass sees an already-migrated DB
	}
	return err
}

// uniqueName returns a process-unique secret name so concurrent tests against
// the shared DB never collide and each test asserts only on rows it created.
func uniqueName(prefix string) string {
	return prefix + "/" + uuid.NewString()
}

// rawCiphertext reads the on-disk ciphertext column directly (bypassing the
// store's decrypt) so a test can assert what is ACTUALLY persisted at rest.
func rawCiphertext(t *testing.T, pool *pgxpool.Pool, name string) []byte {
	t.Helper()
	var ct []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT ciphertext FROM secrets WHERE name=$1`, name).Scan(&ct); err != nil {
		t.Fatalf("read raw ciphertext for %q: %v", name, err)
	}
	return ct
}

// TestPGPutGetRoundTripAndCiphertextAtRest is the core round-trip plus the
// at-rest invariant: Put then Get returns the plaintext verbatim, while the raw
// row is a v1 envelope — enc_version 1, this store's kek_id, a 60-byte wrapped
// DEK (nonce ‖ 32-byte key ‖ tag) and a ciphertext exactly nonce + value + tag
// long that does not contain the plaintext. This pins that secrets are
// encrypted at rest, never stored in the clear.
func TestPGPutGetRoundTripAndCiphertextAtRest(t *testing.T) {
	s, pool, _ := newPGStore(t)
	ctx := context.Background()

	name := uniqueName("gh_token")
	plain := []byte("ghp_super-secret-" + uuid.NewString())
	t.Cleanup(func() { _ = s.Delete(ctx, name) })

	if err := s.Put(ctx, name, plain); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Round-trip: Get returns exactly what we Put.
	got, err := s.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("Get = %q, want %q (round-trip mismatch)", got, plain)
	}

	// At rest: the stored column must be ciphertext, not the plaintext.
	ct := rawCiphertext(t, pool, name)
	if bytes.Equal(ct, plain) {
		t.Fatal("ciphertext column equals the plaintext; secret is NOT encrypted at rest")
	}
	if bytes.Contains(ct, plain) {
		t.Error("ciphertext column CONTAINS the plaintext bytes; encryption is leaking the secret at rest")
	}
	var version int16
	var kekID string
	var wrapped []byte
	if err := pool.QueryRow(ctx, `SELECT enc_version, kek_id, wrapped_dek FROM secrets WHERE name=$1`, name).
		Scan(&version, &kekID, &wrapped); err != nil {
		t.Fatalf("read envelope columns: %v", err)
	}
	if version != 1 || kekID != s.kek.ID() || len(wrapped) != 12+32+16 || len(ct) != 12+len(plain)+16 {
		t.Errorf("row is not a v1 envelope: enc_version=%d kek_id=%q len(wrapped_dek)=%d len(ciphertext)=%d; first bytes=%q",
			version, kekID, len(wrapped), len(ct), firstN(ct, 32))
	}
}

// TestPGGetNotFoundSentinel verifies a Get for an absent name returns the
// not-found sentinel (an error wrapping pgx.ErrNoRows), never an empty success
// the caller might inject as a credential.
func TestPGGetNotFoundSentinel(t *testing.T) {
	s, _, _ := newPGStore(t)

	got, err := s.Get(context.Background(), uniqueName("absent"))
	if err == nil {
		t.Fatalf("Get(absent) returned no error; got value %q", got)
	}
	if got != nil {
		t.Errorf("Get(absent) returned non-nil value %q alongside error", got)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("Get(absent) error %v does not wrap pgx.ErrNoRows (the not-found sentinel)", err)
	}
}

// TestPGDecryptFailureDistinctFromNotFound backs the boot-key fail-closed HIGH
// fix: a value present in the DB but undecryptable (encrypted under a DIFFERENT
// key, i.e. corrupted/wrong-key ciphertext) must surface as a decrypt FAILURE,
// NOT as not-found. If a decrypt failure were indistinguishable from not-found,
// a key mismatch could be mistaken for "no such secret" and silently swallowed
// instead of failing closed.
func TestPGDecryptFailureDistinctFromNotFound(t *testing.T) {
	// store A writes a value under key A.
	storeA, pool, _ := newPGStore(t)
	ctx := context.Background()

	name := uniqueName("wrong-key")
	t.Cleanup(func() { _ = storeA.Delete(ctx, name) })
	if err := storeA.Put(ctx, name, []byte("only-A-can-read")); err != nil {
		t.Fatalf("Put under key A: %v", err)
	}

	// store B uses a DIFFERENT identity over the SAME pool: the row exists, but
	// B cannot decrypt it.
	idB, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate key B: %v", err)
	}
	storeB, err := New(pool, idB)
	if err != nil {
		t.Fatalf("New(B): %v", err)
	}

	_, errWrongKey := storeB.Get(ctx, name)
	if errWrongKey == nil {
		t.Fatal("Get with the WRONG key succeeded; a key mismatch must fail, not return the plaintext")
	}
	// Crucially, this is NOT a not-found: the row IS present, B just cannot
	// decrypt it. The two failure modes must be distinguishable.
	if errors.Is(errWrongKey, pgx.ErrNoRows) {
		t.Error("decrypt-failure error is indistinguishable from not-found (wraps pgx.ErrNoRows); " +
			"a wrong/missing boot key would be mistaken for an absent secret instead of failing closed")
	}

	// And the genuine not-found path for a name that does NOT exist still maps to
	// the not-found sentinel — proving the two are truly distinct error classes.
	_, errAbsent := storeB.Get(ctx, uniqueName("really-absent"))
	if !errors.Is(errAbsent, pgx.ErrNoRows) {
		t.Errorf("genuine-absent Get error %v does not wrap pgx.ErrNoRows", errAbsent)
	}
}

// firstN returns the first n bytes of b (or all of b if shorter), for failure
// messages that show a prefix without dumping the whole ciphertext.
func firstN(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

// TestPGHarnessBlobIsPerOwnerAndIsolated is the storage half of the per-user
// AWS SSO credential (0.7.2). The whole lane rests on two properties of this
// table that no in-memory double can prove:
//
//  1. the primary key is COMPOSITE — (owned_by, name) — so two principals hold
//     rows of the SAME reserved harness name side by side. A name-only key would
//     make the second person's sign-in overwrite the first's, silently, and the
//     failure would look like "my session keeps logging out".
//  2. List is OWN-ROWS-ONLY, while Get falls back to the operator's. That pair
//     is exactly why the API layer reads a per_user blob List-then-Get: the bare
//     Get would serve the admin's session to a member who has captured nothing.
func TestPGHarnessBlobIsPerOwnerAndIsolated(t *testing.T) {
	s, _, _ := newPGStore(t)
	ctx := context.Background()
	name := uniqueName("wardyn-harness-aws-oauth")
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()

	if err := s.For("").Put(ctx, name, []byte("operator-session")); err != nil {
		t.Fatalf("operator put: %v", err)
	}
	if err := s.For(alice).Put(ctx, name, []byte("alice-session")); err != nil {
		t.Fatalf("alice put: %v", err)
	}
	if err := s.For(bob).Put(ctx, name, []byte("bob-session")); err != nil {
		t.Fatalf("bob put: %v", err)
	}

	// (1) Three rows of ONE name coexist, each read back verbatim.
	for owner, want := range map[string]string{"": "operator-session", alice: "alice-session", bob: "bob-session"} {
		got, err := s.For(owner).Get(ctx, name)
		if err != nil {
			t.Fatalf("get for %q: %v", owner, err)
		}
		if string(got) != want {
			t.Errorf("owner %q read %q, want %q — the composite key did not keep the rows apart", owner, got, want)
		}
	}

	// (2) List is own-rows-only: it is what lets a caller tell "this principal
	// captured a session" from "Get would have fallen back to the operator's".
	carol := "carol-" + uuid.NewString()
	names, err := s.For(carol).List(ctx)
	if err != nil {
		t.Fatalf("carol list: %v", err)
	}
	for _, n := range names {
		if n == name {
			t.Fatalf("List for a principal who stored nothing returned %q — own-rows-only is what the per_user read depends on", n)
		}
	}
	// …while Get for that same principal DOES fall back, which is the contract
	// the API layer must not use for a per_user credential.
	if got, gerr := s.For(carol).Get(ctx, name); gerr != nil || string(got) != "operator-session" {
		t.Fatalf("Get for a principal with no row = (%q, %v); the documented operator fallback is the trap per_user closes", got, gerr)
	}

	// Deleting one person's row leaves everybody else's alone.
	if err := s.For(alice).Delete(ctx, name); err != nil {
		t.Fatalf("alice delete: %v", err)
	}
	if got, gerr := s.For(bob).Get(ctx, name); gerr != nil || string(got) != "bob-session" {
		t.Errorf("bob's session after alice's delete = (%q, %v), want it untouched", got, gerr)
	}
	if got, gerr := s.For("").Get(ctx, name); gerr != nil || string(got) != "operator-session" {
		t.Errorf("the operator's session after alice's delete = (%q, %v), want it untouched", got, gerr)
	}
}

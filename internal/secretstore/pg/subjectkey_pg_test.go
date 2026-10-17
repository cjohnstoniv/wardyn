// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The pg store's half of subject keys, against a real Postgres: principal keys
// counted as local rows, moved by an age-key rotation, and the `secrets` rewrap
// passes leaving a credential row under a principal key alone.

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// LocalRows counts a live principal key wrapped under a local key, so
// refuseIdleAgeKey never advises removing an age key that still wraps one; a
// destroyed one holds no wrap and does not count.
func TestLocalRowsCountsPrincipalKeys(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := t.Context()
	s, err := New(pool, mustIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.LocalRows(ctx); err != nil || n != 0 {
		t.Fatalf("LocalRows on an empty store = (%d, %v)", n, err)
	}
	if _, _, err := s.SubjectKeys().Current(ctx, "alice", subjectkey.PurposeAuditSeal); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LocalRows(ctx); err != nil || n != 1 {
		t.Fatalf("LocalRows with one local principal key = (%d, %v); want 1", n, err)
	}
	// A principal key under a key service is not local: one row that names a
	// service kek_id.
	if _, err := pool.Exec(ctx, `INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key) VALUES ('bob', 'cred', 1, 'default', 'transit:transit/x', '\x01')`); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.LocalRows(ctx); n != 1 {
		t.Fatalf("LocalRows counted a principal key under a key service: %d", n)
	}
	if _, err := s.SubjectKeys().Destroy(ctx, "alice", subjectkey.PurposeAuditSeal); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.LocalRows(ctx); n != 0 {
		t.Fatalf("LocalRows counted a destroyed principal key: %d", n)
	}
}

// An age-key rotation rewraps the principal keys with the rows, in one
// transaction: afterwards the new key opens a principal key and the old one
// does not.
func TestRekeyMovesPrincipalKeys(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := t.Context()
	oldID, newID := mustIdentity(t), mustIdentity(t)
	oldStore, err := New(pool, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldStore.For("alice").Put(ctx, "pat", []byte("alice-value")); err != nil {
		t.Fatal(err)
	}
	v, key, err := oldStore.SubjectKeys().Current(ctx, "alice", subjectkey.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := kek.Seal(key, []byte("masked"), kek.Encode("test"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := Rekey(ctx, pool, oldID, newID, nil); err != nil || n != 1 {
		t.Fatalf("Rekey = (%d, %v); want the one secret", n, err)
	}
	newStore, err := New(pool, newID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := newStore.SubjectKeys().Key(ctx, "alice", subjectkey.PurposeCred, v)
	if err != nil {
		t.Fatalf("Key under the NEW age key: %v", err)
	}
	if plain, err := kek.Open(got, sealed, kek.Encode("test")); err != nil || string(plain) != "masked" {
		t.Fatalf("a value sealed before the rotation = (%q, %v)", plain, err)
	}
	cold, err := New(pool, oldID) // a restarted wardynd with the old key: no warm cache
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cold.SubjectKeys().Key(ctx, "alice", subjectkey.PurposeCred, v); err == nil {
		t.Fatal("the OLD age key still opens the principal key; the rotation did not move it")
	}
}

// The `secrets` passes skip a credential row under a principal key (enc_version
// 3, whose data key is not under any root) instead of aborting on its version,
// and leave it byte for byte as it was.
func TestRewrapSkipsPrincipalKeyRows(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := t.Context()
	id := mustIdentity(t)
	s, err := New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.For("alice").Put(ctx, "pat", []byte("v1-value")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ('alice', 'pk-row', 3, 'pk:v1', '\x01', '\x02')`); err != nil {
		t.Fatal(err)
	}
	res, err := RewrapKeys(ctx, secretstore.Deps{Pool: pool, AgeIdentity: id})
	if err != nil {
		t.Fatalf("RewrapKeys with a v3 row present: %v", err)
	}
	if res.Rewrapped != 0 {
		t.Fatalf("RewrapKeys moved %d rows; the v1 row is already where a write puts it", res.Rewrapped)
	}
	if n, err := Rekey(ctx, pool, id, mustIdentity(t), nil); err != nil || n != 1 {
		t.Fatalf("Rekey with a v3 row present = (%d, %v); want the one v1 row moved", n, err)
	}
	var kekID string
	if err := pool.QueryRow(ctx, `SELECT kek_id FROM secrets WHERE owned_by='alice' AND name='pk-row'`).Scan(&kekID); err != nil || kekID != "pk:v1" {
		t.Fatalf("the v3 row after Rekey = (%q, %v); want it untouched", kekID, err)
	}
}

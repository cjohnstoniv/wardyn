// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// Subject keys under the Transit KEK, against a real Postgres (WARDYN_TEST_PG)
// and the fake Vault: the subjectkey contract, and root rotation that covers
// principal_keys so a Transit version is retirable only once they are at it too.

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

func TestSubjectKeys_TransitContract(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	newFakeTransit(t, f)
	subjectkeytest.Run(t, pool, func(t *testing.T) kek.KEK {
		tr, err := openFakeTransit(t, f)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	})
}

// With audit-seal rows beside v1 credential rows, a rotation and -rewrap move
// both tables, and only then is the old Transit version safe to retire. The
// control: with the old version retired first, the principal key is stranded.
func TestSubjectKeys_RewrapRetiresOldVersionsOnlyOnceBothTablesAreAtIt(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	tr := newFakeTransit(t, f)
	s := pgStore(t, pool, nil, tr, true)
	ctx := t.Context()
	if err := s.For("alice").Put(ctx, "pat", []byte("alice-value")); err != nil {
		t.Fatal(err)
	}
	sk := s.SubjectKeys()
	v, key, err := sk.Current(ctx, "alice", subjectkey.PurposeAuditSeal)
	if err != nil {
		t.Fatal(err)
	}
	aad := kek.Encode("test", "alice")
	sealed, err := kek.Seal(key, []byte("sealed field"), aad)
	if err != nil {
		t.Fatal(err)
	}
	setMin := func(n int) {
		f.mu.Lock()
		f.transit.minDecrypt = n
		f.mu.Unlock()
	}

	f.rotateTransit()
	setMin(2)
	if _, err := pgStore(t, pool, nil, tr, true).SubjectKeys().Key(ctx, "alice", subjectkey.PurposeAuditSeal, v); err == nil || errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Key under a retired version = %v; want a definitive refusal (the principal key would be stranded without rotation coverage)", err)
	}
	setMin(0)

	res, err := secretstorepg.RewrapKeys(ctx, keys(pool, nil, tr, true))
	if err != nil || res.Rewrapped != 1 || res.PrincipalKeys != 1 || res.KeyVersion != "2" {
		t.Fatalf("RewrapKeys = (%+v, %v); want 1 secret and 1 principal key at v2", res, err)
	}
	var wraps [][]byte
	for _, q := range []string{`SELECT wrapped_dek FROM secrets`, `SELECT wrapped_key FROM principal_keys WHERE destroyed_at IS NULL`} {
		rows, err := pool.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var w []byte
			if err := rows.Scan(&w); err != nil {
				t.Fatal(err)
			}
			wraps = append(wraps, w)
		}
		rows.Close()
	}
	if len(wraps) != 2 {
		t.Fatalf("%d wraps, want one in each table", len(wraps))
	}
	for _, w := range wraps {
		if n, err := tr.WrapVersion(w); err != nil || n != res.KeyVersion {
			t.Fatalf("a wrap is at version (%q, %v); want %s in both tables", n, err, res.KeyVersion)
		}
	}
	setMin(2)
	restarted := pgStore(t, pool, nil, tr, true)
	if got, err := restarted.For("alice").Get(ctx, "pat"); err != nil || string(got) != "alice-value" {
		t.Fatalf("Get after retirement and restart = (%q, %v)", got, err)
	}
	got, err := restarted.SubjectKeys().Key(ctx, "alice", subjectkey.PurposeAuditSeal, v)
	if err != nil {
		t.Fatalf("Key after retirement and restart: %v", err)
	}
	if plain, err := kek.Open(got, sealed, aad); err != nil || string(plain) != "sealed field" {
		t.Fatalf("a field sealed before the rotation = (%q, %v)", plain, err)
	}
	if res, err := secretstorepg.RewrapKeys(ctx, keys(pool, nil, tr, true)); err != nil || res.Rewrapped != 0 || res.PrincipalKeys != 0 {
		t.Fatalf("a second RewrapKeys = (%+v, %v); want nothing to move", res, err)
	}
}

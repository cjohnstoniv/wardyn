// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// kekStore builds the registered "pg" store with k wrapping every write.
func kekStore(t *testing.T, pool *pgxpool.Pool, k kek.KEK) *secretstorepg.Store {
	t.Helper()
	s, err := secretstore.New("pg", secretstore.Deps{Pool: pool, KEK: k, KEKWrites: true})
	if err != nil {
		t.Fatalf("secretstore.New(pg): %v", err)
	}
	return s.(*secretstorepg.Store)
}

// The Key Vault KEK through the pg store, end to end: rows (a boot key among
// them) are sealed under it; after a rotation of both keys `-rewrap` moves
// every row onto the new versions and a second run moves none; with every
// other version disabled every row still reads, on a restarted wardynd too;
// and a wrap and value copied onto another row in SQL are refused without an
// unwrapkey call.
func TestKEK_PGRotateRewrapRetire(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	s := kekStore(t, pool, k)
	ctx := t.Context()
	rows := []struct{ owner, name, value string }{
		{"", "wardyn-signing-key", "boot-key"},
		{"", "github-app-key", "operator-value"},
		{"alice@example.com", "pat", "alice-value"},
	}
	for _, r := range rows {
		if err := s.For(r.owner).Put(ctx, r.name, []byte(r.value)); err != nil {
			t.Fatalf("Put(%s/%s): %v", r.owner, r.name, err)
		}
	}
	readAll := func(s *secretstorepg.Store, when string) {
		t.Helper()
		for _, r := range rows {
			if v, err := s.For(r.owner).Get(ctx, r.name); err != nil || string(v) != r.value {
				t.Fatalf("%s: Get(%s/%s) = (%q, %v)", when, r.owner, r.name, v, err)
			}
		}
	}
	readAll(s, "before the rotation")

	wv := f.rotateKey(fakeWrapKey)
	sv := f.rotateKey(fakeSignKey)
	d := secretstore.Deps{Pool: pool, KEK: k, KEKWrites: true}
	res, err := secretstorepg.RewrapKeys(ctx, d)
	if err != nil || res.Rewrapped != 3 || res.KeyService != k.ID() || res.KeyVersion != wv+"/"+sv {
		t.Fatalf("RewrapKeys after a rotation = (%+v, %v); want 3 rows at %s/%s", res, err, wv, sv)
	}
	if res, err := secretstorepg.RewrapKeys(ctx, d); err != nil || res.Rewrapped != 0 {
		t.Fatalf("a second RewrapKeys = (%+v, %v); want 0 rows", res, err)
	}

	f.disableAllBut(fakeWrapKey, wv)
	f.disableAllBut(fakeSignKey, sv)
	readAll(s, "with every other version disabled")
	readAll(kekStore(t, pool, newFakeKEK(t, f)), "on a restarted wardynd")

	if _, err := pool.Exec(ctx, `UPDATE secrets AS b SET wrapped_dek=a.wrapped_dek, ciphertext=a.ciphertext
		FROM secrets AS a WHERE a.owned_by='' AND a.name='github-app-key' AND b.owned_by='alice@example.com' AND b.name='pat'`); err != nil {
		t.Fatal(err)
	}
	f.resetCalls()
	_, err = s.For("alice@example.com").Get(ctx, "pat")
	if err == nil || errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get of a row carrying another row's wrap = %v; want a definitive refusal", err)
	}
	if n := f.cryptoCalls("unwrapkey"); n != 0 {
		t.Fatalf("a moved wrap cost %d unwrapkey calls; want 0", n)
	}
}

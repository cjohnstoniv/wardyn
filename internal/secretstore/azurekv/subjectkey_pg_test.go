// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

// Subject keys under the Key Vault KEK, against a real Postgres (WARDYN_TEST_PG)
// and the fake Key Vault: the subjectkey contract, and root rotation that covers
// principal_keys so a Key Vault version is retirable only once they are at it too.

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

func TestSubjectKeys_KeyVaultContract(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	subjectkeytest.Run(t, pool, func(t *testing.T) kek.KEK { return newFakeKEK(t, f) })
}

// With audit-seal rows beside v1 credential rows, a rotation of both keys and
// -rewrap move both tables, and only then are the old versions safe to disable.
// The control: with the old versions disabled first, the principal key is stranded.
func TestSubjectKeys_RewrapRetiresOldVersionsOnlyOnceBothTablesAreAtIt(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	s := kekStore(t, pool, k)
	ctx := t.Context()
	if err := s.For("alice").Put(ctx, "pat", []byte("alice-value")); err != nil {
		t.Fatal(err)
	}
	v, key, err := s.SubjectKeys().Current(ctx, "alice", subjectkey.PurposeAuditSeal)
	if err != nil {
		t.Fatal(err)
	}
	aad := kek.Encode("test", "alice")
	sealed, err := kek.Seal(key, []byte("sealed field"), aad)
	if err != nil {
		t.Fatal(err)
	}

	wv := f.rotateKey(fakeWrapKey)
	sv := f.rotateKey(fakeSignKey)
	f.disableAllBut(fakeWrapKey, wv)
	f.disableAllBut(fakeSignKey, sv)
	if _, err := kekStore(t, pool, newFakeKEK(t, f)).SubjectKeys().Key(ctx, "alice", subjectkey.PurposeAuditSeal, v); err == nil || errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Key under disabled versions = %v; want a definitive refusal (the principal key would be stranded without rotation coverage)", err)
	}
	f.enableAll(fakeWrapKey)
	f.enableAll(fakeSignKey)

	res, err := secretstorepg.RewrapKeys(ctx, secretstore.Deps{Pool: pool, KEK: k, KEKWrites: true})
	if err != nil || res.Rewrapped != 1 || res.PrincipalKeys != 1 || res.KeyVersion != wv+"/"+sv {
		t.Fatalf("RewrapKeys = (%+v, %v); want 1 secret and 1 principal key at %s/%s", res, err, wv, sv)
	}
	f.disableAllBut(fakeWrapKey, wv)
	f.disableAllBut(fakeSignKey, sv)
	restarted := kekStore(t, pool, newFakeKEK(t, f))
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
}

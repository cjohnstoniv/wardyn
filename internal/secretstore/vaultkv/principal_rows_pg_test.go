// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// Credential rows under principal keys (enc_version 3) beside every other row
// format, with the principal keys wrapped under the fake Transit key: a root
// rotation and retirement leaves v1 rows, a vaultkv pointer and v3 rows all
// readable after a restart, and the boot keys stay v1 with the flag on.

import (
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

func TestPrincipalRows_TransitRetirementReadsEveryFormat(t *testing.T) {
	for _, principal := range []bool{true, false} {
		t.Run(fmt.Sprintf("flag_%v", principal), func(t *testing.T) {
			pool := throwawayDB(t)
			f := newFakeVault(t)
			tr := newFakeTransit(t, f)
			ext := newFakeStore(t, f)
			ctx := t.Context()
			// open builds a store over the shared database as a (re)started wardynd would.
			open := func(on bool) *secretstorepg.Store {
				d := keys(pool, nil, tr, true)
				d.External, d.PrincipalKeys = ext, on
				s, err := secretstore.New("pg", d)
				if err != nil {
					t.Fatal(err)
				}
				return s.(*secretstorepg.Store)
			}
			seed, on := open(false), open(true)

			put := func(s secretstore.Store, owner, name, value string) {
				t.Helper()
				if err := s.For(owner).Put(ctx, name, []byte(value)); err != nil {
					t.Fatalf("Put (%q, %q): %v", owner, name, err)
				}
			}
			put(seed, "", "operator-token", "operator-value")
			put(seed, "bob", "v1-cred", "bob-v1")
			put(storeMode(t, pool, ext, nil), "bob", "vault-cred", "bob-vault")
			put(on, "alice", "pk-a", "alice-v3")
			put(on, "bob", "pk-b", "bob-v3")
			// The boot keys, with the flag as under test: owner "" never takes v3.
			sk := open(principal)
			for name := range secretstore.PlatformNames {
				put(sk, "", name, "boot-"+name)
			}

			// An azurekv pointer sits in the database too; this store cannot reach it,
			// and a rotation leaves it exactly as it is.
			if _, err := pool.Exec(ctx, `INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ('zoe', 'azure-cred', 2, 'azurekv:ref', ''::bytea, ''::bytea)`); err != nil {
				t.Fatal(err)
			}

			version := func(owner, name string) (int16, string) {
				var v int16
				var kekID string
				if err := pool.QueryRow(ctx, `SELECT enc_version, kek_id FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&v, &kekID); err != nil {
					t.Fatalf("row (%q, %q): %v", owner, name, err)
				}
				return v, kekID
			}
			for name := range secretstore.PlatformNames {
				if v, id := version("", name); v != 1 || id != tr.ID() {
					t.Fatalf("boot key %q = (v%d, %q) with the flag %v; want v1 under Transit", name, v, id, principal)
				}
				got, err := open(principal).Get(secretstore.WithPurpose(ctx, secretstore.PurposeBoot), name)
				if err != nil || string(got) != "boot-"+name {
					t.Fatalf("boot key %q read back = (%q, %v)", name, got, err)
				}
			}
			if v, _ := version("alice", "pk-a"); v != 3 {
				t.Fatalf("alice's row is enc_version %d; want 3", v)
			}
			var v3Before []byte
			if err := pool.QueryRow(ctx, `SELECT wrapped_dek FROM secrets WHERE owned_by='alice' AND name='pk-a'`).Scan(&v3Before); err != nil {
				t.Fatal(err)
			}

			// Rotate the Transit key, move every wrap onto it, then retire the old version.
			f.rotateTransit()
			res, err := secretstorepg.RewrapKeys(ctx, func() secretstore.Deps { d := keys(pool, nil, tr, true); d.External = ext; return d }())
			if err != nil || res.KeyVersion != "2" || res.PrincipalKeys != 2 {
				t.Fatalf("RewrapKeys = (%+v, %v); want both principal keys moved to Transit v2", res, err)
			}
			var v3After []byte
			if err := pool.QueryRow(ctx, `SELECT wrapped_dek FROM secrets WHERE owned_by='alice' AND name='pk-a'`).Scan(&v3After); err != nil {
				t.Fatal(err)
			}
			if string(v3Before) != string(v3After) {
				t.Fatal("a root rotation changed a v3 row's wrapped data key; its data key is under the principal key, not the root")
			}
			f.mu.Lock()
			f.transit.minDecrypt = 2
			f.mu.Unlock()

			// A restarted wardynd (cold caches) reads every format it can reach.
			restarted := open(principal)
			for _, c := range []struct{ owner, name, want string }{
				{"", "operator-token", "operator-value"},
				{"bob", "v1-cred", "bob-v1"},
				{"bob", "vault-cred", "bob-vault"},
				{"alice", "pk-a", "alice-v3"},
				{"bob", "pk-b", "bob-v3"},
			} {
				got, err := restarted.For(c.owner).Get(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), c.name)
				if err != nil || string(got) != c.want {
					t.Fatalf("after retirement and restart, Get (%q, %q) = (%q, %v); want %q", c.owner, c.name, got, err, c.want)
				}
			}
			if v, id := version("zoe", "azure-cred"); v != 2 || id != "azurekv:ref" {
				t.Fatalf("the azurekv pointer = (v%d, %q) after the rotation; want it untouched", v, id)
			}
			if _, err := restarted.SubjectKeys().Key(ctx, "alice", subjectkey.PurposeCred, 1); err != nil {
				t.Fatalf("alice's principal key after retirement and restart: %v", err)
			}
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE enc_version=3 AND owned_by=''`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("v3 rows owned by the operator namespace = (%d, %v); want none", n, err)
			}
		})
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

// Credential rows under principal keys (enc_version 3) beside every other row
// format, with the principal keys wrapped under the fake Key Vault KEK: after a
// rotation of both keys and the disabling of every older version, v1 rows, an
// azurekv pointer and v3 rows all read on a restarted wardynd.

import (
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

func TestPrincipalRows_KeyVaultRetirementReadsEveryFormat(t *testing.T) {
	for _, principal := range []bool{true, false} {
		t.Run(fmt.Sprintf("flag_%v", principal), func(t *testing.T) {
			pool := throwawayDB(t)
			f := newFakeKV(t)
			k := newFakeKEK(t, f)
			ext := newFakeStore(t, f)
			ctx := t.Context()
			open := func(on bool, kk *KEK) *secretstorepg.Store {
				s, err := secretstore.New("pg", secretstore.Deps{Pool: pool, KEK: kk, KEKWrites: true, External: ext, PrincipalKeys: on})
				if err != nil {
					t.Fatal(err)
				}
				return s.(*secretstorepg.Store)
			}
			put := func(s secretstore.Store, owner, name, value string) {
				t.Helper()
				if err := s.For(owner).Put(ctx, name, []byte(value)); err != nil {
					t.Fatalf("Put (%q, %q): %v", owner, name, err)
				}
			}
			put(open(false, k), "", "operator-token", "operator-value")
			put(open(false, k), "bob", "v1-cred", "bob-v1")
			akv, err := secretstore.New(Name, secretstore.Deps{Pool: pool, KEK: k, KEKWrites: true, External: ext})
			if err != nil {
				t.Fatal(err)
			}
			put(akv, "bob", "akv-cred", "bob-akv")
			put(open(true, k), "alice", "pk-a", "alice-v3")
			for name := range secretstore.PlatformNames {
				put(open(principal, k), "", name, "boot-"+name)
			}
			for name := range secretstore.PlatformNames {
				var v int16
				if err := pool.QueryRow(ctx, `SELECT enc_version FROM secrets WHERE owned_by='' AND name=$1`, name).Scan(&v); err != nil || v != 1 {
					t.Fatalf("boot key %q = (v%d, %v) with the flag %v; want v1", name, v, err, principal)
				}
			}

			wv := f.rotateKey(fakeWrapKey)
			sv := f.rotateKey(fakeSignKey)
			res, err := secretstorepg.RewrapKeys(ctx, secretstore.Deps{Pool: pool, KEK: k, KEKWrites: true, External: ext})
			if err != nil || res.PrincipalKeys != 1 || res.KeyVersion != wv+"/"+sv {
				t.Fatalf("RewrapKeys = (%+v, %v); want the principal key moved to %s/%s", res, err, wv, sv)
			}
			f.disableAllBut(fakeWrapKey, wv)
			f.disableAllBut(fakeSignKey, sv)

			restarted := open(principal, newFakeKEK(t, f))
			for _, c := range []struct{ owner, name, want string }{
				{"", "operator-token", "operator-value"},
				{"bob", "v1-cred", "bob-v1"},
				{"bob", "akv-cred", "bob-akv"},
				{"alice", "pk-a", "alice-v3"},
			} {
				got, err := restarted.For(c.owner).Get(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), c.name)
				if err != nil || string(got) != c.want {
					t.Fatalf("after retirement and restart, Get (%q, %q) = (%q, %v); want %q", c.owner, c.name, got, err, c.want)
				}
			}
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE enc_version=3 AND owned_by=''`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("v3 rows owned by the operator namespace = (%d, %v); want none", n, err)
			}
		})
	}
}

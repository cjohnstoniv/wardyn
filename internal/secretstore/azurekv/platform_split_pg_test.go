// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

// The Key Vault platform split against a real Postgres (WARDYN_TEST_PG) and
// two fake vaults: the boot keys are wrapped and signed by a second key pair
// reached as a second Entra identity, every other row stays under the
// credential pair, and -rewrap moves the boot keys onto the platform pair and
// back. The Transit analogue is internal/secretstore/vaultkv/transit_pg_test.go.

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

const (
	bootKey        = "wardyn-signing-key"
	platformClient = "client-platform"
)

// platformPair opens the platform KEK over a vault of its own, as a second
// Entra identity: the fake vault only exchanges that client id.
func platformPair(t *testing.T) (*fakeKV, *KEK) {
	t.Helper()
	f := newFakeKV(t)
	f.client = platformClient
	return f, newFakeKEK(t, f)
}

// platformDeps are the Deps of a store whose boot keys have a key pair of
// their own; writes false leaves that pair read-only (the retire direction).
func platformDeps(pool *pgxpool.Pool, cred, plat *KEK, writes bool) secretstore.Deps {
	return secretstore.Deps{Pool: pool, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: writes}
}

func newStore(t *testing.T, d secretstore.Deps) *secretstorepg.Store {
	t.Helper()
	s, err := secretstore.New("pg", d)
	if err != nil {
		t.Fatalf("secretstore.New(pg): %v", err)
	}
	return s.(*secretstorepg.Store)
}

func mustPut(t *testing.T, s secretstore.Store, owner, name, v string) {
	t.Helper()
	if err := s.For(owner).Put(t.Context(), name, []byte(v)); err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, s secretstore.Store, owner, name, want string) {
	t.Helper()
	if v, err := s.For(owner).Get(t.Context(), name); err != nil || string(v) != want {
		t.Fatalf("Get(%q, %q) = (%q, %v), want %q", owner, name, v, err, want)
	}
}

func rowKEKID(t *testing.T, pool *pgxpool.Pool, owner, name string) (kekID string, wrapped []byte) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), `SELECT kek_id, wrapped_dek FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&kekID, &wrapped); err != nil {
		t.Fatal(err)
	}
	return kekID, wrapped
}

// With the platform pair the boot keys alone are wrapped under it; every other
// row, a person's row that borrows a boot key's name included, stays under the
// credential pair. A daemon holding only the credential pair cannot open a boot
// key, and a boot key planted under the credential pair, which is what a leaked
// credential-identity token can do, is refused at the serving boot.
func TestPlatformSplit_BootKeysWrapApart(t *testing.T) {
	pool := throwawayDB(t)
	cred := newFakeKEK(t, newFakeKV(t))
	_, plat := platformPair(t)
	s := newStore(t, platformDeps(pool, cred, plat, true))
	mustPut(t, s, "", bootKey, "boot")
	mustPut(t, s, "", "github-app-key", "cred")
	mustPut(t, s, "alice", bootKey, "alice")
	for _, w := range []struct{ owner, name, want string }{
		{"", bootKey, plat.ID()}, {"", "github-app-key", cred.ID()}, {"alice", bootKey, cred.ID()},
	} {
		if k, _ := rowKEKID(t, pool, w.owner, w.name); k != w.want {
			t.Fatalf("row (%q, %q) is sealed under %q, want %q", w.owner, w.name, k, w.want)
		}
	}
	mustGet(t, s, "", bootKey, "boot")
	mustGet(t, s, "alice", bootKey, "alice")

	credOnly := newStore(t, secretstore.Deps{Pool: pool, KEK: cred, KEKWrites: true})
	if _, err := credOnly.Get(t.Context(), bootKey); err == nil || errors.Is(err, secretstore.ErrNotFound) || !strings.Contains(err.Error(), "not configured to reach") {
		t.Fatalf("credential-pair-only Get of a boot key = %v; want a refusal naming the key", err)
	}

	if _, err := pool.Exec(t.Context(), `DELETE FROM secrets WHERE owned_by='' AND name=$1`, bootKey); err != nil {
		t.Fatal(err)
	}
	mustPut(t, credOnly, "", bootKey, "forged")
	if v, err := s.Get(t.Context(), bootKey); err == nil || errors.Is(err, secretstore.ErrNotFound) || !strings.Contains(err.Error(), "opens boot keys only under") {
		t.Fatalf("Get of a boot key under the credential pair = (%q, %v); want a refusal", v, err)
	}
}

// -rewrap -rewrap-adopt-boot-keys moves the boot keys onto the platform pair
// and -rewrap -rewrap-retire-platform-key, with the pair read-only, back onto
// the credential pair, each at its own latest versions: a credential row is
// never touched and a second run does nothing.
func TestPlatformSplit_RewrapAdoptAndRetire(t *testing.T) {
	pool := throwawayDB(t)
	cred := newFakeKEK(t, newFakeKV(t))
	fp, plat := platformPair(t)
	ctx := t.Context()
	one := newStore(t, secretstore.Deps{Pool: pool, KEK: cred, KEKWrites: true})
	mustPut(t, one, "", bootKey, "boot")
	mustPut(t, one, "", "github-app-key", "cred")
	_, credWrap := rowKEKID(t, pool, "", "github-app-key")

	rewrap := func(writes bool) secretstorepg.RewrapResult {
		t.Helper()
		d := platformDeps(pool, cred, plat, writes)
		d.AdoptBootKeys = true // the first run onto the platform pair is the adoption
		res, err := secretstorepg.RewrapKeys(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	check := func(wantKEK string) {
		t.Helper()
		if k, _ := rowKEKID(t, pool, "", bootKey); k != wantKEK {
			t.Fatalf("boot key is sealed under %q, want %q", k, wantKEK)
		}
		if _, w := rowKEKID(t, pool, "", "github-app-key"); string(w) != string(credWrap) {
			t.Fatal("-rewrap rewrapped a credential row")
		}
		mustGet(t, one, "", "github-app-key", "cred")
	}

	// Adopt: onto the platform pair.
	if res := rewrap(true); res.Rewrapped != 1 || res.PlatformKeyService != plat.ID() || res.PlatformKeyVersion == "" || res.KeyService != cred.ID() {
		t.Fatalf("rewrap onto the platform pair = %+v", res)
	}
	check(plat.ID())
	if res := rewrap(true); res.Rewrapped != 0 {
		t.Fatalf("second rewrap moved %d rows", res.Rewrapped)
	}
	mustGet(t, newStore(t, platformDeps(pool, cred, plat, true)), "", bootKey, "boot")

	// A rotation of the platform pair alone moves the boot key once; the
	// credential pair, not rotated, is not chased.
	wv, sv := fp.rotateKey(fakeWrapKey), fp.rotateKey(fakeSignKey)
	if res := rewrap(true); res.Rewrapped != 1 || res.PlatformKeyVersion != wv+"/"+sv {
		t.Fatalf("rewrap after rotating the platform pair = %+v; want the boot key at %s/%s", res, wv, sv)
	}
	if res := rewrap(true); res.Rewrapped != 0 {
		t.Fatalf("second rewrap after the rotation moved %d rows", res.Rewrapped)
	}

	// Retire: back onto the credential pair.
	if res := rewrap(false); res.Rewrapped != 1 || res.PlatformKeyService != "" {
		t.Fatalf("rewrap back = %+v", res)
	}
	check(cred.ID())
	if res := rewrap(false); res.Rewrapped != 0 {
		t.Fatalf("second rewrap back moved %d rows", res.Rewrapped)
	}
	mustGet(t, one, "", bootKey, "boot")
}

// The platform pair is reached as its own Entra identity: the credential
// identity's client id is not exchanged for a token at the platform vault, so a
// KEK built with it cannot boot there.
func TestPlatformSplit_PlatformPairNeedsItsOwnIdentity(t *testing.T) {
	f := newFakeKV(t)
	f.client = platformClient
	if _, err := openFakeKEK(t, f, func(c *KEKConfig) { c.ClientID = "client-1" }); err == nil {
		t.Fatal("the credential identity booted a KEK at the platform vault; want the token exchange refused")
	}
	if _, err := openFakeKEK(t, f, func(c *KEKConfig) { c.ClientID = platformClient }); err != nil {
		t.Fatalf("the platform identity could not boot the platform KEK: %v", err)
	}
}

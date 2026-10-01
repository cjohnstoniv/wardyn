// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// The Transit KEK under the pg store, against a real Postgres (WARDYN_TEST_PG)
// and the fake Vault: rows sealed under Transit, reads dispatched on kek_id,
// and the client-side -rewrap in both directions.

import (
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

// keys are the Deps of a pg store. id may be nil (no age key); tr may be nil
// (no key service), and writes says whether it wraps new rows.
func keys(pool *pgxpool.Pool, id age.Identity, tr *Transit, writes bool) secretstore.Deps {
	d := secretstore.Deps{Pool: pool, AgeIdentity: id, KEKWrites: writes}
	if tr != nil {
		d.KEK = tr
	}
	return d
}

// pgStore builds the registered "pg" store over keys(pool, id, tr, writes).
func pgStore(t *testing.T, pool *pgxpool.Pool, id age.Identity, tr *Transit, writes bool) *secretstorepg.Store {
	t.Helper()
	s, err := secretstore.New("pg", keys(pool, id, tr, writes))
	if err != nil {
		t.Fatalf("secretstore.New(pg): %v", err)
	}
	return s.(*secretstorepg.Store)
}

func TestTransitKEK_Conformance(t *testing.T) {
	pool := throwawayDB(t)
	tr := newFakeTransit(t, newFakeVault(t))
	secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store { return pgStore(t, pool, nil, tr, true) })
}

// A tampered row under Transit is a refusal, never not-found.
func TestTransitKEK_TamperConformance(t *testing.T) {
	pool := throwawayDB(t)
	tr := newFakeTransit(t, newFakeVault(t))
	secretstoretest.RunTamperConformance(t, func(t *testing.T) secretstore.Store { return pgStore(t, pool, nil, tr, true) },
		secretstoretest.FlipCiphertext(pool))
}

func rowKEK(t *testing.T, pool *pgxpool.Pool, owner, name string) (string, []byte) {
	t.Helper()
	var id string
	var wrapped []byte
	if err := pool.QueryRow(t.Context(), `SELECT kek_id, wrapped_dek FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&id, &wrapped); err != nil {
		t.Fatal(err)
	}
	return id, wrapped
}

// A row sealed under Transit names it, holds a Transit wrap, and refuses to
// open when its wrap and value are moved under another person's row — the
// associated_data binding, end to end. The refusal is never not-found.
func TestTransitKEK_RowIsBoundAndMovedRowRefused(t *testing.T) {
	pool := throwawayDB(t)
	tr := newFakeTransit(t, newFakeVault(t))
	s := pgStore(t, pool, nil, tr, true)
	ctx := t.Context()
	if err := s.For("alice").Put(ctx, "pat", []byte("alice-secret")); err != nil {
		t.Fatal(err)
	}
	if err := s.For("bob").Put(ctx, "pat", []byte("bob-secret")); err != nil {
		t.Fatal(err)
	}
	id, wrapped := rowKEK(t, pool, "alice", "pat")
	if id != "transit:transit/wardyn" || !strings.HasPrefix(string(wrapped), "vault:v1:") {
		t.Fatalf("row = (%q, %q), want a Transit wrap", id, wrapped)
	}
	if _, err := pool.Exec(ctx, `UPDATE secrets SET (wrapped_dek, ciphertext) =
		(SELECT wrapped_dek, ciphertext FROM secrets WHERE owned_by='alice' AND name='pat')
		WHERE owned_by='bob' AND name='pat'`); err != nil {
		t.Fatal(err)
	}
	v, err := s.For("bob").Get(ctx, "pat")
	if err == nil || errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Get of a row carrying another person's wrap = (%q, %v); want a definitive refusal", v, err)
	}
	if !strings.Contains(err.Error(), `owned_by="bob"`) {
		t.Fatalf("refusal %v does not name the row", err)
	}
}

// Online migration local → Transit and back: reads follow each row's kek_id
// while writes go where WARDYN_KEK says, and -rewrap moves the rest.
func TestTransitKEK_OnlineMigrationBothWays(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	tr := newFakeTransit(t, f)
	id, _ := age.GenerateX25519Identity()
	ctx := t.Context()

	local := pgStore(t, pool, id, nil, false)
	for _, n := range []string{"a", "b"} {
		if err := local.Put(ctx, n, []byte("v-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	// A pure-Transit daemon cannot read the local rows, and says so by name.
	if _, err := pgStore(t, pool, nil, tr, true).Get(ctx, "a"); err == nil || !strings.Contains(err.Error(), "no WARDYN_AGE_KEY") {
		t.Fatalf("Get of a local row with no age key = %v", err)
	}

	both := pgStore(t, pool, id, tr, true)
	if err := both.Put(ctx, "c", []byte("v-c")); err != nil {
		t.Fatal(err)
	}
	if k, _ := rowKEK(t, pool, "", "c"); k != tr.ID() {
		t.Fatalf("a write with WARDYN_KEK=transit is sealed under %q", k)
	}
	for _, n := range []string{"a", "b", "c"} {
		if v, err := both.Get(ctx, n); err != nil || string(v) != "v-"+n {
			t.Fatalf("Get(%s) = (%q, %v)", n, v, err)
		}
	}
	res, err := secretstorepg.RewrapKeys(ctx, keys(pool, id, tr, true))
	if err != nil || res.Rewrapped != 2 || res.KeyService != tr.ID() || res.KeyVersion != "1" {
		t.Fatalf("Rewrap = (%+v, %v), want 2 rows to %s v1", res, err, tr.ID())
	}
	if n, _ := both.LocalRows(ctx); n != 0 {
		t.Fatalf("%d local rows left after -rewrap", n)
	}
	if res, err := secretstorepg.RewrapKeys(ctx, keys(pool, id, tr, true)); err != nil || res.Rewrapped != 0 {
		t.Fatalf("second Rewrap = (%+v, %v), want nothing to do", res, err)
	}
	transitOnly := pgStore(t, pool, nil, tr, true)
	for _, n := range []string{"a", "b", "c"} {
		if v, err := transitOnly.Get(ctx, n); err != nil || string(v) != "v-"+n {
			t.Fatalf("Transit-only Get(%s) = (%q, %v)", n, v, err)
		}
	}

	// A daemon without the key service refuses a Transit row by name (rule 15's
	// KEK twin), never as not-found — so loadOrCreateSecret never mints over it.
	if _, err := pgStore(t, pool, id, nil, false).Get(ctx, "a"); err == nil || errors.Is(err, secretstore.ErrNotFound) || !strings.Contains(err.Error(), "not configured to reach") {
		t.Fatalf("Get of a Transit row with no key service = %v; want a refusal naming the key", err)
	}

	// Back: the key service read-only, the local key writing.
	res, err = secretstorepg.RewrapKeys(ctx, keys(pool, id, tr, false))
	if err != nil || res.Rewrapped != 3 || res.KeyService != "" || res.KeyVersion != "" {
		t.Fatalf("Rewrap back = (%+v, %v), want 3 rows to the local key", res, err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if v, err := pgStore(t, pool, id, nil, false).Get(ctx, n); err != nil || string(v) != "v-"+n {
			t.Fatalf("local-only Get(%s) after the move back = (%q, %v)", n, v, err)
		}
	}
	if got := f.callCount("POST transit/rewrap"); got != 0 {
		t.Fatalf("Vault's server-side rewrap was called %d times", got)
	}
}

// After a Transit rotation, -rewrap moves every row to the latest version, so
// min_decryption_version can retire the old one with every row still readable.
// Raised early, the rows it strands refuse definitively, naming the row.
func TestTransitKEK_RewrapRetiresOldVersions(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	tr := newFakeTransit(t, f)
	s := pgStore(t, pool, nil, tr, true)
	ctx := t.Context()
	for _, n := range []string{"a", "b"} {
		if err := s.Put(ctx, n, []byte("v-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	f.rotateTransit()
	f.mu.Lock()
	f.transit.minDecrypt = 2
	f.mu.Unlock()
	_, err := s.Get(ctx, "a")
	if err == nil || errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), `name="a"`) {
		t.Fatalf("Get of a row under a retired version = %v; want a definitive refusal naming the row", err)
	}
	f.mu.Lock()
	f.transit.minDecrypt = 0
	f.mu.Unlock()
	res, err := secretstorepg.RewrapKeys(ctx, keys(pool, nil, tr, true))
	if err != nil || res.Rewrapped != 2 || res.KeyVersion != "2" {
		t.Fatalf("Rewrap after a rotation = (%+v, %v), want 2 rows to v2", res, err)
	}
	f.mu.Lock()
	f.transit.minDecrypt = 2
	f.mu.Unlock()
	for _, n := range []string{"a", "b"} {
		if v, err := s.Get(ctx, n); err != nil || string(v) != "v-"+n {
			t.Fatalf("Get(%s) with v1 retired = (%q, %v)", n, v, err)
		}
	}
}

// A transient Vault failure reaches the caller as ErrUnavailable (the sink's
// K8 grace), never as a refusal or not-found.
func TestTransitKEK_OutageIsTransient(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := pgStore(t, pool, nil, newFakeTransit(t, f), true)
	ctx := t.Context()
	if err := s.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.force = []int{503, 503, 503, 503}
	f.mu.Unlock()
	_, err := s.Get(ctx, "k")
	if !errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get while Vault is sealed = %v; want ErrUnavailable", err)
	}
}

// -rotate-age-key rewraps the rows under the age key and leaves the rows under
// Transit alone, instead of aborting on them.
func TestTransitKEK_RotateAgeKeyLeavesTransitRows(t *testing.T) {
	pool := throwawayDB(t)
	tr := newFakeTransit(t, newFakeVault(t))
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateX25519Identity()
	ctx := t.Context()
	if err := pgStore(t, pool, oldID, nil, false).Put(ctx, "local-row", []byte("l")); err != nil {
		t.Fatal(err)
	}
	if err := pgStore(t, pool, oldID, tr, true).Put(ctx, "transit-row", []byte("t")); err != nil {
		t.Fatal(err)
	}
	_, before := rowKEK(t, pool, "", "transit-row")
	n, err := secretstorepg.Rekey(ctx, pool, oldID, newID, nil)
	if err != nil || n != 1 {
		t.Fatalf("Rekey = (%d, %v), want the one local row", n, err)
	}
	if _, after := rowKEK(t, pool, "", "transit-row"); string(after) != string(before) {
		t.Fatal("Rekey rewrote a row sealed under Transit")
	}
	s := pgStore(t, pool, newID, tr, true)
	for n, want := range map[string]string{"local-row": "l", "transit-row": "t"} {
		if v, err := s.Get(ctx, n); err != nil || string(v) != want {
			t.Fatalf("Get(%s) after the rotation = (%q, %v)", n, v, err)
		}
	}
}

const bootKey = "wardyn-signing-key"

// platformKeys are the Deps of a store whose boot keys have a Transit key of
// their own; writes false leaves that key read-only.
func platformKeys(pool *pgxpool.Pool, cred, plat *Transit, writes bool) secretstore.Deps {
	d := keys(pool, nil, cred, true)
	d.PlatformKEK, d.PlatformKEKWrites = plat, writes
	return d
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

// With a platform key service the boot keys alone are wrapped under it; every
// other row, a person's row that borrows a boot key's name included, stays
// under the credential key. The credential key's holder cannot open a boot
// key, and a boot key forged under the credential key is refused.
func TestPlatformKEK_BootKeysWrapApart(t *testing.T) {
	pool := throwawayDB(t)
	cred := newFakeTransit(t, newFakeVault(t))
	plat := newFakeTransitKey(t, newFakeVault(t), "wardyn-platform")
	s, err := secretstore.New("pg", platformKeys(pool, cred, plat, true))
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, "", bootKey, "boot")
	mustPut(t, s, "", "github-app-key", "cred")
	mustPut(t, s, "alice", bootKey, "alice")
	for _, w := range []struct{ owner, name, want string }{
		{"", bootKey, plat.ID()}, {"", "github-app-key", cred.ID()}, {"alice", bootKey, cred.ID()},
	} {
		if k, _ := rowKEK(t, pool, w.owner, w.name); k != w.want {
			t.Fatalf("row (%q, %q) is sealed under %q, want %q", w.owner, w.name, k, w.want)
		}
	}
	mustGet(t, s, "", bootKey, "boot")
	mustGet(t, s, "alice", bootKey, "alice")

	// A daemon holding only the credential key cannot read the boot key.
	if _, err := pgStore(t, pool, nil, cred, true).Get(t.Context(), bootKey); err == nil || errors.Is(err, secretstore.ErrNotFound) || !strings.Contains(err.Error(), "not configured to reach") {
		t.Fatalf("credential-key-only Get of a boot key = %v; want a refusal naming the key", err)
	}

	// A boot key written under the credential key (by one who holds it and the
	// database) is not opened once the platform key service is configured.
	if _, err := pool.Exec(t.Context(), `DELETE FROM secrets WHERE owned_by='' AND name=$1`, bootKey); err != nil {
		t.Fatal(err)
	}
	mustPut(t, pgStore(t, pool, nil, cred, true), "", bootKey, "forged")
	if v, err := s.Get(t.Context(), bootKey); err == nil || !strings.Contains(err.Error(), "opens boot keys only under") {
		t.Fatalf("Get of a boot key under the credential key = (%q, %v); want a refusal", v, err)
	}
}

// -rewrap moves the boot keys onto the platform key service and, with it
// read-only, back onto the credential key, at each key's own latest version:
// a credential row is never touched and a second run does nothing.
func TestPlatformKEK_RewrapBothWaysIsIdempotent(t *testing.T) {
	pool := throwawayDB(t)
	fp := newFakeVault(t)
	cred := newFakeTransit(t, newFakeVault(t))
	plat := newFakeTransitKey(t, fp, "wardyn-platform")
	ctx := t.Context()
	one := pgStore(t, pool, nil, cred, true)
	mustPut(t, one, "", bootKey, "boot")
	mustPut(t, one, "", "github-app-key", "cred")
	_, credWrap := rowKEK(t, pool, "", "github-app-key")

	rewrap := func(writes bool) secretstorepg.RewrapResult {
		t.Helper()
		d := platformKeys(pool, cred, plat, writes)
		d.AdoptBootKeys = true // the first run onto the platform key is the adoption
		res, err := secretstorepg.RewrapKeys(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	check := func(wantKEK string) {
		t.Helper()
		if k, _ := rowKEK(t, pool, "", bootKey); k != wantKEK {
			t.Fatalf("boot key is sealed under %q, want %q", k, wantKEK)
		}
		if _, w := rowKEK(t, pool, "", "github-app-key"); string(w) != string(credWrap) {
			t.Fatal("-rewrap rewrapped a credential row")
		}
		mustGet(t, one, "", "github-app-key", "cred")
	}

	// Onto the platform key.
	if res := rewrap(true); res.Rewrapped != 1 || res.PlatformKeyService != plat.ID() || res.PlatformKeyVersion != "1" || res.KeyService != cred.ID() || res.KeyVersion != "1" {
		t.Fatalf("rewrap onto the platform key = %+v", res)
	}
	check(plat.ID())
	if res := rewrap(true); res.Rewrapped != 0 {
		t.Fatalf("second rewrap moved %d rows", res.Rewrapped)
	}
	mustGet(t, secretstoreNew(t, platformKeys(pool, cred, plat, true)), "", bootKey, "boot")

	// A rotation of the platform key alone moves the boot key to v2 once; the
	// credential key, still at v1, is not chased.
	fp.rotateTransit()
	if res := rewrap(true); res.Rewrapped != 1 || res.PlatformKeyVersion != "2" {
		t.Fatalf("rewrap after rotating the platform key = %+v", res)
	}
	if res := rewrap(true); res.Rewrapped != 0 {
		t.Fatalf("second rewrap after the rotation moved %d rows", res.Rewrapped)
	}

	// Back onto the credential key.
	if res := rewrap(false); res.Rewrapped != 1 || res.PlatformKeyService != "" {
		t.Fatalf("rewrap back = %+v", res)
	}
	check(cred.ID())
	if res := rewrap(false); res.Rewrapped != 0 {
		t.Fatalf("second rewrap back moved %d rows", res.Rewrapped)
	}
	mustGet(t, one, "", bootKey, "boot")
}

// No platform key service: a boot key is wrapped under the credential key, as
// before, and the rewrap reports no platform key.
func TestPlatformKEK_UnsetKeepsOneKey(t *testing.T) {
	pool := throwawayDB(t)
	cred := newFakeTransit(t, newFakeVault(t))
	s := pgStore(t, pool, nil, cred, true)
	mustPut(t, s, "", bootKey, "boot")
	if k, w := rowKEK(t, pool, "", bootKey); k != cred.ID() || !strings.HasPrefix(string(w), "vault:v1:") {
		t.Fatalf("boot key = (%q, %q), want a wrap under the credential key", k, w)
	}
	res, err := secretstorepg.RewrapKeys(t.Context(), keys(pool, nil, cred, true))
	if err != nil || res.Rewrapped != 0 || res.PlatformKeyService != "" || res.PlatformKeyVersion != "" || res.KeyService != cred.ID() || res.KeyVersion != "1" {
		t.Fatalf("rewrap = (%+v, %v)", res, err)
	}
	mustGet(t, s, "", bootKey, "boot")
}

func secretstoreNew(t *testing.T, d secretstore.Deps) secretstore.Store {
	t.Helper()
	s, err := secretstore.New("pg", d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

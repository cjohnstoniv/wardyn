// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// Store mode end to end: the pg store selected as "vaultkv", its pointer rows
// in a real Postgres, its values in the in-process fake Vault. Guarded by
// WARDYN_TEST_PG; each test gets its own throwaway database.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

func throwawayDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed store-mode test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_vkv_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// storeMode builds the registered "vaultkv" store; id may be nil (no age key).
func storeMode(t *testing.T, pool *pgxpool.Pool, ext secretstore.External, id age.Identity) *secretstorepg.Store {
	t.Helper()
	s, err := secretstore.New(Name, secretstore.Deps{Pool: pool, AgeIdentity: id, External: ext})
	if err != nil {
		t.Fatalf("secretstore.New(vaultkv): %v", err)
	}
	return s.(*secretstorepg.Store)
}

func TestVaultKV_Conformance(t *testing.T) {
	pool := throwawayDB(t)
	ext := newFakeStore(t, newFakeVault(t))
	secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store { return storeMode(t, pool, ext, nil) })
}

func TestStoreMode_RowIsAPointerAndHoldsNoValue(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	if err := s.For("alice").Put(ctx, "pat", []byte("value-bytes")); err != nil {
		t.Fatal(err)
	}
	var ver int16
	var kekID string
	var wrapped, ct []byte
	if err := pool.QueryRow(ctx, `SELECT enc_version, kek_id, wrapped_dek, ciphertext FROM secrets WHERE owned_by='alice' AND name='pat'`).
		Scan(&ver, &kekID, &wrapped, &ct); err != nil {
		t.Fatal(err)
	}
	want := "vaultkv:wardyn/ns1/people/" + strings.ToLower(b32.EncodeToString([]byte("alice"))) + "/pat"
	if ver != 2 || kekID != want || len(wrapped) != 0 || len(ct) != 0 {
		t.Fatalf("row = (v%d, %q, %d wrapped, %d ct), want (v2, %q, 0, 0)", ver, kekID, len(wrapped), len(ct), want)
	}
	if s.Name() != Name || !strings.HasPrefix(s.StoresExternally(), "Vault at ") {
		t.Fatalf("Name/StoresExternally = %q/%q", s.Name(), s.StoresExternally())
	}
}

// Rule 17: a row whose value is gone at the store is a refusal, never
// ErrNotFound, so loadOrCreateSecret can never mint a boot key over it.
func TestStoreMode_ValueGoneBehindARowIsNotNotFound(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	if err := s.Put(ctx, "wardyn-signing-key", []byte("pem")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.kv, "ns1/platform/wardyn-signing-key")
	f.mu.Unlock()
	_, err := s.Get(ctx, "wardyn-signing-key")
	if err == nil || errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get with the value gone = %v; want a refusal that is NOT ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "wardyn-signing-key") {
		t.Fatalf("refusal does not name the row: %v", err)
	}
}

// Rule 16 through the database: a writer who points Alice's row at Bob's
// value is refused, and the refusal names Alice's row.
func TestStoreMode_PointerMovedToAnotherOwnerIsRefused(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	if err := s.For("bob").Put(ctx, "pat", []byte("bob-secret")); err != nil {
		t.Fatal(err)
	}
	if err := s.For("alice").Put(ctx, "pat", []byte("alice-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE secrets SET kek_id=(SELECT kek_id FROM secrets WHERE owned_by='bob' AND name='pat') WHERE owned_by='alice' AND name='pat'`); err != nil {
		t.Fatal(err)
	}
	v, err := s.For("alice").Get(ctx, "pat")
	if err == nil || string(v) == "bob-secret" {
		t.Fatalf("alice's moved pointer read (%q, %v); want a refusal", v, err)
	}
	if !strings.Contains(err.Error(), `owned_by="alice"`) {
		t.Fatalf("refusal does not name alice's row: %v", err)
	}
}

// Rule 15: a pointer row on an install with no Vault client refuses by name.
func TestStoreMode_PointerWithNoStoreConfiguredRefusesByName(t *testing.T) {
	pool := throwawayDB(t)
	s := storeMode(t, pool, newFakeStore(t, newFakeVault(t)), nil)
	if err := s.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	id, _ := age.GenerateX25519Identity()
	local, err := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: id})
	if err != nil {
		t.Fatal(err)
	}
	_, err = local.Get(t.Context(), "k")
	if err == nil || errors.Is(err, secretstore.ErrNotFound) || !strings.Contains(err.Error(), `"vaultkv"`) {
		t.Fatalf("pg-only Get of a vaultkv row = %v; want a refusal naming the store", err)
	}
	if err := local.Delete(t.Context(), "k"); err == nil {
		t.Fatal("pg-only Delete removed a vaultkv row whose value it cannot reach")
	}
}

// Rule 18, Put: a store failure writes no row, and no row write is even
// attempted; a row failure comes after the value reached Vault, and removes it.
func TestStoreMode_PutWritesTheStoreBeforeTheRow(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()

	// Every row write issued for 'k' leaves a note in row_writes that a later
	// compensating DELETE of the row does not remove.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE row_writes (name text);
		CREATE FUNCTION note_row_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO row_writes VALUES (NEW.name); RETURN NEW; END $$;
		CREATE TRIGGER note_row_write BEFORE INSERT OR UPDATE ON secrets FOR EACH ROW WHEN (NEW.name = 'k') EXECUTE FUNCTION note_row_write();`); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.force = []int{403, 403}
	f.mu.Unlock()
	if err := s.Put(ctx, "k", []byte("v")); err == nil {
		t.Fatal("Put succeeded with Vault refusing")
	}
	var n, attempts int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE name='k'`).Scan(&n)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM row_writes`).Scan(&attempts)
	if n != 0 || attempts != 0 {
		t.Fatalf("with the store write refused: %d rows, %d row writes attempted; want 0 and 0 (store before row)", n, attempts)
	}

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION refuse_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'row refused'; END $$;
		CREATE TRIGGER refuse_boom BEFORE INSERT OR UPDATE ON secrets FOR EACH ROW WHEN (NEW.name = 'boom') EXECUTE FUNCTION refuse_boom();`); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "boom", []byte("v")); err == nil || !strings.Contains(err.Error(), "removed again") {
		t.Fatalf("Put with the row refused = %v; want the value removed again", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.kv["ns1/operator/boom"]; ok {
		t.Fatal("the value outlived its failed row (an orphan)")
	}
	write, remove := -1, -1
	for i, c := range f.calls {
		switch c {
		case "POST wardyn/data/ns1/operator/boom":
			write = i
		case "DELETE wardyn/metadata/ns1/operator/boom":
			remove = i
		}
	}
	if write < 0 || remove < write {
		t.Fatalf("Vault calls %v: want the value written, then removed after the row failed", f.calls)
	}
}

// Rule 18, Delete: a store failure keeps the row, and the credential with it.
func TestStoreMode_DeleteRemovesTheStoreBeforeTheRow(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	if err := s.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.force = []int{403, 403}
	f.mu.Unlock()
	if err := s.Delete(ctx, "k"); err == nil {
		t.Fatal("Delete succeeded with Vault refusing")
	}
	if v, err := s.Get(ctx, "k"); err != nil || string(v) != "v" {
		t.Fatalf("after a failed Delete, Get = (%q, %v); want the value intact", v, err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kv) != 0 {
		t.Fatalf("Delete left %d paths in Vault", len(f.kv))
	}
}

// DeleteEverywhere removes every owner's value from the store, not only the
// rows, and a store failure keeps every row.
func TestStoreMode_DeleteEverywhereRemovesEveryOwnersValue(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	for _, owner := range []string{"", "alice", "bob"} {
		if err := s.For(owner).Put(ctx, "k", []byte("v-"+owner)); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	f.force = []int{403, 403}
	f.mu.Unlock()
	if _, err := s.DeleteEverywhere(ctx, []string{"k"}); err == nil {
		t.Fatal("DeleteEverywhere succeeded with Vault refusing")
	}
	if v, err := s.For("bob").Get(ctx, "k"); err != nil || string(v) != "v-bob" {
		t.Fatalf("after a failed DeleteEverywhere, Get = (%q, %v); want the value intact", v, err)
	}
	f.mu.Lock()
	f.force = nil
	f.mu.Unlock()
	if n, err := s.DeleteEverywhere(ctx, []string{"k"}); err != nil || n != 3 {
		t.Fatalf("DeleteEverywhere = (%d, %v), want 3 rows removed", n, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kv) != 0 {
		t.Fatalf("DeleteEverywhere left %d paths in Vault", len(f.kv))
	}
}

// The half-way shape: one owner's external delete goes through before a
// second owner's is refused. The call still errors and every row survives —
// including the first owner's, now a dangling pointer — that dangling
// pointer reads as a definitive refusal, never ErrNotFound (rule 17), and a
// retry once the fault clears finishes the job.
func TestStoreMode_DeleteEverywhereHalfway(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	for _, owner := range []string{"alice", "bob"} {
		if err := s.For(owner).Put(ctx, "k", []byte("v-"+owner)); err != nil {
			t.Fatal(err)
		}
	}

	// Vault refuses every call naming bob's path. deleteExternalEverywhere's
	// SELECT carries no ORDER BY, so this produces the half-way shape (one
	// owner through, one refused) only if Postgres visits alice's row before
	// bob's — true in practice for a freshly inserted 2-row match, though not
	// guaranteed by the query itself; the assertions below don't assume WHICH
	// owner that is, only that exactly one of them is.
	f.mu.Lock()
	f.denyPath = "people/" + strings.ToLower(b32.EncodeToString([]byte("bob"))) + "/"
	f.mu.Unlock()

	if _, err := s.DeleteEverywhere(ctx, []string{"k"}); err == nil {
		t.Fatal("DeleteEverywhere succeeded with bob's delete refused")
	}
	alicePath := "ns1/people/" + strings.ToLower(b32.EncodeToString([]byte("alice"))) + "/k"
	bobPath := "ns1/people/" + strings.ToLower(b32.EncodeToString([]byte("bob"))) + "/k"
	f.mu.Lock()
	_, aliceLive := f.kv[alicePath]
	_, bobLive := f.kv[bobPath]
	f.mu.Unlock()
	if aliceLive == bobLive {
		t.Fatalf("alice live=%v, bob live=%v; want exactly one owner's value removed (the half-way shape)", aliceLive, bobLive)
	}
	deletedOwner, keptOwner, keptValue := "alice", "bob", "v-bob"
	if aliceLive {
		deletedOwner, keptOwner, keptValue = "bob", "alice", "v-alice"
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE name='k'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d rows survive the failed DeleteEverywhere, want 2", n)
	}

	// The fault only ever named bob's DELETE; clear it before reading back, so
	// the Gets below check the row/value state, not the fault itself.
	f.mu.Lock()
	f.denyPath = ""
	f.mu.Unlock()

	// deletedOwner's row is now a dangling pointer: a definitive refusal,
	// never ErrNotFound, so loadOrCreateSecret can never mint a boot key over it.
	if _, err := s.For(deletedOwner).Get(ctx, "k"); err == nil || errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get on %s's dangling pointer = %v; want a refusal that is NOT ErrNotFound", deletedOwner, err)
	}
	// keptOwner's value is untouched.
	if v, err := s.For(keptOwner).Get(ctx, "k"); err != nil || string(v) != keptValue {
		t.Fatalf("Get on %s = (%q, %v); want the value intact", keptOwner, v, err)
	}

	// A retry with the fault cleared completes and leaves no rows.
	if n, err := s.DeleteEverywhere(ctx, []string{"k"}); err != nil || n != 2 {
		t.Fatalf("retried DeleteEverywhere = (%d, %v), want (2, nil)", n, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kv) != 0 {
		t.Fatalf("DeleteEverywhere left %d paths in Vault", len(f.kv))
	}
}

// Rule 22: migrate both ways, idempotent, nothing left behind.
func TestMigrate_BothWaysLeavesNothingBehind(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	ext := newFakeStore(t, f)
	id, _ := age.GenerateX25519Identity()
	ctx := t.Context()
	local, err := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: id, External: ext})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[[2]string]string{{"", "wardyn-signing-key"}: "pem", {"", "github-app-key"}: "k", {"alice", "pat"}: "a\x00\xff"}
	for r, v := range rows {
		if err := local.For(r[0]).Put(ctx, r[1], []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	lps := local.(*secretstorepg.Store)
	reads := 0
	count := func(string, string) { reads++ }

	check := func(wantVersion int16) {
		t.Helper()
		for r, v := range rows {
			got, err := local.For(r[0]).Get(ctx, r[1])
			if err != nil || string(got) != v {
				t.Fatalf("Get %v = (%q, %v), want %q", r, got, err, v)
			}
		}
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE enc_version <> $1`, wantVersion).Scan(&n)
		if n != 0 {
			t.Fatalf("%d rows are not at enc_version %d", n, wantVersion)
		}
	}

	if res, err := lps.Migrate(ctx, Name, count); err != nil || res.Moved != 3 || reads != 3 {
		t.Fatalf("Migrate to vaultkv = (%d, %v), %d reads; want 3 moved, 3 reads", res.Moved, err, reads)
	}
	check(2)
	var leftover int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE length(ciphertext) > 0 OR length(wrapped_dek) > 0`).Scan(&leftover)
	if leftover != 0 {
		t.Fatalf("%d rows still hold a local copy after moving to Vault", leftover)
	}
	if res, err := lps.Migrate(ctx, Name, count); err != nil || res.Moved != 0 {
		t.Fatalf("re-run = (%d, %v), want (0, nil)", res.Moved, err)
	}

	if res, err := lps.Migrate(ctx, secretstorepg.MigrateLocal, count); err != nil || res.Moved != 3 {
		t.Fatalf("Migrate to local = (%d, %v), want 3", res.Moved, err)
	}
	check(1)
	f.mu.Lock()
	left := len(f.kv)
	f.mu.Unlock()
	if left != 0 {
		t.Fatalf("Vault still holds %d values after moving back", left)
	}
	if res, err := lps.Migrate(ctx, secretstorepg.MigrateLocal, count); err != nil || res.Moved != 0 {
		t.Fatalf("re-run = (%d, %v), want (0, nil)", res.Moved, err)
	}
}

// The migrator never overwrites a value it did not put there: an orphan at
// the target path (or a concurrent Put) aborts it, naming the row.
func TestMigrate_RefusesToOverwriteAValueAlreadyAtTheTarget(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	ext := newFakeStore(t, f)
	id, _ := age.GenerateX25519Identity()
	ctx := t.Context()
	local, _ := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: id, External: ext})
	if err := local.Put(ctx, "k", []byte("local")); err != nil {
		t.Fatal(err)
	}
	if _, err := ext.Put(ctx, "", "k", "", []byte("other"), false); err != nil {
		t.Fatal(err)
	}
	_, err := local.(*secretstorepg.Store).Migrate(ctx, Name, func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), `name="k"`) {
		t.Fatalf("Migrate over an existing value = %v; want an abort naming the row", err)
	}
	if v, _ := local.Get(ctx, "k"); string(v) != "local" {
		t.Fatalf("row value = %q, want it untouched", v)
	}
}

// Rules 18 and 22: Vault answers 404 to a write under a mount it does not
// serve. The migrator must abort with nothing moved and the local copy intact,
// not flip the row to a pointer at nothing.
func TestMigrate_AbortsOnAMountVaultDoesNotServe(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	ext := newFakeStore(t, f)
	id, _ := age.GenerateX25519Identity()
	ctx := t.Context()
	local, _ := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: id, External: ext})
	if err := local.Put(ctx, "wardyn-signing-key", []byte("pem")); err != nil {
		t.Fatal(err)
	}
	ext.mount = "typo"
	res, err := local.(*secretstorepg.Store).Migrate(ctx, Name, func(string, string) {})
	if err == nil || res.Moved != 0 {
		t.Fatalf("Migrate to an unserved mount = (%d, %v); want an abort with 0 moved", res.Moved, err)
	}
	var ver int16
	var ct []byte
	if err := pool.QueryRow(ctx, `SELECT enc_version, ciphertext FROM secrets WHERE name='wardyn-signing-key'`).Scan(&ver, &ct); err != nil {
		t.Fatal(err)
	}
	if ver != 1 || len(ct) == 0 {
		t.Fatalf("row after the abort = (v%d, %d ciphertext bytes); want the local copy intact", ver, len(ct))
	}
	if v, err := local.Get(ctx, "wardyn-signing-key"); err != nil || string(v) != "pem" {
		t.Fatalf("Get after the abort = (%q, %v); want the value", v, err)
	}
}

// A database writer who points Alice's row at Bob's orphan cannot hide the
// orphan: a row claims only the path its owner and name derive.
func TestReconcile_AForgedPointerDoesNotHideAnOrphan(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	ext := newFakeStore(t, f)
	s := storeMode(t, pool, ext, nil)
	ctx := t.Context()
	if err := s.For("alice").Put(ctx, "pat", []byte("v")); err != nil {
		t.Fatal(err)
	}
	bobRef, err := ext.Put(ctx, "bob", "orphan", "", []byte("v"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE secrets SET kek_id=$1 WHERE owned_by='alice' AND name='pat'`, Name+":"+bobRef); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Dangling) != 1 || !strings.Contains(rep.Dangling[0], `owned_by="alice"`) {
		t.Fatalf("dangling = %v; want alice's forged row", rep.Dangling)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0].Owner != "bob" {
		t.Fatalf("orphans = %+v; want bob's value, which no row derives", rep.Orphans)
	}
}

func TestReconcile_ReportsBothSidesAndDeletesNothing(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	ext := newFakeStore(t, f)
	s := storeMode(t, pool, ext, nil)
	ctx := t.Context()
	for _, n := range []string{"kept", "dangling"} {
		if err := s.Put(ctx, n, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ext.Put(ctx, "bob", "orphan", "", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	// Soft-deleted at Vault: the metadata is still there, the current version
	// holds no value.
	if _, err := ext.c.call(ctx, http.MethodDelete, "wardyn/data/ns1/operator/dangling", nil, nil); err != nil {
		t.Fatal(err)
	}
	deletesBefore := f.callCount("DELETE")
	rep, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 2 || len(rep.Dangling) != 1 || !strings.Contains(rep.Dangling[0], "dangling") {
		t.Fatalf("report = %+v; want 2 checked, the one dangling row", rep)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0].Owner != "bob" || rep.Orphans[0].Name != "orphan" {
		t.Fatalf("orphans = %+v; want bob's orphan", rep.Orphans)
	}
	deletes := f.callCount("DELETE") - deletesBefore
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.kv["ns1/operator/kept"]; !ok || deletes != 0 {
		t.Fatal("reconcile deleted something")
	}
}

// -rotate-age-key during a migration: pointer rows hold nothing under the age
// key, so a rotation rewraps the local rows and leaves the pointers alone.
func TestRekey_LeavesPointerRowsAlone(t *testing.T) {
	pool := throwawayDB(t)
	ext := newFakeStore(t, newFakeVault(t))
	ctx := t.Context()
	oldID, _ := age.GenerateX25519Identity()
	local, _ := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: oldID, External: ext})
	if err := local.Put(ctx, "local-row", []byte("l")); err != nil {
		t.Fatal(err)
	}
	if err := storeMode(t, pool, ext, nil).Put(ctx, "pointer-row", []byte("p")); err != nil {
		t.Fatal(err)
	}
	newID, _ := age.GenerateX25519Identity()
	if n, err := secretstorepg.Rekey(ctx, pool, oldID, newID, nil); err != nil || n != 1 {
		t.Fatalf("Rekey = (%d, %v), want 1 local row rewrapped", n, err)
	}
	after, _ := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: newID, External: ext})
	for name, want := range map[string]string{"local-row": "l", "pointer-row": "p"} {
		if v, err := after.Get(ctx, name); err != nil || string(v) != want {
			t.Fatalf("Get %s after the rotation = (%q, %v), want %q", name, v, err, want)
		}
	}
}

// CS-5 in store mode: the erase and the expiry sweep remove the value from
// Vault before the row, so -reconcile finds neither an orphan nor a dangling
// row afterwards; and a sweep that Vault refuses keeps the row (fail closed,
// retried by the next sweep) rather than dropping the pointer.
func TestStoreMode_EraseAndSweepLeaveNothingInVault(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeVault(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	past := secretstore.WithExpiry(ctx, time.Now().Add(-time.Hour))
	for _, w := range []struct {
		ctx         context.Context
		owner, name string
	}{{ctx, "alice", "pat"}, {past, "alice", "sso"}, {past, "bob", "sso"}, {ctx, "bob", "pat"}} {
		if err := s.For(w.owner).Put(w.ctx, w.name, []byte("v-"+w.owner)); err != nil {
			t.Fatal(err)
		}
	}

	if rep, err := secretstore.EraseOwner(ctx, s, "alice"); err != nil || rep.Count != 2 {
		t.Fatalf("EraseOwner(alice) = (%+v, %v), want 2", rep, err)
	}

	f.mu.Lock()
	f.force = []int{403, 403}
	f.mu.Unlock()
	if gone, err := s.DeleteExpired(ctx); err == nil || len(gone) != 0 {
		t.Fatalf("DeleteExpired with Vault refusing = (%v, %v), want an error and nothing deleted", gone, err)
	}
	if v, err := s.For("bob").Get(ctx, "sso"); err != nil || string(v) != "v-bob" {
		t.Fatalf("bob's expired row after a refused sweep = (%q, %v), want it kept intact", v, err)
	}
	gone, err := s.DeleteExpired(ctx)
	if err != nil || len(gone) != 1 || gone[0].Owner != "bob" || gone[0].Name != "sso" {
		t.Fatalf("DeleteExpired = (%+v, %v), want bob's sso", gone, err)
	}

	rep, err := s.Reconcile(ctx)
	if err != nil || rep.Checked != 1 || len(rep.Dangling) != 0 || len(rep.Orphans) != 0 {
		t.Fatalf("reconcile = (%+v, %v), want only bob's pat, nothing dangling or orphaned", rep, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kv) != 1 {
		t.Fatalf("Vault holds %d paths, want only bob's pat", len(f.kv))
	}
}

// deleteBarrier holds a store-mode delete right after its external delete
// succeeds, until release: the window a concurrent Put must not land in.
type deleteBarrier struct {
	secretstore.External
	deleted, resume chan struct{}
	hit, once       sync.Once
}

func (b *deleteBarrier) Delete(ctx context.Context, owner, name, ref string) error {
	if err := b.External.Delete(ctx, owner, name, ref); err != nil {
		return err
	}
	b.hit.Do(func() { close(b.deleted) })
	select {
	case <-b.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *deleteBarrier) release() { b.once.Do(func() { close(b.resume) }) }

// deleteOp removes alice's "review-key" the way op does.
func deleteOp(ctx context.Context, st *secretstorepg.Store, op string) error {
	switch op {
	case "Delete":
		return st.For("alice").Delete(ctx, "review-key")
	case "DeleteEverywhere":
		n, err := st.DeleteEverywhere(ctx, []string{"review-key"})
		if err == nil && n != 1 {
			err = fmt.Errorf("DeleteEverywhere removed %d rows, want 1", n)
		}
		return err
	default:
		rep, err := secretstore.EraseOwner(ctx, st, "alice")
		if rep.Count != 1 {
			err = errors.Join(err, fmt.Errorf("EraseOwner deleted %d rows, want 1", rep.Count))
		}
		return err
	}
}

// waitOnRowLock waits until a session of pool's database is blocked on an
// advisory lock: the row lock a store-mode Put takes.
func waitOnRowLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		var n int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'`,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the concurrent Put never waited on the row's lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// #1057: a delete and a concurrent Put of the same row serialise. The delete
// is held right after its external delete; a Put of the row waits on the
// row's lock until the delete commits, then lands. The end state is one row
// pointing at the replacement, or no row and no value — never a live value
// with no row. EraseOwner's re-list may see the replacement, which it reports.
func TestStoreMode_DeleteRacesPutWithoutOrphan(t *testing.T) {
	for _, op := range []string{"Delete", "DeleteEverywhere", "EraseOwner"} {
		t.Run(op, func(t *testing.T) {
			ctx := t.Context()
			pool := throwawayDB(t)
			ext := newFakeStore(t, newFakeVault(t))
			barrier := &deleteBarrier{External: ext, deleted: make(chan struct{}), resume: make(chan struct{})}
			t.Cleanup(barrier.release)
			st := storeMode(t, pool, barrier, nil)
			view := st.For("alice")
			if err := view.Put(ctx, "review-key", []byte("before-delete")); err != nil {
				t.Fatal(err)
			}
			ref, err := ext.Ref("alice", "review-key", "")
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- deleteOp(ctx, st, op) }()
			select {
			case <-barrier.deleted:
			case <-time.After(5 * time.Second):
				t.Fatal("the external delete did not reach the barrier")
			}
			putDone := make(chan error, 1)
			go func() { putDone <- view.Put(ctx, "review-key", []byte("concurrent-replacement")) }()
			waitOnRowLock(t, pool)
			barrier.release()

			select {
			case err := <-done:
				if err != nil && (op != "EraseOwner" || !strings.Contains(err.Error(), "written while the erase ran")) {
					t.Fatalf("%s: %v", op, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not finish", op)
			}
			select {
			case err := <-putDone:
				if err != nil {
					t.Fatalf("the concurrent Put, once the delete committed: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the concurrent Put did not finish")
			}

			names, err := view.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := ext.Walk(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch len(names) {
			case 0:
				if len(entries) != 0 {
					t.Fatalf("no row, but Vault holds %d values: %+v", len(entries), entries)
				}
				if err := ext.Check(ctx, "alice", "review-key", ref); err == nil {
					t.Fatal("no row, but the value is live in Vault")
				}
			case 1:
				if v, err := view.Get(ctx, "review-key"); err != nil || string(v) != "concurrent-replacement" {
					t.Fatalf("Get = (%q, %v), want the concurrent replacement", v, err)
				}
			default:
				t.Fatalf("rows = %v", names)
			}
		})
	}
}

// putBarrier holds a store-mode Put of "second" after its external write,
// while it holds the row's advisory lock and has not yet touched the row.
type putBarrier struct {
	secretstore.External
	entered, resume chan struct{}
	hit, once       sync.Once
}

func (b *putBarrier) Put(ctx context.Context, owner, name, prev string, value []byte, createOnly bool) (string, error) {
	loc, err := b.External.Put(ctx, owner, name, prev, value, createOnly)
	if string(value) == "second" {
		b.hit.Do(func() { close(b.entered) })
		select {
		case <-b.resume:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return loc, err
}

func (b *putBarrier) release() { b.once.Do(func() { close(b.resume) }) }

// #1057: DeleteEverywhere waits for a Put already holding the row's advisory
// lock, then deletes what it wrote. Locking the row before its advisory lock
// (a FOR UPDATE listing) would deadlock with the Put's row write.
func TestStoreMode_DeleteEverywhereWaitsForAPutWithoutDeadlock(t *testing.T) {
	pool := throwawayDB(t)
	ctx := t.Context()
	ext := newFakeStore(t, newFakeVault(t))
	b := &putBarrier{External: ext, entered: make(chan struct{}), resume: make(chan struct{})}
	t.Cleanup(b.release)
	st := storeMode(t, pool, b, nil)
	if err := st.For("alice").Put(ctx, "k", []byte("first")); err != nil {
		t.Fatal(err)
	}
	putDone := make(chan error, 1)
	go func() { putDone <- st.For("alice").Put(ctx, "k", []byte("second")) }()
	<-b.entered
	deDone := make(chan error, 1)
	go func() {
		n, err := st.DeleteEverywhere(ctx, []string{"k"})
		if err == nil && n != 1 {
			err = fmt.Errorf("removed %d rows, want 1", n)
		}
		deDone <- err
	}()
	waitOnRowLock(t, pool)
	b.release()
	for what, ch := range map[string]chan error{"Put": putDone, "DeleteEverywhere": deDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("%s: %v", what, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s did not finish", what)
		}
	}
	entries, err := ext.Walk(ctx)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Vault after DeleteEverywhere = (%+v, %v), want empty", entries, err)
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// Store mode end to end: the pg store selected as "vaultkv", its pointer rows
// in a real Postgres, its values in the in-process fake Vault. Guarded by
// WARDYN_TEST_PG; each test gets its own throwaway database.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

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
	if n, err := secretstorepg.Rekey(ctx, pool, oldID, newID); err != nil || n != 1 {
		t.Fatalf("Rekey = (%d, %v), want 1 local row rewrapped", n, err)
	}
	after, _ := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: newID, External: ext})
	for name, want := range map[string]string{"local-row": "l", "pointer-row": "p"} {
		if v, err := after.Get(ctx, name); err != nil || string(v) != want {
			t.Fatalf("Get %s after the rotation = (%q, %v), want %q", name, v, err, want)
		}
	}
}

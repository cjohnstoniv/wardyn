// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

// Store mode end to end: the pg store selected as "azurekv", its pointer rows
// in a real Postgres, its values in the in-process fake Key Vault. Guarded by
// WARDYN_TEST_PG; each test gets its own throwaway database.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
	name := "wardyn_akv_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

// storeMode builds the registered "azurekv" store; id may be nil (no age key).
func storeMode(t *testing.T, pool *pgxpool.Pool, ext secretstore.External, id age.Identity) *secretstorepg.Store {
	t.Helper()
	s, err := secretstore.New(Name, secretstore.Deps{Pool: pool, AgeIdentity: id, External: ext})
	if err != nil {
		t.Fatalf("secretstore.New(azurekv): %v", err)
	}
	return s.(*secretstorepg.Store)
}

func kekID(t *testing.T, pool *pgxpool.Pool, owner, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(), `SELECT kek_id FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAzureKV_Conformance(t *testing.T) {
	pool := throwawayDB(t)
	ext := newFakeStore(t, newFakeKV(t))
	secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store { return storeMode(t, pool, ext, nil) })
}

// The row is a pointer: kek_id "azurekv:<vault-host>/<stem>-g<gen>#<n>", no
// ciphertext; a replace is a new version of the same name, counted.
func TestStoreMode_RowIsAPointerAndAReplaceIsAVersion(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	ext := newFakeStore(t, f)
	s := storeMode(t, pool, ext, nil)
	ctx := t.Context()
	for _, v := range []string{"one", "two", "three"} {
		if err := s.For("alice").Put(ctx, "pat", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	var ver int16
	var wrapped, ct []byte
	if err := pool.QueryRow(ctx, `SELECT enc_version, wrapped_dek, ciphertext FROM secrets WHERE owned_by='alice' AND name='pat'`).Scan(&ver, &wrapped, &ct); err != nil {
		t.Fatal(err)
	}
	id := kekID(t, pool, "alice", "pat")
	want := regexp.MustCompile(`^azurekv:` + regexp.QuoteMeta(ext.c.host) + `/ns1-people-[0-9a-f]{32}-g[0-9a-z]+#3$`)
	if ver != 2 || !want.MatchString(id) || len(wrapped) != 0 || len(ct) != 0 {
		t.Fatalf("row = (v%d, %q, %d wrapped, %d ct); want a v2 pointer at count 3", ver, id, len(wrapped), len(ct))
	}
	sn, _, _, _ := ext.parse("alice", "pat", strings.TrimPrefix(id, "azurekv:"))
	if enabled, total := f.liveVersions(sn); enabled != 1 || total != 3 {
		t.Fatalf("versions: %d enabled of %d; want 1 of 3", enabled, total)
	}
	if v, err := s.For("alice").Get(ctx, "pat"); err != nil || string(v) != "three" {
		t.Fatalf("Get = (%q, %v)", v, err)
	}
	if s.Name() != Name || !strings.HasPrefix(s.StoresExternally(), "Key Vault ") {
		t.Fatalf("Name/StoresExternally = %q/%q", s.Name(), s.StoresExternally())
	}
}

// The generation rollover: once the row moves to a new name, the old
// generation is deleted and purged.
func TestStoreMode_RolloverRemovesTheOldGeneration(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	ext := newFakeStore(t, f, func(c *Config) { c.MaxVersions = 2 })
	s := storeMode(t, pool, ext, nil)
	ctx := t.Context()
	for _, v := range []string{"a", "b"} {
		if err := s.Put(ctx, "k", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	old := kekID(t, pool, "", "k")
	if err := s.Put(ctx, "k", []byte("c")); err != nil {
		t.Fatal(err)
	}
	now := kekID(t, pool, "", "k")
	if secretstore.RefObject(now) == secretstore.RefObject(old) || !strings.HasSuffix(now, "#1") {
		t.Fatalf("kek_id %q after %q; want a new generation", now, old)
	}
	oldName, _, _, _ := ext.parse("", "k", strings.TrimPrefix(old, "azurekv:"))
	f.mu.Lock()
	_, live := f.secrets[oldName]
	_, soft := f.deleted[oldName]
	n := len(f.secrets)
	f.mu.Unlock()
	if live || soft || n != 1 {
		t.Fatalf("old generation live %v, soft-deleted %v, %d names; want it purged and one name left", live, soft, n)
	}
	if v, err := s.Get(ctx, "k"); err != nil || string(v) != "c" {
		t.Fatalf("Get = (%q, %v)", v, err)
	}
}

// Two Puts for one row from two replicas are serialised on the row's lock:
// the second reaches Key Vault only after the first has written its row, so
// their writes and disables never interleave.
func TestStoreMode_PutsForOneRowAreSerialised(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	ext := newFakeStore(t, f)
	a, b := storeMode(t, pool, ext, nil), storeMode(t, pool, ext, nil)
	ctx := t.Context()
	if err := a.Put(ctx, "k", []byte("v0")); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	var puts atomic.Int32
	f.mu.Lock()
	f.after = func(r *http.Request) {
		if r.Method == http.MethodPut && puts.Add(1) == 1 {
			close(held) // A's version is written; A has not seen the answer
			<-release
		}
	}
	f.mu.Unlock()
	errs := make(chan error, 2)
	go func() { errs <- a.Put(ctx, "k", []byte("a")) }()
	<-held
	calls := f.count("")
	go func() { errs <- b.Put(ctx, "k", []byte("b")) }()
	// B is waiting on the row's lock, not writing.
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objsubid=2 AND classid::int8=$1 AND NOT granted
			AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`, int64(db.SecretRowLockClass)).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("B never waited on the row lock (%d vault calls made while A was held)", f.count("")-calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.count("") - calls; n != 0 {
		t.Fatalf("B made %d vault calls while A held the row", n)
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if v, err := b.Get(ctx, "k"); err != nil || string(v) != "b" {
		t.Fatalf("Get = (%q, %v); want the later Put's value", v, err)
	}
	sn, _, _, _ := ext.parse("", "k", strings.TrimPrefix(kekID(t, pool, "", "k"), "azurekv:"))
	if enabled, total := f.liveVersions(sn); enabled != 1 || total != 3 {
		t.Fatalf("versions: %d enabled of %d; want 1 of 3", enabled, total)
	}
}

// Rule 18 with versions: a row that cannot be written removes a NEW name's
// value, but never the name the row already points to (that would delete the
// credential the row still reads).
func TestStoreMode_RowFailureNeverDeletesTheRowsOwnName(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	if err := s.Put(ctx, "boom", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION refuse_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'row refused'; END $$;
		CREATE TRIGGER refuse_boom BEFORE INSERT OR UPDATE ON secrets FOR EACH ROW WHEN (NEW.name LIKE 'boom%') EXECUTE FUNCTION refuse_boom();`); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "boom", []byte("second")); !errors.Is(err, secretstore.ErrRowNotWritten) || !strings.Contains(err.Error(), "is live") || strings.Contains(err.Error(), "removed again") {
		t.Fatalf("Put on an existing row with the row refused = %v; want ErrRowNotWritten saying the value is live, removing nothing", err)
	}
	if v, err := s.Get(ctx, "boom"); err != nil || string(v) != "second" {
		t.Fatalf("Get after the failed row update = (%q, %v); want the value the name now holds", v, err)
	}
	if err := s.Put(ctx, "boom-new", []byte("v")); !errors.Is(err, secretstore.ErrRowNotWritten) || !strings.Contains(err.Error(), "removed again") {
		t.Fatalf("Put of a new row with the row refused = %v; want ErrRowNotWritten, the value removed again", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.secrets) != 1 || len(f.deleted) != 0 {
		t.Fatalf("%d live, %d deleted names; want only boom's", len(f.secrets), len(f.deleted))
	}
}

// Rule 17: a row whose value is gone at the vault is a refusal, never
// ErrNotFound, so loadOrCreateSecret can never mint a boot key over it.
func TestStoreMode_ValueGoneBehindARowIsNotNotFound(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	if err := s.Put(ctx, "wardyn-signing-key", []byte("pem")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.secrets = map[string]*kvSecret{}
	f.mu.Unlock()
	_, err := s.Get(ctx, "wardyn-signing-key")
	if err == nil || errors.Is(err, secretstore.ErrNotFound) || !strings.Contains(err.Error(), "wardyn-signing-key") {
		t.Fatalf("Get with the value gone = %v; want a refusal naming the row that is NOT ErrNotFound", err)
	}
}

// Rule 16 through the database: Alice's row pointed at Bob's value refuses,
// naming Alice's row.
func TestStoreMode_PointerMovedToAnotherOwnerIsRefused(t *testing.T) {
	pool := throwawayDB(t)
	s := storeMode(t, pool, newFakeStore(t, newFakeKV(t)), nil)
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
	if err == nil || string(v) == "bob-secret" || !strings.Contains(err.Error(), `owned_by="alice"`) {
		t.Fatalf("alice's moved pointer read (%q, %v); want a refusal naming her row", v, err)
	}
	if err := s.For("alice").Delete(ctx, "pat"); err == nil {
		t.Fatal("Delete followed a moved pointer")
	}
	if v, err := s.For("bob").Get(ctx, "pat"); err != nil || string(v) != "bob-secret" {
		t.Fatalf("bob's value after alice's refused Delete = (%q, %v)", v, err)
	}
}

// The delete report reaches the caller through the pg store.
func TestStoreMode_DeleteReportsWhatTheVaultKept(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	f.purgeForbidden = true
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	if err := s.Put(t.Context(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	ctx, rep := secretstore.WithDeleteReport(t.Context())
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if *rep != (secretstore.DeleteReport{Store: Name, Purged: false, RecoverableDays: 90}) {
		t.Fatalf("report = %+v", *rep)
	}
	if _, err := s.Get(t.Context(), "k"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get after Delete = %v; want ErrNotFound (the row is gone)", err)
	}
}

// DeleteEverywhere removes every owner's value from the store, not only the
// rows, and a store failure keeps every row.
func TestStoreMode_DeleteEverywhereRemovesEveryOwnersValue(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	for _, owner := range []string{"", "alice", "bob"} {
		if err := s.For(owner).Put(ctx, "k", []byte("v-"+owner)); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	f.force = []int{403}
	f.mu.Unlock()
	if _, err := s.DeleteEverywhere(ctx, []string{"k"}); err == nil {
		t.Fatal("DeleteEverywhere succeeded with Key Vault refusing")
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
	if len(f.secrets) != 0 {
		t.Fatalf("DeleteEverywhere left %d secrets in Key Vault", len(f.secrets))
	}
}

// The half-way shape: one owner's external delete goes through before a
// second owner's is refused. The call still errors and every row survives —
// including the first owner's, now a dangling pointer — that dangling
// pointer reads as a definitive refusal, never ErrNotFound (rule 17), and a
// retry once the fault clears finishes the job.
func TestStoreMode_DeleteEverywhereHalfway(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	ext := newFakeStore(t, f)
	s := storeMode(t, pool, ext, nil)
	ctx := t.Context()
	for _, owner := range []string{"alice", "bob"} {
		if err := s.For(owner).Put(ctx, "k", []byte("v-"+owner)); err != nil {
			t.Fatal(err)
		}
	}

	// Both secret names, computed up front while both rows are guaranteed to
	// still exist: deleteExternalEverywhere's SELECT has no ORDER BY, so which
	// owner survives is not fixed, and reading a row's kek_id after a mutation
	// has already removed it would fail here instead of at the assertion below.
	snAlice, _, _, _ := ext.parse("alice", "k", strings.TrimPrefix(kekID(t, pool, "alice", "k"), "azurekv:"))
	snBob, _, _, _ := ext.parse("bob", "k", strings.TrimPrefix(kekID(t, pool, "bob", "k"), "azurekv:"))

	// Key Vault refuses every call naming bob's secret. Per the ORDER BY note
	// above, this produces the half-way shape (one owner through, one
	// refused) only if Postgres visits alice's row before bob's — true in
	// practice for a freshly inserted 2-row match, though not guaranteed; the
	// assertions below don't assume WHICH owner that is, only that exactly
	// one of them is.
	f.mu.Lock()
	f.denyPath = ext.stem("bob", "k")
	f.mu.Unlock()

	if _, err := s.DeleteEverywhere(ctx, []string{"k"}); err == nil {
		t.Fatal("DeleteEverywhere succeeded with bob's delete refused")
	}
	f.mu.Lock()
	_, aliceLive := f.secrets[snAlice]
	_, bobLive := f.secrets[snBob]
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

	// The fault only ever named bob's delete; clear it before reading back, so
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
	if len(f.secrets) != 0 {
		t.Fatalf("DeleteEverywhere left %d secrets in Key Vault", len(f.secrets))
	}
}

// Rule 22: migrate both ways, idempotent, nothing left behind (not even a
// soft-deleted secret).
func TestMigrate_BothWaysLeavesNothingBehind(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
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
			if got, err := local.For(r[0]).Get(ctx, r[1]); err != nil || string(got) != v {
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
		t.Fatalf("Migrate to azurekv = (%d, %v), %d reads; want 3 moved, 3 reads", res.Moved, err, reads)
	}
	check(2)
	if res, err := lps.Migrate(ctx, Name, count); err != nil || res.Moved != 0 {
		t.Fatalf("re-run = (%d, %v), want (0, nil)", res.Moved, err)
	}
	if res, err := lps.Migrate(ctx, secretstorepg.MigrateLocal, count); err != nil || res.Moved != 3 || res.SoftDeleted != 0 {
		t.Fatalf("Migrate to local = (%+v, %v), want 3 moved, none left soft-deleted", res, err)
	}
	check(1)
	f.mu.Lock()
	live, soft := len(f.secrets), len(f.deleted)
	f.mu.Unlock()
	if live+soft != 0 {
		t.Fatalf("Key Vault still holds %d live and %d deleted secrets after moving back", live, soft)
	}
}

// On a vault that withholds purge, a migration back to local leaves each old
// copy soft-deleted: the result counts them, and -reconcile lists them.
func TestMigrate_ToLocalWithPurgeWithheldReportsWhatItLeft(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	f.purgeForbidden = true
	ext := newFakeStore(t, f)
	id, _ := age.GenerateX25519Identity()
	ctx := t.Context()
	local, err := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: id, External: ext})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if err := local.Put(ctx, n, []byte("v-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	lps := local.(*secretstorepg.Store)
	if res, err := lps.Migrate(ctx, Name, func(string, string) {}); err != nil || res.Moved != 2 {
		t.Fatalf("Migrate to azurekv = (%+v, %v)", res, err)
	}
	res, err := lps.Migrate(ctx, secretstorepg.MigrateLocal, func(string, string) {})
	if err != nil || res.Moved != 2 || res.SoftDeleted != 2 {
		t.Fatalf("Migrate to local = (%+v, %v); want 2 moved, 2 left soft-deleted", res, err)
	}
	rep, err := lps.Reconcile(ctx)
	if err != nil || len(rep.Dangling)+len(rep.Orphans) != 0 || len(rep.SoftDeleted) != 2 {
		t.Fatalf("Reconcile = (%+v, %v); want the two soft-deleted copies, no drift", rep, err)
	}
	for _, e := range rep.SoftDeleted {
		if e.RecoverableDays != 90 {
			t.Fatalf("soft-deleted %+v; want 90 recoverable days", e)
		}
	}
}

// A row's claim on a value comes from the stem its owner and name derive, so
// a pointer forged to another row's secret hides nothing from the orphan list.
func TestReconcile_AForgedPointerDoesNotHideAnOrphan(t *testing.T) {
	pool := throwawayDB(t)
	ext := newFakeStore(t, newFakeKV(t))
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
	if len(rep.Orphans) != 2 {
		t.Fatalf("orphans = %+v; want bob's value and alice's own, which the forged row no longer names", rep.Orphans)
	}
}

func TestReconcile_ReportsBothSidesAndDeletesNothing(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	ext := newFakeStore(t, f)
	s := storeMode(t, pool, ext, nil)
	ctx := t.Context()
	for _, n := range []string{"kept", "dangling"} {
		if err := s.Put(ctx, n, []byte("v")); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(ctx, n, []byte("w")); err != nil { // two versions: counts differ from Walk's refs
			t.Fatal(err)
		}
	}
	if _, err := ext.Put(ctx, "bob", "orphan", "", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	sn, _, _, _ := ext.parse("", "dangling", strings.TrimPrefix(kekID(t, pool, "", "dangling"), "azurekv:"))
	f.mu.Lock()
	f.secrets[sn].versions[1].enabled = false
	f.mu.Unlock()
	deletes, patches := f.count("DELETE"), f.count("PATCH")
	rep, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 2 || len(rep.Dangling) != 1 || !strings.Contains(rep.Dangling[0], "dangling") {
		t.Fatalf("report = %+v; want 2 checked, the one dangling row", rep)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0].Owner != "bob" || rep.Orphans[0].Name != "orphan" {
		t.Fatalf("orphans = %+v; want bob's orphan only", rep.Orphans)
	}
	if f.count("DELETE") != deletes || f.count("PATCH") != patches {
		t.Fatal("reconcile changed something")
	}
}

// A store-mode write is bounded as a whole, at six times
// WARDYN_SECRET_STORE_TIMEOUT: a stalling vault cannot hold the row's lock and
// a database connection for minutes.
func TestStoreMode_WriteIsBounded(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	st, err := secretstore.New(Name, secretstore.Deps{Pool: pool, External: newFakeStore(t, f), ExternalTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := st.Put(ctx, "k", []byte("one")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.after = func(*http.Request) { time.Sleep(time.Second) } // every vault answer stalls
	f.mu.Unlock()
	start := time.Now()
	err = st.Put(ctx, "k", []byte("two"))
	if took := time.Since(start); err == nil || took > 900*time.Millisecond {
		t.Fatalf("Put against a stalling vault = %v after %v; want a failure within the 300ms budget", err, took)
	}
	f.mu.Lock()
	f.after = nil
	f.mu.Unlock()
}

// CS-5 in Key Vault store mode: the erase says what the vault kept, so the
// erase text can name the recoverable window (§3 ERASE.BODY_AZURE), and the
// expiry sweep removes the value before the row.
func TestStoreMode_EraseAndSweepSayWhatTheVaultKept(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKV(t)
	f.purgeForbidden = true
	s := storeMode(t, pool, newFakeStore(t, f), nil)
	ctx := t.Context()
	past := secretstore.WithExpiry(ctx, time.Now().Add(-time.Hour))
	for _, owner := range []string{"alice", "bob"} {
		if err := s.For(owner).Put(past, "sso", []byte("v-"+owner)); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := secretstore.EraseOwner(ctx, s, "alice")
	if err != nil || rep != (secretstore.EraseReport{Count: 1, Store: Name, Purged: false, RecoverableDays: 90}) {
		t.Fatalf("EraseOwner(alice) = (%+v, %v), want 1 erased, soft-deleted for 90 days", rep, err)
	}
	gone, err := s.DeleteExpired(ctx)
	if err != nil || len(gone) != 1 || gone[0].Owner != "bob" {
		t.Fatalf("DeleteExpired = (%+v, %v), want bob's sso", gone, err)
	}
	rc, err := s.Reconcile(ctx)
	if err != nil || rc.Checked != 0 || len(rc.Dangling) != 0 || len(rc.Orphans) != 0 || len(rc.SoftDeleted) != 2 {
		t.Fatalf("reconcile = (%+v, %v), want no rows, no orphans, and the two soft-deleted values", rc, err)
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
// is held right after its external delete (soft delete and purge); a Put of
// the row waits on the row's lock until the delete commits, then lands. The
// end state is one row pointing at the replacement, or no row and no value —
// never a live value with no row. EraseOwner's re-list may see the
// replacement, which it reports.
func TestStoreMode_DeleteRacesPutWithoutOrphan(t *testing.T) {
	for _, op := range []string{"Delete", "DeleteEverywhere", "EraseOwner"} {
		t.Run(op, func(t *testing.T) {
			ctx := t.Context()
			pool := throwawayDB(t)
			ext := newFakeStore(t, newFakeKV(t))
			barrier := &deleteBarrier{External: ext, deleted: make(chan struct{}), resume: make(chan struct{})}
			t.Cleanup(barrier.release)
			st := storeMode(t, pool, barrier, nil)
			view := st.For("alice")
			if err := view.Put(ctx, "review-key", []byte("before-delete")); err != nil {
				t.Fatal(err)
			}
			ref := strings.TrimPrefix(kekID(t, pool, "alice", "review-key"), Name+":")

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
					t.Fatalf("no row, but Key Vault holds %d values: %+v", len(entries), entries)
				}
				if err := ext.Check(ctx, "alice", "review-key", ref); err == nil {
					t.Fatal("no row, but the value is live in Key Vault")
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

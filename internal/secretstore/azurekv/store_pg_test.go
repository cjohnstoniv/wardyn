// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

// Store mode end to end: the pg store selected as "azurekv", its pointer rows
// in a real Postgres, its values in the in-process fake Key Vault. Guarded by
// WARDYN_TEST_PG; each test gets its own throwaway database.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
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

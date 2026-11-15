// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// End-to-end coverage for `wardynd -rotate-age-key` against a real Postgres: the
// whole maintenance mode (key file, lock, transaction, audit row, file swap),
// not just the secretstore/pg Rekey it calls. Each run gets its own throwaway
// CREATE DATABASE because a rotation rewrites EVERY row of `secrets` and the
// rekey advisory lock is per database — sharing either with a sibling test
// would make both flaky. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// rotateDatabase creates and migrates a fresh database on the WARDYN_TEST_PG
// server and returns its DSN plus a pool, both dropped on cleanup.
func rotateDatabase(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the Postgres-backed -rotate-age-key test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_rotate_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = admin.Exec(cctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(cctx, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to throwaway database %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate throwaway database: %v", err)
	}
	return u.String(), pool
}

// rotateSeed is one operator row and one member-owned row: the audit row must
// name neither, and both must come out readable under the new key only.
var rotateSeed = []struct{ owner, name, value string }{
	{"", "anthropic-api-key", "sk-ant-not-a-real-key-000000000000"},
	{"alice", "github-pat", "ghp_not_a_real_token_0000000000"},
}

// seedRotateStore fills the store the way a running install does: a
// pre-envelope row the boot converts, the boot's own signing key (sealed under
// the platform purpose's key), and rotateSeed's rows (the credential purpose's).
func seedRotateStore(t *testing.T, pool *pgxpool.Pool, id *age.X25519Identity) {
	t.Helper()
	ctx := context.Background()
	seedV0Row(t, pool, id, "legacy-pre-envelope-token", []byte("legacy-not-a-real-value"))
	booted, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatalf("boot the store to seed it: %v", err)
	}
	if _, err := loadOrCreateSigningKey(ctx, unlocked(booted)); err != nil {
		t.Fatalf("seed the boot signing key: %v", err)
	}
	st, err := secretstorepg.New(pool, id)
	if err != nil {
		t.Fatalf("secretstorepg.New: %v", err)
	}
	for _, r := range rotateSeed {
		if err := st.For(r.owner).Put(ctx, r.name, []byte(r.value)); err != nil {
			t.Fatalf("seed %s/%s: %v", r.owner, r.name, err)
		}
	}
}

// storeRow is one row of `secrets` by its key.
type storeRow struct{ owner, name string }

// readEveryRow opens every row of `secrets` under id and returns its value by
// row. A row id cannot open fails the test, so the result is the whole table.
func readEveryRow(t *testing.T, pool *pgxpool.Pool, id age.Identity) map[storeRow]string {
	t.Helper()
	ctx := context.Background()
	st, err := secretstorepg.New(pool, id)
	if err != nil {
		t.Fatalf("secretstorepg.New: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT owned_by, name FROM secrets`)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (storeRow, error) {
		var k storeRow
		err := r.Scan(&k.owner, &k.name)
		return k, err
	})
	if err != nil {
		t.Fatalf("scan secrets: %v", err)
	}
	out := make(map[storeRow]string, len(keys))
	for _, k := range keys {
		v, err := st.For(k.owner).Get(ctx, k.name)
		if err != nil {
			t.Errorf("%s/%s does not decrypt under the expected identity: %v", k.owner, k.name, err)
			continue
		}
		out[k] = string(v)
	}
	return out
}

// assertEveryRowReadsUnder fails unless the table holds exactly the rows of
// before, each opening under want to the value it held before, and none
// opening under reject.
func assertEveryRowReadsUnder(t *testing.T, pool *pgxpool.Pool, before map[storeRow]string, want, reject age.Identity) {
	t.Helper()
	after := readEveryRow(t, pool, want)
	if len(after) != len(before) {
		t.Errorf("%d rows open under the expected identity, want all %d", len(after), len(before))
	}
	for k, v := range before {
		if got, ok := after[k]; ok && got != v {
			t.Errorf("%s/%s changed value across the rotation", k.owner, k.name)
		}
	}
	bad, err := secretstorepg.New(pool, reject)
	if err != nil {
		t.Fatalf("secretstorepg.New(reject): %v", err)
	}
	for k := range before {
		if _, err := bad.For(k.owner).Get(context.Background(), k.name); err == nil {
			t.Errorf("%s/%s still decrypts under the identity it must no longer answer to", k.owner, k.name)
		}
	}
}

func rekeyAuditRows(t *testing.T, pool *pgxpool.Pool) []struct{ actorType, actor, target, outcome, data string } {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT actor_type, actor, target, outcome, data::text FROM audit_events WHERE action='secret.rekey'`)
	if err != nil {
		t.Fatalf("query secret.rekey rows: %v", err)
	}
	defer rows.Close()
	var out []struct{ actorType, actor, target, outcome, data string }
	for rows.Next() {
		var r struct{ actorType, actor, target, outcome, data string }
		if err := rows.Scan(&r.actorType, &r.actor, &r.target, &r.outcome, &r.data); err != nil {
			t.Fatalf("scan secret.rekey row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate secret.rekey rows: %v", err)
	}
	return out
}

// TestRotateAgeKeyMode_EndToEnd runs the whole maintenance mode against a real
// store: every row decrypts with the new identity only, the key file holds the
// new identity at 0600, `.bak` holds the old one at 0600, nothing staged is left
// behind, and exactly one secret.rekey row carries the count and the new public
// recipient and no secret name.
func TestRotateAgeKeyMode_EndToEnd(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	oldID := newIdentity(t)
	seedRotateStore(t, pool, oldID)
	before := readEveryRow(t, pool, oldID)
	if len(before) != len(rotateSeed)+2 {
		t.Fatalf("seeded %d rows, want the converted row, the signing key and %d more", len(before), len(rotateSeed))
	}

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte("# created by age-keygen\n"+oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the current key file: %v", err)
	}

	if err := rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath); err != nil {
		t.Fatalf("rotateAgeKeyMode: %v", err)
	}

	newKey, err := readAgeKeyFile(keyPath)
	if err != nil {
		t.Fatalf("read the rotated key file: %v", err)
	}
	if newKey == "" || newKey == oldID.String() {
		t.Fatalf("key file still holds the old identity (or none) after the rotation")
	}
	newID, err := age.ParseX25519Identity(newKey)
	if err != nil {
		t.Fatalf("parse the rotated key: %v", err)
	}
	assertEveryRowReadsUnder(t, pool, before, newID, oldID)

	for path, want := range map[string]string{keyPath: newKey, keyPath + ".bak": oldID.String()} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", path, perm)
		}
		got, err := readAgeKeyFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if got != want {
			t.Errorf("%s holds the wrong identity", path)
		}
	}
	for _, leftover := range []string{keyPath + ".new", keyPath + ".new.tmp", keyPath + ".tmp", keyPath + ".bak.tmp"} {
		if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the rotation holding a key copy (stat err = %v)", leftover, err)
		}
	}

	rows := rekeyAuditRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("secret.rekey rows = %d, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.actorType != "system" || r.actor != rekeyActor || r.target != "pg" || r.outcome != "success" {
		t.Errorf("secret.rekey row = (%s, %s, %s, %s), want (system, %s, pg, success)", r.actorType, r.actor, r.target, r.outcome, rekeyActor)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(r.data), &data); err != nil {
		t.Fatalf("decode secret.rekey data %q: %v", r.data, err)
	}
	if len(data) != 2 || data["secrets"] != float64(len(before)) || data["public_recipient"] != newID.Recipient().String() {
		t.Errorf("secret.rekey data = %s, want exactly {secrets: %d, public_recipient: the new recipient}", r.data, len(before))
	}
	for k := range before {
		if strings.Contains(r.data, k.name) || strings.Contains(r.target, k.name) {
			t.Errorf("secret.rekey row names the secret %q; it must carry the count only", k.name)
		}
	}
}

// TestRotateAgeKeyMode_RefusesWhileTheRekeyLockIsHeld: a second rotation on the
// same database is refused while db.SecretRekeyLockKey is held elsewhere, and
// the refusal touches nothing — the store still answers to the old key, the key
// file is unchanged, nothing is staged and no audit row is written.
func TestRotateAgeKeyMode_RefusesWhileTheRekeyLockIsHeld(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	oldID := newIdentity(t)
	seedRotateStore(t, pool, oldID)
	before := readEveryRow(t, pool, oldID)

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the current key file: %v", err)
	}

	_, release, ok, err := db.TryAdvisoryLockDedicated(context.Background(), pool, db.SecretRekeyLockKey)
	if err != nil || !ok {
		t.Fatalf("hold the rekey lock: ok=%v err=%v", ok, err)
	}
	defer release()

	err = rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath)
	if err == nil {
		t.Fatal("rotateAgeKeyMode rotated while another holder had the rekey lock")
	}
	if !strings.Contains(err.Error(), "holds the rekey advisory lock") {
		t.Errorf("refusal %q does not name the rekey lock", err)
	}

	got, rerr := readAgeKeyFile(keyPath)
	if rerr != nil || got != oldID.String() {
		t.Errorf("key file changed by a refused rotation: %q, %v", got, rerr)
	}
	for _, leftover := range []string{keyPath + ".new", keyPath + ".bak"} {
		if _, serr := os.Stat(leftover); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("a refused rotation left %s behind (stat err = %v)", leftover, serr)
		}
	}
	assertEveryRowReadsUnder(t, pool, before, oldID, newIdentity(t))
	if rows := rekeyAuditRows(t, pool); len(rows) != 0 {
		t.Errorf("a refused rotation wrote %d secret.rekey rows, want 0", len(rows))
	}
}

// TestRotateAgeKeyMode_RefusesAStagedKeyLeftAfterTheFirstCheck: a staged key
// that appears after the first check passed (a racing run's) is caught by the
// check under the rekey lock, which refuses before the file is overwritten or
// the store is touched.
func TestRotateAgeKeyMode_RefusesAStagedKeyLeftAfterTheFirstCheck(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	oldID := newIdentity(t)
	seedRotateStore(t, pool, oldID)
	before := readEveryRow(t, pool, oldID)

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the current key file: %v", err)
	}
	staged := keyPath + ".new"
	stagedKey := newIdentity(t).String() + "\n"
	prev := beforeLockedStagedKeyCheck
	t.Cleanup(func() { beforeLockedStagedKeyCheck = prev })
	beforeLockedStagedKeyCheck = func() {
		if err := os.WriteFile(staged, []byte(stagedKey), 0o600); err != nil {
			t.Errorf("plant the staged key: %v", err)
		}
	}

	err := rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath)
	if err == nil || !strings.Contains(err.Error(), "ONLY key") || !strings.Contains(err.Error(), staged) {
		t.Fatalf("err = %v, want the staged-key refusal naming %s", err, staged)
	}
	if b, rerr := os.ReadFile(staged); rerr != nil || string(b) != stagedKey {
		t.Errorf("the staged key was changed or removed: %q, %v", b, rerr)
	}
	if got, rerr := readAgeKeyFile(keyPath); rerr != nil || got != oldID.String() {
		t.Errorf("key file changed by a refused rotation: %q, %v", got, rerr)
	}
	assertEveryRowReadsUnder(t, pool, before, oldID, newIdentity(t))
	if rows := rekeyAuditRows(t, pool); len(rows) != 0 {
		t.Errorf("a refused rotation wrote %d secret.rekey rows, want 0", len(rows))
	}
}

// ageKeyFromFile resolves WARDYN_AGE_KEY through its WARDYN_AGE_KEY_FILE twin
// the way boot does, from the one list of _FILE settings.
func ageKeyFromFile(t *testing.T, f *bootFlags) {
	t.Helper()
	for _, s := range secretFileSettings(f) {
		if s.fileVar == "WARDYN_AGE_KEY_FILE" {
			if err := resolveSecretFiles([]secretFileSetting{s}); err != nil {
				t.Fatalf("resolve WARDYN_AGE_KEY_FILE: %v", err)
			}
			return
		}
	}
	t.Fatal("WARDYN_AGE_KEY_FILE is not a _FILE setting")
}

// TestRotateAgeKeyMode_FromAMountedKeyFile is the docs/operations/secrets-and-keys.md runbook for a
// file-mounted key: `WARDYN_AGE_KEY_FILE=/tmp/age.key wardynd -rotate-age-key
// /tmp/age.key`. The old key comes in through the _FILE twin, the rotation
// replaces that same file, and the next boot's _FILE read hands back the new
// key: every row opens under it, the old key opens none, and a boot on it
// reads the signing key it had before.
func TestRotateAgeKeyMode_FromAMountedKeyFile(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	oldID := newIdentity(t)
	seedRotateStore(t, pool, oldID)
	before := readEveryRow(t, pool, oldID)

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the mounted key copy: %v", err)
	}
	t.Setenv("WARDYN_AGE_KEY_FILE", keyPath)

	f := rekeyFlags(dsn, "pg", "")
	ageKeyFromFile(t, f)
	if *f.ageKey != oldID.String() {
		t.Fatal("WARDYN_AGE_KEY_FILE did not resolve to the current key")
	}
	if err := rotateAgeKeyMode(f, keyPath); err != nil {
		t.Fatalf("rotateAgeKeyMode: %v", err)
	}

	next := rekeyFlags(dsn, "pg", "")
	ageKeyFromFile(t, next)
	newID, err := age.ParseX25519Identity(*next.ageKey)
	if err != nil {
		t.Fatalf("the rotated key file does not resolve to an age identity through WARDYN_AGE_KEY_FILE: %v", err)
	}
	if newID.String() == oldID.String() {
		t.Fatal("WARDYN_AGE_KEY_FILE still resolves to the old key after the rotation")
	}
	assertEveryRowReadsUnder(t, pool, before, newID, oldID)

	booted, err := buildSecretStore(context.Background(), pool, *next.ageKey, nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatalf("boot on the rotated key: %v", err)
	}
	raw, err := booted.Get(secretstore.WithPurpose(context.Background(), secretstore.PurposeBoot), secretSigningKey)
	if err != nil || string(raw) != before[storeRow{"", secretSigningKey}] {
		t.Fatalf("the boot on the rotated key read the signing key as (%d bytes, %v), want the key it had before", len(raw), err)
	}
}

// TestRotateAgeKeyMode_KeepsTheStagedKeyWhenTheCommitReplyIsLost: a COMMIT that
// lands but whose reply is lost is an error to the rotation while the store
// already answers to the new key alone. The staged key must survive and open
// every row, the key file must be untouched, and the error must name the file
// and the kek_id ending whose rows prove the rotation landed.
func TestRotateAgeKeyMode_KeepsTheStagedKeyWhenTheCommitReplyIsLost(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	oldID := newIdentity(t)
	seedRotateStore(t, pool, oldID)
	before := readEveryRow(t, pool, oldID)

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the current key file: %v", err)
	}
	lost := errors.New("injected: the commit's reply was lost")
	prev := rekeyStore
	t.Cleanup(func() { rekeyStore = prev })
	rekeyStore = func(ctx context.Context, p *pgxpool.Pool, from, to, platform age.Identity) (int, error) {
		if _, err := prev(ctx, p, from, to, platform); err != nil {
			t.Errorf("the real rekey failed before the injected error: %v", err)
		}
		return 0, lost
	}

	err := rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath)
	if !errors.Is(err, lost) {
		t.Fatalf("err = %v, want the injected commit error", err)
	}
	staged := keyPath + ".new"
	stagedKey, rerr := readAgeKeyFile(staged)
	if rerr != nil || stagedKey == "" {
		t.Fatalf("the staged key did not survive the commit error (%q, %v); it is the only key that reads the store", stagedKey, rerr)
	}
	newID, perr := age.ParseX25519Identity(stagedKey)
	if perr != nil {
		t.Fatalf("parse the staged key: %v", perr)
	}
	assertEveryRowReadsUnder(t, pool, before, newID, oldID)
	if got, gerr := readAgeKeyFile(keyPath); gerr != nil || got != oldID.String() {
		t.Errorf("the key file changed although the rotation failed: %q, %v", got, gerr)
	}
	suffix := ":" + kekFingerprint(newID)
	for _, want := range []string{staged, "ONLY key", suffix} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if moved := rowsUnderKeySuffix(t, pool, suffix); moved == 0 {
		t.Errorf("no row's kek_id ends in %q; the check the error names would miss the committed rotation", suffix)
	}
}

// rowsUnderKeySuffix counts the secrets and principal_keys rows whose kek_id
// ends in suffix: the check the rotation's error and the runbook name.
func rowsUnderKeySuffix(t *testing.T, pool *pgxpool.Pool, suffix string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM secrets WHERE kek_id LIKE '%' || $1::text) + (SELECT count(*) FROM principal_keys WHERE kek_id LIKE '%' || $1::text)`, suffix,
	).Scan(&n); err != nil {
		t.Fatalf("count the rows under %q: %v", suffix, err)
	}
	return n
}

// TestRotateAgeKeyMode_KeepsTheStagedKeyWhenTheCommitLandsAfterTheError: a
// COMMIT error does not say whether the commit will land, so this pin lands it
// after the error returned, and a look at the rows in between finds none under
// the new key. The staged key must survive that look and a refused re-run, and
// neither message may say to discard it: once the commit lands it is the only
// key that opens the store.
func TestRotateAgeKeyMode_KeepsTheStagedKeyWhenTheCommitLandsAfterTheError(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	oldID := newIdentity(t)
	seedRotateStore(t, pool, oldID)
	before := readEveryRow(t, pool, oldID)

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the current key file: %v", err)
	}
	lost := errors.New("injected: the commit's reply was lost")
	var inFlight age.Identity
	prev := rekeyStore
	t.Cleanup(func() { rekeyStore = prev })
	rekeyStore = func(_ context.Context, _ *pgxpool.Pool, _, to, _ age.Identity) (int, error) {
		inFlight = to // the rewrap has not landed when the error returns
		return 0, lost
	}

	err := rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath)
	if !errors.Is(err, lost) {
		t.Fatalf("err = %v, want the injected commit error", err)
	}
	staged := keyPath + ".new"
	stagedKey, rerr := readAgeKeyFile(staged)
	if rerr != nil || stagedKey == "" {
		t.Fatalf("the staged key did not survive the commit error (%q, %v)", stagedKey, rerr)
	}
	newID, perr := age.ParseX25519Identity(stagedKey)
	if perr != nil {
		t.Fatalf("parse the staged key: %v", perr)
	}
	suffix := ":" + kekFingerprint(newID)
	if n := rowsUnderKeySuffix(t, pool, suffix); n != 0 {
		t.Fatalf("%d rows are under the new key before its commit landed, want 0", n)
	}
	rerun := rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath)
	if rerun == nil {
		t.Fatalf("a re-run beside the staged key was not refused")
	}
	for _, e := range []error{err, rerun} {
		msg := strings.ToLower(e.Error())
		if !strings.Contains(msg, "move it aside") || !strings.Contains(msg, "0600") || strings.Contains(msg, "remove") || strings.Contains(msg, "delete") {
			t.Errorf("error %q must say to move the staged key aside at mode 0600, never to remove or delete it", e)
		}
	}
	if got, gerr := readAgeKeyFile(staged); gerr != nil || got != stagedKey {
		t.Fatalf("the staged key did not survive a zero-row look and a refused re-run (%q, %v)", got, gerr)
	}

	if _, cerr := prev(context.Background(), pool, oldID, inFlight, nil); cerr != nil {
		t.Fatalf("land the delayed commit: %v", cerr)
	}
	assertEveryRowReadsUnder(t, pool, before, newID, oldID)
	if rowsUnderKeySuffix(t, pool, suffix) == 0 {
		t.Errorf("no row's kek_id ends in %q after the delayed commit landed", suffix)
	}
}

// TestRotateAgeKeyMode_FindsALandedRotationThroughPrincipalKeysAlone: a store
// whose only rows under the age key are principal keys (the default domain, no
// row of secrets) answers the check after a COMMIT whose reply was lost through
// principal_keys alone. The staged key must survive and open the principal key
// the old key no longer opens, and its fingerprint must find that row.
func TestRotateAgeKeyMode_FindsALandedRotationThroughPrincipalKeysAlone(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	ctx := context.Background()
	oldID := newIdentity(t)
	oldStore, err := secretstorepg.New(pool, oldID)
	if err != nil {
		t.Fatalf("secretstorepg.New: %v", err)
	}
	v, key, err := oldStore.SubjectKeys().Current(ctx, "alice", subjectkey.PurposeCred)
	if err != nil {
		t.Fatalf("create alice's principal key in the default domain: %v", err)
	}
	secretRows := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets`).Scan(&n); err != nil {
			t.Fatalf("count secrets: %v", err)
		}
		return n
	}
	if n := secretRows(); n != 0 {
		t.Fatalf("%d rows of secrets, want none: only principal_keys may answer the check", n)
	}

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the current key file: %v", err)
	}
	lost := errors.New("injected: the commit's reply was lost")
	prev := rekeyStore
	t.Cleanup(func() { rekeyStore = prev })
	rekeyStore = func(ctx context.Context, p *pgxpool.Pool, from, to, platform age.Identity) (int, error) {
		if _, err := prev(ctx, p, from, to, platform); err != nil {
			t.Errorf("the real rekey failed before the injected error: %v", err)
		}
		return 0, lost
	}

	err = rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath)
	if !errors.Is(err, lost) {
		t.Fatalf("err = %v, want the injected commit error", err)
	}
	stagedKey, rerr := readAgeKeyFile(keyPath + ".new")
	if rerr != nil || stagedKey == "" {
		t.Fatalf("the staged key did not survive the commit error (%q, %v); it is the only key that opens the principal key", stagedKey, rerr)
	}
	newID, perr := age.ParseX25519Identity(stagedKey)
	if perr != nil {
		t.Fatalf("parse the staged key: %v", perr)
	}
	suffix := ":" + kekFingerprint(newID)
	if !strings.Contains(err.Error(), suffix) {
		t.Errorf("error %q does not name %q", err, suffix)
	}
	if n := secretRows(); n != 0 {
		t.Fatalf("%d rows of secrets after the rotation, want none", n)
	}
	if n := rowsUnderKeySuffix(t, pool, suffix); n != 1 {
		t.Errorf("%d rows under %q, want the one principal key: the check must count principal_keys", n, suffix)
	}
	newStore, err := secretstorepg.New(pool, newID)
	if err != nil {
		t.Fatalf("secretstorepg.New(new): %v", err)
	}
	if got, err := newStore.SubjectKeys().Key(ctx, "alice", subjectkey.PurposeCred, v); err != nil || !bytes.Equal(got, key) {
		t.Errorf("the staged key opens alice's principal key as (%d bytes, %v), want the key it held before", len(got), err)
	}
	cold, err := secretstorepg.New(pool, oldID)
	if err != nil {
		t.Fatalf("secretstorepg.New(old): %v", err)
	}
	if _, err := cold.SubjectKeys().Key(ctx, "alice", subjectkey.PurposeCred, v); err == nil {
		t.Errorf("the old key still opens alice's principal key; the rotation did not move it")
	}
}

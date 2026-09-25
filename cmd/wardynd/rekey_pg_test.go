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
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
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

func seedRotateStore(t *testing.T, pool *pgxpool.Pool, id age.Identity) {
	t.Helper()
	st, err := secretstorepg.New(pool, id)
	if err != nil {
		t.Fatalf("secretstorepg.New: %v", err)
	}
	for _, r := range rotateSeed {
		if err := st.For(r.owner).Put(context.Background(), r.name, []byte(r.value)); err != nil {
			t.Fatalf("seed %s/%s: %v", r.owner, r.name, err)
		}
	}
}

// assertStoreReadsUnder fails unless every seeded row reads back verbatim under
// want and under neither of the identities in reject.
func assertStoreReadsUnder(t *testing.T, pool *pgxpool.Pool, want age.Identity, reject age.Identity) {
	t.Helper()
	ctx := context.Background()
	good, err := secretstorepg.New(pool, want)
	if err != nil {
		t.Fatalf("secretstorepg.New(want): %v", err)
	}
	bad, err := secretstorepg.New(pool, reject)
	if err != nil {
		t.Fatalf("secretstorepg.New(reject): %v", err)
	}
	for _, r := range rotateSeed {
		got, gerr := good.For(r.owner).Get(ctx, r.name)
		if gerr != nil {
			t.Errorf("%s/%s does not decrypt under the expected identity: %v", r.owner, r.name, gerr)
		} else if string(got) != r.value {
			t.Errorf("%s/%s = %q, want %q", r.owner, r.name, got, r.value)
		}
		if _, berr := bad.For(r.owner).Get(ctx, r.name); berr == nil {
			t.Errorf("%s/%s still decrypts under the identity it must no longer answer to", r.owner, r.name)
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
	assertStoreReadsUnder(t, pool, newID, oldID)

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
	if len(data) != 2 || data["secrets"] != float64(len(rotateSeed)) || data["public_recipient"] != newID.Recipient().String() {
		t.Errorf("secret.rekey data = %s, want exactly {secrets: %d, public_recipient: the new recipient}", r.data, len(rotateSeed))
	}
	for _, s := range rotateSeed {
		if strings.Contains(r.data, s.name) || strings.Contains(r.target, s.name) {
			t.Errorf("secret.rekey row names the secret %q; it must carry the count only", s.name)
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

	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the current key file: %v", err)
	}

	release, ok, err := db.TryAdvisoryLock(context.Background(), pool, db.SecretRekeyLockKey)
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
	assertStoreReadsUnder(t, pool, oldID, newIdentity(t))
	if rows := rekeyAuditRows(t, pool); len(rows) != 0 {
		t.Errorf("a refused rotation wrote %d secret.rekey rows, want 0", len(rows))
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awskms

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// throwawayDB is a migrated database of its own, guarded by WARDYN_TEST_PG.
func throwawayDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed KEK test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_akms_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

// kekStore builds the registered "pg" store with k wrapping every write.
func kekStore(t *testing.T, pool *pgxpool.Pool, k kek.KEK) *secretstorepg.Store {
	t.Helper()
	s, err := secretstore.New("pg", secretstore.Deps{Pool: pool, KEK: k, KEKWrites: true})
	if err != nil {
		t.Fatalf("secretstore.New(pg): %v", err)
	}
	return s.(*secretstorepg.Store)
}

// The AWS KMS KEK through the pg store, end to end: rows (a boot key among
// them) are sealed under it and read back, on a restarted wardynd too;
// `-rewrap` has nothing to move (KMS rotation is transparent); and a wrap and
// value copied onto another row in SQL are refused as a definitive answer
// that does not destroy the row.
func TestKEK_PGSealReadAndMovedWrap(t *testing.T) {
	pool := throwawayDB(t)
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	s := kekStore(t, pool, k)
	ctx := t.Context()
	rows := []struct{ owner, name, value string }{
		{"", "wardyn-signing-key", "boot-key"},
		{"", "github-app-key", "operator-value"},
		{"alice@example.com", "pat", "alice-value"},
	}
	for _, r := range rows {
		if err := s.For(r.owner).Put(ctx, r.name, []byte(r.value)); err != nil {
			t.Fatalf("Put(%s/%s): %v", r.owner, r.name, err)
		}
	}
	readAll := func(s *secretstorepg.Store, when string) {
		t.Helper()
		for _, r := range rows {
			if v, err := s.For(r.owner).Get(ctx, r.name); err != nil || string(v) != r.value {
				t.Fatalf("%s: Get(%s/%s) = (%q, %v)", when, r.owner, r.name, v, err)
			}
		}
	}
	if got := s.KeyService(); got != "AWS KMS in us-east-1" || s.KeyServiceStaticKey() {
		t.Fatalf("KeyService = %q, KeyServiceStaticKey = %v; want AWS KMS in us-east-1 under role credentials", got, s.KeyServiceStaticKey())
	}
	readAll(s, "after the writes")
	readAll(kekStore(t, pool, newTestKEK(t, f)), "on a restarted wardynd")

	var kekID string
	if err := pool.QueryRow(ctx, `SELECT kek_id FROM secrets WHERE owned_by='alice@example.com' AND name='pat'`).Scan(&kekID); err != nil || kekID != k.ID() {
		t.Fatalf("kek_id = (%q, %v); want %q", kekID, err, k.ID())
	}
	// Under a long-lived access key the store says so, but only while the KEK wraps writes.
	useCreds(t, longLivedKeyID, "")
	static, err := NewKEK(ctx, Config{KeyARN: f.arn, Endpoint: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if st := kekStore(t, pool, static); !st.KeyServiceStaticKey() {
		t.Fatal("KeyServiceStaticKey = false under a long-lived access key")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := secretstore.New("pg", secretstore.Deps{Pool: pool, KEK: static, AgeIdentity: id})
	if err != nil || readOnly.(*secretstorepg.Store).KeyServiceStaticKey() {
		t.Fatalf("a read-only AWS KMS KEK reports a key service in use: (%v, %v)", readOnly, err)
	}
	d := secretstore.Deps{Pool: pool, KEK: k, KEKWrites: true}
	if res, err := secretstorepg.RewrapKeys(ctx, d); err != nil || res.Rewrapped != 0 || res.KeyService != k.ID() {
		t.Fatalf("RewrapKeys = (%+v, %v); want 0 rows under %s", res, err, k.ID())
	}

	if _, err := pool.Exec(ctx, `UPDATE secrets AS b SET wrapped_dek=a.wrapped_dek, ciphertext=a.ciphertext
		FROM secrets AS a WHERE a.owned_by='' AND a.name='github-app-key' AND b.owned_by='alice@example.com' AND b.name='pat'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.For("alice@example.com").Get(ctx, "pat")
	if err == nil || errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, secretstore.ErrNotFound) || !errors.Is(err, kek.ErrRefused) {
		t.Fatalf("Get of a row carrying another row's wrap = %v; want a definitive kek.ErrRefused", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM secrets WHERE owned_by='alice@example.com' AND name='pat'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the refused row: count = (%d, %v); a reader destroys nothing", n, err)
	}
}

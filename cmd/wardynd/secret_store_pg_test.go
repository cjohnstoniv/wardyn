// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The boot half of envelope v1, against a real Postgres: buildSecretStore
// converts v0 rows BEFORE the boot keys are read (rule 11), refuses an
// ephemeral key over rows sealed under an age key (rule 14), and a tampered
// boot-key row fails boot instead of being minted over (rule 9). Each test uses
// its own throwaway database: conversion rewrites every v0 row in the table.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envelopeDB is a fresh, migrated database on the WARDYN_TEST_PG server,
// dropped on cleanup — this file's own copy of the throwaway pattern in
// internal/secretstore/pg's rekey tests (an unexported _test.go helper there).
func envelopeDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed boot-conversion test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_env_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	return pool
}

// seedV0Row writes an operator row exactly as a pre-envelope wardynd did.
func seedV0Row(t *testing.T, pool *pgxpool.Pool, to *age.X25519Identity, name string, value []byte) {
	t.Helper()
	seedOwnedV0Row(t, pool, to, "", name, value)
}

// seedOwnedV0Row is seedV0Row for owner's namespace.
func seedOwnedV0Row(t *testing.T, pool *pgxpool.Pool, to *age.X25519Identity, owner, name string, value []byte) {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, to.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(value); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO secrets (owned_by, name, ciphertext) VALUES ($1, $2, $3)`, owner, name, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func envelopeColumns(t *testing.T, pool *pgxpool.Pool, name string) (version int16, wrapped, ct []byte) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT enc_version, wrapped_dek, ciphertext FROM secrets WHERE owned_by='' AND name=$1`, name,
	).Scan(&version, &wrapped, &ct); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return version, wrapped, ct
}

// TestPG_BootConvertsV0BeforeBootKeysAreRead: a signing key stored by a
// pre-envelope wardynd survives the upgrade — buildSecretStore converts it, so
// loadOrCreateSigningKey reads the SAME key rather than failing or minting a
// new one — and a second boot converts nothing.
func TestPG_BootConvertsV0BeforeBootKeysAreRead(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	orig, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pemBytes, err := marshalECPrivateKeyPEM(orig)
	if err != nil {
		t.Fatal(err)
	}
	seedV0Row(t, pool, id, secretSigningKey, pemBytes)

	rec := &capturingRecorder{}
	secrets, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, rec)
	if err != nil {
		t.Fatalf("first v1 boot: %v", err)
	}
	got, err := loadOrCreateSigningKey(ctx, secrets)
	if err != nil {
		t.Fatalf("loadOrCreateSigningKey after conversion: %v", err)
	}
	if !got.Equal(orig) {
		t.Fatal("the boot read a different signing key; the v0 row was not converted before it")
	}
	// Both reads of the key are on the record, once each and both purpose boot:
	// the conversion's, of a v0 row that has no ref, then the boot's, which
	// names the envelope's key.
	if len(rec.got) != 2 {
		t.Fatalf("recorded %d audit events, want the conversion's read and the boot's", len(rec.got))
	}
	for i, wantRef := range []string{"", "local/platform:"} {
		ev := rec.got[i]
		var d map[string]string
		if ev.Action != "secret.read" || ev.Target != secretSigningKey || ev.Outcome != "success" || json.Unmarshal(ev.Data, &d) != nil {
			t.Fatalf("unexpected audit event %+v", ev)
		}
		if d["purpose"] != "boot" {
			t.Errorf("read %d records purpose %q, want boot", i, d["purpose"])
		}
		if ref, ok := d["ref"]; wantRef == "" && ok || !strings.HasPrefix(ref, wantRef) {
			t.Errorf("read %d records ref %q, want %q", i, ref, wantRef+"…")
		}
		if _, ok := d["row_owner"]; !ok {
			t.Errorf("read %d records no row_owner: %v", i, d)
		}
	}
	version, wrapped, ct := envelopeColumns(t, pool, secretSigningKey)
	if version != 1 {
		t.Fatalf("signing-key row enc_version = %d after boot, want 1", version)
	}

	if _, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{}); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	v2, w2, ct2 := envelopeColumns(t, pool, secretSigningKey)
	if v2 != 1 || !bytes.Equal(w2, wrapped) || !bytes.Equal(ct2, ct) {
		t.Fatal("the second boot rewrote a converted row; conversion must be a no-op once done")
	}
}

// secretAuditRows is every secret.* row in audit_events, oldest first.
func secretAuditRows(t *testing.T, pool *pgxpool.Pool) []types.AuditEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT actor_type, actor, action, target, outcome, data FROM audit_events WHERE action LIKE 'secret.%' ORDER BY time, id`)
	if err != nil {
		t.Fatalf("query secret.* audit rows: %v", err)
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (types.AuditEvent, error) {
		var ev types.AuditEvent
		err := r.Scan(&ev.ActorType, &ev.Actor, &ev.Action, &ev.Target, &ev.Outcome, &ev.Data)
		return ev, err
	})
	if err != nil {
		t.Fatalf("scan secret.* audit rows: %v", err)
	}
	return evs
}

// TestPG_BootConversionRecordsABootReadPerRow is the owner's ruling
// (2026-09-25) on #717, through the real audit chain into audit_events: every
// row the boot conversion opens writes one secret.read, purpose boot, naming
// that row and never its value, and nothing else (no secret.convert). A second
// boot converts nothing and writes nothing.
func TestPG_BootConversionRecordsABootReadPerRow(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	seeded := []struct{ owner, name, value string }{
		{"", "anthropic-api-key", "sk-ant-not-a-real-key-111111111111"},
		{"alice@corp.example", "github-pat", "ghp_not_a_real_token_1111111111"},
		{"bob@corp.example", "github-pat", "ghp_not_a_real_token_2222222222"},
	}
	for _, r := range seeded {
		seedOwnedV0Row(t, pool, id, r.owner, r.name, []byte(r.value))
	}
	rec, fan, _, _, err := buildAuditChain(ctx, "", "", "", pool, secretmask.NewRegistry())
	if err != nil {
		t.Fatalf("build the audit chain: %v", err)
	}
	if fan != nil {
		t.Fatal("no sinks were configured, yet the audit chain has a fanout")
	}

	if _, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, rec); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	evs := secretAuditRows(t, pool)
	if len(evs) != len(seeded) {
		t.Fatalf("first boot wrote %d secret.* audit rows, want one secret.read per converted row (%d)", len(evs), len(seeded))
	}
	got := map[[2]string]bool{}
	for _, ev := range evs {
		var d map[string]string
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decode %s data %s: %v", ev.Action, ev.Data, err)
		}
		if ev.Action != "secret.read" || ev.ActorType != types.ActorSystem || ev.Actor != "wardynd" || ev.Outcome != "success" {
			t.Errorf("conversion row = (%s, %s, %s, %s), want (secret.read, system, wardynd, success)", ev.Action, ev.ActorType, ev.Actor, ev.Outcome)
		}
		if d["purpose"] != string(secretstore.PurposeBoot) || d["store"] != "pg" || d["owner"] != d["row_owner"] {
			t.Errorf("conversion row data = %v, want purpose boot, store pg, owner = row_owner", d)
		}
		if _, ok := d["ref"]; ok {
			t.Errorf("conversion row records a ref %q; a v0 row has none", d["ref"])
		}
		got[[2]string{d["row_owner"], ev.Target}] = true
		for _, r := range seeded {
			if strings.Contains(string(ev.Data), r.value) || strings.Contains(ev.Target, r.value) {
				t.Errorf("conversion row carries the value of %s/%s", r.owner, r.name)
			}
		}
	}
	for _, r := range seeded {
		if !got[[2]string{r.owner, r.name}] {
			t.Errorf("no conversion row names %s/%s", r.owner, r.name)
		}
	}

	if _, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, rec); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if n := len(secretAuditRows(t, pool)); n != len(seeded) {
		t.Errorf("second boot left %d secret.* audit rows, want the first boot's %d: it converts nothing, so it reads nothing", n, len(seeded))
	}
	st, err := secretstorepg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range seeded {
		if v, err := st.For(r.owner).Get(ctx, r.name); err != nil || string(v) != r.value {
			t.Errorf("%s/%s after conversion = (%q, %v), want its value", r.owner, r.name, v, err)
		}
	}
}

// TestOperationsDoc_CarriesTheEphemeralKeyRecovery: the ephemeral-key boot
// refusal hands the operator a statement to run, and OPERATIONS.md must carry
// the same one, so the runbook and the refusal never disagree on what to delete.
func TestOperationsDoc_CarriesTheEphemeralKeyRecovery(t *testing.T) {
	// Quoted whole, as the runbook's psql -c argument: a bare substring match
	// would pass a statement cut short to a prefix of the documented one.
	if want := `-c "` + ephemeralKeyRecoverySQL + `"`; !strings.Contains(readDoc(t, "docs/OPERATIONS.md"), want) {
		t.Errorf("docs/OPERATIONS.md does not carry the ephemeral-key recovery %s", want)
	}
}

// TestPG_BootAbortsOnAnUndecryptableV0Row: a v0 row the key cannot read stops
// boot, naming the row.
func TestPG_BootAbortsOnAnUndecryptableV0Row(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	stray, _ := age.GenerateX25519Identity()
	seedV0Row(t, pool, stray, "github-app-key", []byte("under-another-key"))

	_, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err == nil {
		t.Fatal("boot succeeded over an undecryptable v0 row")
	}
	for _, want := range []string{"refusing to start", `name="github-app-key"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("boot error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "under-another-key") {
		t.Error("boot error carries a value")
	}
}

// TestPG_BootRefusesAnEphemeralKeyOverAgeSealedRows is rule 14: with
// WARDYN_AGE_KEY unset, a store holding v1 local rows — or v0 rows — refuses to
// boot instead of minting a key that would strand them. An empty store boots.
// The refusal also names the way out when no key exists to set (#755): rows
// written under an EARLIER ephemeral key are unrecoverable and must be deleted.
func TestPG_BootRefusesAnEphemeralKeyOverAgeSealedRows(t *testing.T) {
	empty := envelopeDB(t)
	if _, err := buildSecretStore(t.Context(), empty, "", nil, "", storeClients{}, &capturingRecorder{}); err != nil {
		t.Fatalf("an ephemeral key over an empty store must boot: %v", err)
	}

	for label, seed := range map[string]func(*testing.T, *pgxpool.Pool){
		"v1 local rows": func(t *testing.T, pool *pgxpool.Pool) {
			id, _ := age.GenerateX25519Identity()
			s, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Put(t.Context(), "anthropic-api-key", []byte("v")); err != nil {
				t.Fatal(err)
			}
		},
		"v0 rows": func(t *testing.T, pool *pgxpool.Pool) {
			id, _ := age.GenerateX25519Identity()
			seedV0Row(t, pool, id, "anthropic-api-key", []byte("v"))
		},
	} {
		t.Run(label, func(t *testing.T) {
			pool := envelopeDB(t)
			seed(t, pool)
			_, err := buildSecretStore(t.Context(), pool, "", nil, "", storeClients{}, &capturingRecorder{})
			if err == nil || !strings.Contains(err.Error(), "WARDYN_AGE_KEY is unset, but 1 stored secrets") {
				t.Fatalf("ephemeral boot over %s = %v, want the rule-14 refusal", label, err)
			}
			for _, want := range []string{"earlier ephemeral key", "unrecoverable", "DELETE FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%' OR kek_id LIKE 'local/%'"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
		})
	}
}

// TestPG_TamperedBootKeyFailsClosed is rule 9 at the call site that motivates
// it: a signing-key row holding another row's envelope is REFUSED, not read as
// missing — so loadOrCreateSecret fails boot and never Puts a fresh key over it.
func TestPG_TamperedBootKeyFailsClosed(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	secrets, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(ctx, secrets); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSessionKey(ctx, secrets); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE secrets SET wrapped_dek=s.wrapped_dek, ciphertext=s.ciphertext
		  FROM (SELECT wrapped_dek, ciphertext FROM secrets WHERE owned_by='' AND name=$2) AS s
		 WHERE secrets.owned_by='' AND secrets.name=$1`, secretSigningKey, secretSessionKey); err != nil {
		t.Fatal(err)
	}
	_, tamperedW, tamperedCT := envelopeColumns(t, pool, secretSigningKey)

	if _, err := loadOrCreateSigningKey(ctx, secrets); err == nil {
		t.Fatal("loadOrCreateSigningKey accepted a tampered row")
	}
	_, w, ct := envelopeColumns(t, pool, secretSigningKey)
	if !bytes.Equal(w, tamperedW) || !bytes.Equal(ct, tamperedCT) {
		t.Fatal("loadOrCreateSecret wrote over the tampered row; a refusal must never be treated as not-found")
	}
}

// TestPG_ServedStoreSweepsExpiredCredentials: the store buildSecretStore hands
// the server is wrapped for auditing, and the daily sweep
// (api.Server.SweepExpiredCredentials) must still reach its DeleteExpired —
// a wrapper that hides it turns least retention off without a word.
func TestPG_ServedStoreSweepsExpiredCredentials(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	secrets, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatalf("buildSecretStore: %v", err)
	}
	sw, ok := secrets.(interface {
		DeleteExpired(context.Context) ([]secretstore.Expired, error)
	})
	if !ok {
		t.Fatalf("buildSecretStore returned %T, which has no DeleteExpired: the expiry sweep never runs", secrets)
	}
	past := secretstore.WithExpiry(ctx, time.Now().Add(-time.Hour))
	if err := secrets.For("bob").Put(past, "wardyn-harness-aws-oauth", []byte(`{"refresh_token":"x"}`)); err != nil {
		t.Fatalf("put an expired sign-in: %v", err)
	}
	gone, err := sw.DeleteExpired(ctx)
	if err != nil || len(gone) != 1 || gone[0].Owner != "bob" || gone[0].Name != "wardyn-harness-aws-oauth" {
		t.Fatalf("DeleteExpired = %+v, %v; want bob's expired sign-in", gone, err)
	}
}

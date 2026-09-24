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

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
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

// seedV0Row writes a row exactly as a pre-envelope wardynd did.
func seedV0Row(t *testing.T, pool *pgxpool.Pool, to *age.X25519Identity, name string, value []byte) {
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
	if _, err := pool.Exec(context.Background(), `INSERT INTO secrets (name, ciphertext) VALUES ($1, $2)`, name, buf.Bytes()); err != nil {
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
	secrets, err := buildSecretStore(ctx, pool, id.String(), "", storeClients{}, rec)
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
	// Both reads of the key are on the record, once each: the conversion's and
	// the boot's, which names the envelope's key.
	var purposes []string
	for _, ev := range rec.got {
		var d map[string]string
		if ev.Action != "secret.read" || ev.Target != secretSigningKey || ev.Outcome != "success" || json.Unmarshal(ev.Data, &d) != nil {
			t.Fatalf("unexpected audit event %+v", ev)
		}
		if d["purpose"] == "boot" && !strings.HasPrefix(d["ref"], "local:") {
			t.Errorf("boot read records ref %q, want the row's local kek_id", d["ref"])
		}
		if _, ok := d["row_owner"]; !ok {
			t.Errorf("%s read records no row_owner: %v", d["purpose"], d)
		}
		purposes = append(purposes, d["purpose"])
	}
	if strings.Join(purposes, ",") != "migrate,boot" {
		t.Fatalf("recorded secret.read purposes %v, want [migrate boot]", purposes)
	}
	version, wrapped, ct := envelopeColumns(t, pool, secretSigningKey)
	if version != 1 {
		t.Fatalf("signing-key row enc_version = %d after boot, want 1", version)
	}

	if _, err := buildSecretStore(ctx, pool, id.String(), "", storeClients{}, &capturingRecorder{}); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	v2, w2, ct2 := envelopeColumns(t, pool, secretSigningKey)
	if v2 != 1 || !bytes.Equal(w2, wrapped) || !bytes.Equal(ct2, ct) {
		t.Fatal("the second boot rewrote a converted row; conversion must be a no-op once done")
	}
}

// TestPG_BootAbortsOnAnUndecryptableV0Row: a v0 row the key cannot read stops
// boot, naming the row.
func TestPG_BootAbortsOnAnUndecryptableV0Row(t *testing.T) {
	pool := envelopeDB(t)
	id, _ := age.GenerateX25519Identity()
	stray, _ := age.GenerateX25519Identity()
	seedV0Row(t, pool, stray, "github-app-key", []byte("under-another-key"))

	_, err := buildSecretStore(t.Context(), pool, id.String(), "", storeClients{}, &capturingRecorder{})
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
	if _, err := buildSecretStore(t.Context(), empty, "", "", storeClients{}, &capturingRecorder{}); err != nil {
		t.Fatalf("an ephemeral key over an empty store must boot: %v", err)
	}

	for label, seed := range map[string]func(*testing.T, *pgxpool.Pool){
		"v1 local rows": func(t *testing.T, pool *pgxpool.Pool) {
			id, _ := age.GenerateX25519Identity()
			s, err := buildSecretStore(t.Context(), pool, id.String(), "", storeClients{}, &capturingRecorder{})
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
			_, err := buildSecretStore(t.Context(), pool, "", "", storeClients{}, &capturingRecorder{})
			if err == nil || !strings.Contains(err.Error(), "WARDYN_AGE_KEY is unset, but 1 stored secrets") {
				t.Fatalf("ephemeral boot over %s = %v, want the rule-14 refusal", label, err)
			}
			for _, want := range []string{"earlier ephemeral key", "unrecoverable", "DELETE FROM secrets WHERE enc_version=0 OR kek_id LIKE 'local:%'"} {
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
	secrets, err := buildSecretStore(ctx, pool, id.String(), "", storeClients{}, &capturingRecorder{})
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

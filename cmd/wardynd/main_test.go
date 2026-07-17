// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
)

// fakeSecretStore is a hand-rolled secretKeyStore fake. getErr/getVal model the
// store's Get; putCalls records every Put so a test can assert that a fail-closed
// path NEVER overwrites existing ciphertext. It mimics the pg store's contract:
// a TRUE not-found wraps pgx.ErrNoRows; any other error (e.g. an age-decrypt
// failure) is a generic error that must NOT be treated as "key absent".
type fakeSecretStore struct {
	getVal   []byte
	getErr   error
	putCalls []putCall
	putErr   error
}

type putCall struct {
	name  string
	value []byte
}

func (f *fakeSecretStore) Get(_ context.Context, _ string) ([]byte, error) {
	return f.getVal, f.getErr
}

func (f *fakeSecretStore) Put(_ context.Context, name string, value []byte) error {
	f.putCalls = append(f.putCalls, putCall{name: name, value: value})
	return f.putErr
}

// notFoundErr mirrors how the pg secret store reports a missing row: it wraps
// pgx.ErrNoRows so callers can distinguish "absent" from "present-but-broken".
func notFoundErr() error {
	return fmt.Errorf("pg secretstore: secret %q not found: %w", "k", pgx.ErrNoRows)
}

// decryptErr mirrors a transient/permanent age-decrypt failure: a generic error
// that does NOT wrap pgx.ErrNoRows. Treating this as "absent" would overwrite
// the existing ciphertext and silently invalidate every issued SVID/session.
func decryptErr() error {
	return errors.New("pg secretstore: decrypt k: age decrypt: no identity matched")
}

// Finding A (boot-key destruction): a decrypt error must FAIL CLOSED — the
// helper returns the error and does NOT call Put, so the existing ciphertext is
// never overwritten.
func TestLoadOrCreateSecret_DecryptErrorFailsClosed(t *testing.T) {
	store := &fakeSecretStore{getErr: decryptErr()}
	genCalled := false
	_, err := loadOrCreateSecret(context.Background(), store, "wardyn-signing-key",
		func(b []byte) bool { return len(b) > 0 },
		func() ([]byte, error) { genCalled = true; return []byte("new"), nil },
	)
	if err == nil {
		t.Fatal("expected error on decrypt failure (fail closed), got nil")
	}
	if len(store.putCalls) != 0 {
		t.Fatalf("decrypt error must NOT overwrite the key; got %d Put call(s)", len(store.putCalls))
	}
	if genCalled {
		t.Fatal("decrypt error must NOT generate a fresh key")
	}
}

// Finding A: a TRUE not-found generates a fresh key and persists it.
func TestLoadOrCreateSecret_NotFoundGeneratesAndPuts(t *testing.T) {
	store := &fakeSecretStore{getErr: notFoundErr()}
	want := []byte("freshly-generated")
	got, err := loadOrCreateSecret(context.Background(), store, "wardyn-signing-key",
		func(b []byte) bool { return len(b) > 0 },
		func() ([]byte, error) { return want, nil },
	)
	if err != nil {
		t.Fatalf("not-found path should succeed: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("returned key %q, want %q", got, want)
	}
	if len(store.putCalls) != 1 {
		t.Fatalf("not-found should Put exactly once; got %d", len(store.putCalls))
	}
	if store.putCalls[0].name != "wardyn-signing-key" || string(store.putCalls[0].value) != string(want) {
		t.Fatalf("Put got (%q,%q), want (wardyn-signing-key,%q)",
			store.putCalls[0].name, store.putCalls[0].value, want)
	}
}

// An existing, valid key is returned untouched (no regeneration, no Put).
func TestLoadOrCreateSecret_ExistingKeyReturnedNoPut(t *testing.T) {
	existing := []byte("existing-key-material")
	store := &fakeSecretStore{getVal: existing}
	got, err := loadOrCreateSecret(context.Background(), store, "wardyn-session-key",
		func(b []byte) bool { return len(b) >= len(existing) },
		func() ([]byte, error) { t.Fatal("must not generate when a valid key exists"); return nil, nil },
	)
	if err != nil {
		t.Fatalf("existing key path should succeed: %v", err)
	}
	if string(got) != string(existing) {
		t.Fatalf("returned %q, want existing %q", got, existing)
	}
	if len(store.putCalls) != 0 {
		t.Fatalf("existing key must not be re-Put; got %d Put call(s)", len(store.putCalls))
	}
}

// An existing-but-invalid value (present, no error, but fails the validity
// predicate — e.g. a too-short session key) is regenerated. This preserves the
// original "len >= 32" upgrade behaviour without conflating it with the
// fail-closed decrypt path.
func TestLoadOrCreateSecret_PresentButInvalidRegenerates(t *testing.T) {
	store := &fakeSecretStore{getVal: []byte("short")}
	want := []byte("0123456789abcdef0123456789abcdef")
	got, err := loadOrCreateSecret(context.Background(), store, "wardyn-session-key",
		func(b []byte) bool { return len(b) >= 32 },
		func() ([]byte, error) { return want, nil },
	)
	if err != nil {
		t.Fatalf("present-but-invalid path should regenerate: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("returned %q, want regenerated %q", got, want)
	}
	if len(store.putCalls) != 1 {
		t.Fatalf("present-but-invalid should Put exactly once; got %d", len(store.putCalls))
	}
}

// Sanity: the real session-key generator produces a 32-byte key, exercising the
// loadOrCreateSessionKey wiring end to end against the fake (not-found path).
func TestLoadOrCreateSessionKey_NotFoundIsThirtyTwoBytes(t *testing.T) {
	store := &fakeSecretStore{getErr: notFoundErr()}
	key, err := loadOrCreateSessionKey(context.Background(), store)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("session key length %d, want 32", len(key))
	}
	if len(store.putCalls) != 1 {
		t.Fatalf("session key not-found should Put once; got %d", len(store.putCalls))
	}
}

// Sanity: a decrypt error from the session-key path fails closed (no Put).
func TestLoadOrCreateSessionKey_DecryptErrorFailsClosed(t *testing.T) {
	store := &fakeSecretStore{getErr: decryptErr()}
	if _, err := loadOrCreateSessionKey(context.Background(), store); err == nil {
		t.Fatal("expected fail-closed error on decrypt failure")
	}
	if len(store.putCalls) != 0 {
		t.Fatalf("decrypt error must not overwrite session key; got %d Put(s)", len(store.putCalls))
	}
}

// Sanity: the signing-key path round-trips — a fresh ES256 key is generated,
// persisted, and parses back into a usable *ecdsa.PrivateKey on not-found.
func TestLoadOrCreateSigningKey_NotFoundGeneratesParsableKey(t *testing.T) {
	store := &fakeSecretStore{getErr: notFoundErr()}
	key, err := loadOrCreateSigningKey(context.Background(), store)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	if key == nil || key.PublicKey.Curve == nil {
		t.Fatal("signing key not a usable ecdsa key")
	}
	if len(store.putCalls) != 1 {
		t.Fatalf("signing key not-found should Put once; got %d", len(store.putCalls))
	}
}

// Sanity: a decrypt error on the signing-key path fails closed (no Put) — the
// most damaging case, since overwriting the signing key invalidates every SVID.
func TestLoadOrCreateSigningKey_DecryptErrorFailsClosed(t *testing.T) {
	store := &fakeSecretStore{getErr: decryptErr()}
	if _, err := loadOrCreateSigningKey(context.Background(), store); err == nil {
		t.Fatal("expected fail-closed error on decrypt failure")
	}
	if len(store.putCalls) != 0 {
		t.Fatalf("decrypt error must not overwrite signing key; got %d Put(s)", len(store.putCalls))
	}
}

// -gen-age-key must print a parseable AGE-SECRET-KEY that is NOT the
// publicly-known demo key, with no DSN/DB work (the flag returns via
// genAndPrintAgeKey before validateConfig). We exercise the extracted helper
// directly so the test needs no flag.Parse / Postgres.
func TestGenAndPrintAgeKey_ParseableNotPublic(t *testing.T) {
	var buf bytes.Buffer
	if err := genAndPrintAgeKey(&buf); err != nil {
		t.Fatalf("genAndPrintAgeKey: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(out, "AGE-SECRET-KEY-") {
		t.Fatalf("output %q is not an AGE-SECRET-KEY", out)
	}
	if out == knownPublicAgeKey {
		t.Fatal("generated the publicly-known demo age key")
	}
	if _, err := age.ParseX25519Identity(out); err != nil {
		t.Fatalf("generated key is not parseable: %v", err)
	}
}

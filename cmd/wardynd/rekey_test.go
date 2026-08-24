// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Unit coverage for the -rotate-age-key maintenance mode's NON-database half:
// the key-file reader/writer and every refusal that fires before a connection is
// ever attempted. Each one is a fail-closed branch guarding the only copy of the
// secret store's master key, so each gets a case here. The transactional rekey
// itself is covered against a real Postgres in
// internal/secretstore/pg/rekey_pg_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// rekeyFlags builds the minimal bootFlags rotateAgeKeyMode reads, with a valid
// posture by default so each test perturbs exactly one field.
func rekeyFlags(dsn, storeSel, ageKey string) *bootFlags {
	return &bootFlags{
		dsn:            &dsn,
		secretStoreSel: &storeSel,
		ageKey:         &ageKey,
		auditSinks:     new(string),
		auditSpool:     new(string),
	}
}

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id
}

// TestRotateAgeKeyMode_RefusesBeforeTouchingTheDatabase: every precondition is
// checked before db.Connect, so a misconfigured rotation cannot get as far as
// holding a lock or opening a transaction. Each case uses an unreachable DSN —
// if a refusal ever stopped firing, the test would fail on a connect error
// rather than pass silently.
func TestRotateAgeKeyMode_RefusesBeforeTouchingTheDatabase(t *testing.T) {
	const unreachableDSN = "postgres://nobody@127.0.0.1:1/nope?connect_timeout=1"
	good := newIdentity(t).String()
	other := newIdentity(t).String()

	dir := t.TempDir()
	envFile := filepath.Join(dir, "dotenv")
	if err := os.WriteFile(envFile, []byte("WARDYN_AGE_KEY="+good+"\n"), 0o600); err != nil {
		t.Fatalf("write env fixture: %v", err)
	}
	otherKeyFile := filepath.Join(dir, "other.key")
	if err := os.WriteFile(otherKeyFile, []byte(other+"\n"), 0o600); err != nil {
		t.Fatalf("write other-key fixture: %v", err)
	}

	tests := []struct {
		name    string
		f       *bootFlags
		path    string
		wantSub string
	}{
		{
			name:    "no dsn",
			f:       rekeyFlags("", "pg", good),
			path:    filepath.Join(dir, "unused.key"),
			wantSub: "WARDYN_PG_DSN",
		},
		{
			name:    "non-pg secret store",
			f:       rekeyFlags(unreachableDSN, "openbao", good),
			path:    filepath.Join(dir, "unused.key"),
			wantSub: "-secret-store",
		},
		{
			// The ephemeral-key deployment: nothing durable to rotate FROM.
			name:    "no current key",
			f:       rekeyFlags(unreachableDSN, "pg", ""),
			path:    filepath.Join(dir, "unused.key"),
			wantSub: "no durable key to rotate FROM",
		},
		{
			name:    "unparseable current key",
			f:       rekeyFlags(unreachableDSN, "pg", "not-an-age-key"),
			path:    filepath.Join(dir, "unused.key"),
			wantSub: "parse the current age identity",
		},
		{
			// The footgun this guard exists for: pointing -rotate-age-key at
			// deploy/compose/.env would otherwise REPLACE the whole env file
			// with a bare key line.
			name:    "path is an env file, not a key file",
			f:       rekeyFlags(unreachableDSN, "pg", good),
			path:    envFile,
			wantSub: "is not an age key file",
		},
		{
			// A real key file, but a DIFFERENT deployment's key: replacing it
			// would destroy the only copy of a key still in use elsewhere.
			name:    "key file holds a different identity",
			f:       rekeyFlags(unreachableDSN, "pg", good),
			path:    otherKeyFile,
			wantSub: "does not hold the identity WARDYN_AGE_KEY names",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rotateAgeKeyMode(tt.f, tt.path)
			if err == nil {
				t.Fatal("rotateAgeKeyMode succeeded; the refusal did not fire")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}

	// The env fixture must survive the refusal that protects it — the whole
	// point is that the file is not touched.
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("re-read env fixture: %v", err)
	}
	if !strings.HasPrefix(string(b), "WARDYN_AGE_KEY=") {
		t.Errorf("the refused env file was modified: %q", b)
	}
}

// TestReadAgeKeyFile covers the three shapes the key file can take: absent (a
// first rotation creates it), age-keygen output with leading comments, and a
// bare identity line.
func TestReadAgeKeyFile(t *testing.T) {
	dir := t.TempDir()
	id := newIdentity(t)

	got, err := readAgeKeyFile(filepath.Join(dir, "missing.key"))
	if err != nil || got != "" {
		t.Errorf("readAgeKeyFile(missing) = %q, %v; want \"\", nil", got, err)
	}

	commented := filepath.Join(dir, "keygen.key")
	body := "# created: 2026-08-23T00:00:00Z\n# public key: " + id.Recipient().String() + "\n" + id.String() + "\n"
	if err := os.WriteFile(commented, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err = readAgeKeyFile(commented)
	if err != nil {
		t.Fatalf("readAgeKeyFile(age-keygen output): %v", err)
	}
	if got != id.String() {
		t.Errorf("readAgeKeyFile skipped the wrong line: got %q", got)
	}

	empty := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(empty, []byte("\n\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err = readAgeKeyFile(empty); err != nil || got != "" {
		t.Errorf("readAgeKeyFile(blank) = %q, %v; want \"\", nil", got, err)
	}
}

// TestWriteKeyFileIsOwnerOnly pins the mode: this file IS the secret store's
// master key, so a group- or world-readable one hands over every stored secret.
// It must also be 0600 when it REPLACES an existing looser file, which is the
// case a plain os.WriteFile would get wrong (it keeps the existing mode).
func TestWriteKeyFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("seed a world-readable file: %v", err)
	}
	id := newIdentity(t)
	if err := writeKeyFile(path, id.String()); err != nil {
		t.Fatalf("writeKeyFile: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600 — the secret store's master key is readable by others", perm)
	}
	got, err := readAgeKeyFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != id.String() {
		t.Errorf("read back %q, want the written identity", got)
	}
	// No temp file left behind holding a copy of the key.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("%s.tmp survived the write (stat err = %v)", path, err)
	}
}

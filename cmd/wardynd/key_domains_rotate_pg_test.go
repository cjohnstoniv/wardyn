// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

// -rotate-age-key moves no principal key in a domain, but it refuses to run past
// a live key naming a domain the file does not declare, with nothing changed.
func TestRotateAgeKeyMode_RefusesAnUndeclaredDomain(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	oldID := newIdentity(t)
	seedRotateStore(t, pool, oldID)
	if _, err := pool.Exec(t.Context(), `INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key) VALUES ('alice', 'cred', 1, 'ghost', 'transit:transit/ghost', '\x00')`); err != nil {
		t.Fatal(err)
	}
	keyPath := t.TempDir() + "/age.key"
	if err := os.WriteFile(keyPath, []byte(oldID.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := rotateAgeKeyMode(rekeyFlags(dsn, "pg", oldID.String()), keyPath)
	if err == nil || !strings.Contains(err.Error(), "refusing to rotate") || !strings.Contains(err.Error(), `key domain "ghost"`) {
		t.Fatalf("-rotate-age-key past a live key in an undeclared domain = %v; want a refusal naming it", err)
	}
	if got, rerr := readAgeKeyFile(keyPath); rerr != nil || got != oldID.String() {
		t.Fatalf("key file after the refusal = %q, %v; want it unchanged", got, rerr)
	}
	if rows := rekeyAuditRows(t, pool); len(rows) != 0 {
		t.Fatalf("a refused rotation wrote %d secret.rekey rows", len(rows))
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// The whole mode against a real Postgres: refused with no key, refused while a non-HA daemon serves
// (its single-instance lock) or another maintenance mode runs, then a rotation and a -retire-previous,
// each leaving its secret.rekey row and every previous public key. Guarded by WARDYN_TEST_PG.
func TestRotateAuditReceiptKey_RefusesWhileServing(t *testing.T) {
	dsn, pool := rotateDatabase(t)
	id := newIdentity(t)
	ctx := context.Background()
	t.Setenv("WARDYN_AGE_KEY", id.String())
	resetFlags(t)
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })
	f := parseBootFlags()
	*f.dsn, *f.auditSpool = dsn, ""
	*f.rotateAuditReceiptKey = true

	if err := rotateAuditReceiptKeyMode(f); err == nil {
		t.Fatal("rotated an install with no key")
	}
	booted, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatalf("boot the store: %v", err)
	}
	first, err := loadOrCreateAuditReceiptKey(ctx, unlocked(booted))
	if err != nil {
		t.Fatalf("make the key: %v", err)
	}

	for _, c := range []struct {
		name string
		key  int64
		want string
	}{
		{"a serving daemon's single-instance lock", db.SingleInstanceLockKey, "a wardynd is serving this database"},
		{"another maintenance mode's rekey lock", db.SecretRekeyLockKey, "another maintenance mode holds the rekey advisory lock"},
	} {
		release, ok, err := db.TryAdvisoryLock(ctx, pool, c.key)
		if err != nil || !ok {
			t.Fatalf("hold %s: %v, %v", c.name, ok, err)
		}
		if err := rotateAuditReceiptKeyMode(f); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("under %s: err = %v, want %q", c.name, err, c.want)
		}
		release()
		if k, err := loadOrCreateAuditReceiptKey(ctx, unlocked(booted)); err != nil || k.KeyID() != first.KeyID() {
			t.Fatalf("the rotation refused under %s changed the key: %v", c.name, err)
		}
	}

	if err := rotateAuditReceiptKeyMode(f); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	*f.retirePrevious = true
	if err := rotateAuditReceiptKeyMode(f); err != nil {
		t.Fatalf("rotate and retire: %v", err)
	}
	now, err := loadOrCreateAuditReceiptKey(ctx, unlocked(booted))
	if err != nil {
		t.Fatal(err)
	}
	keys := now.PublicKeys()
	if len(keys) != 3 || keys[2].Kid != first.KeyID() || keys[2].Retired || !keys[1].Retired || keys[0].Retired {
		t.Fatalf("keys after rotate + retire = %+v", keys)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='secret.rekey' AND target=$1`, secretAuditReceiptKey).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("secret.rekey rows for the key = %d, want 2", rows)
	}
}

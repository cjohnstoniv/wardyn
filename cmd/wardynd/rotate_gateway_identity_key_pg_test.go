// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
)

// The whole mode against a real Postgres: a rotation and a -retire-previous through the
// store a serving daemon reads, each leaving its secret.rekey row; a serving
// non-HA daemon's lock refuses; and an install with no key is refused and gets
// none. Guarded by WARDYN_TEST_PG, through rotateDatabase.
func TestRotateGatewayIdentityKeyMode_EndToEnd(t *testing.T) {
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
	*f.rotateGatewayIdentityKey = true

	if err := rotateGatewayIdentityKeyMode(f); err == nil {
		t.Fatal("rotated an install with no key")
	}
	booted, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatalf("boot the store: %v", err)
	}
	if _, err := booted.Get(ctx, secretGatewayIdentityKey); err == nil {
		t.Fatal("a refused rotation made a key row")
	}
	first, err := unlocked(booted).gatewayIdentity()(ctx, true)
	if err != nil || first == nil {
		t.Fatalf("make the key: %v, %v", first, err)
	}

	read := func() *gatewayidentity.Signer {
		t.Helper()
		signer, err := unlocked(booted).gatewayIdentity()(ctx, false)
		if err != nil || signer == nil {
			t.Fatalf("read the key: %v, %v", signer, err)
		}
		return signer
	}

	release, ok, err := db.TryAdvisoryLock(ctx, pool, db.SingleInstanceLockKey)
	if err != nil || !ok {
		t.Fatalf("hold the single-instance lock: %v, %v", ok, err)
	}
	if err := rotateGatewayIdentityKeyMode(f); err == nil {
		t.Error("rotated while the single-instance lock was held")
	}
	release()
	if read().KeyID() != first.KeyID() {
		t.Fatal("the refused rotation changed the key")
	}

	if err := rotateGatewayIdentityKeyMode(f); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	planned := read()
	if got := testGatewayKids(t, planned); !slices.Equal(got, []string{planned.KeyID(), first.KeyID()}) {
		t.Errorf("after a rotation: kids = %v", got)
	}

	*f.rotateGatewayIdentityRetirePrevious = true
	if err := rotateGatewayIdentityKeyMode(f); err != nil {
		t.Fatalf("rotate and retire: %v", err)
	}
	dropped := read()
	if got := testGatewayKids(t, dropped); !slices.Equal(got, []string{dropped.KeyID()}) || dropped.KeyID() == planned.KeyID() {
		t.Errorf("after -retire-previous: kids = %v", got)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='secret.rekey' AND target=$1`, secretGatewayIdentityKey).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("secret.rekey rows for the key = %d, want 2", rows)
	}
}

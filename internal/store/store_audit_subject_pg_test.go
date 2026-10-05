// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The reads behind a WARDYN_AUDIT_SEAL=full actor ("subject:<id>"): a person's
// subject is their oldest identity row, an id maps back only through a bound
// row, and the key's destruction time is the latest generation's.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

func TestPG_AuditSubjectDirectory(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Second)

	if _, ok, err := pg.AuditSubjectFor(ctx, "alice"); err != nil || ok {
		t.Fatalf("a person with no identity row has a subject: ok %v, err %v", ok, err)
	}
	first, err := pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "alice", Issuer: "https://idp-one.test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "alice", Issuer: "https://idp-two.test"}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	id, ok, err := pg.AuditSubjectFor(ctx, "alice")
	if err != nil || !ok || id != first.ID.String() {
		t.Fatalf("subject = %q, %v, %v; want the oldest identity row %s", id, ok, err, first.ID)
	}
	for _, other := range []string{"", "bob"} {
		if _, ok, _ := pg.AuditSubjectFor(ctx, other); ok {
			t.Errorf("principal %q has a subject", other)
		}
	}

	if p, ok, err := pg.AuditPrincipalOf(ctx, id); err != nil || !ok || p != "alice" {
		t.Errorf("AuditPrincipalOf(%s) = %q, %v, %v; want alice", id, p, ok, err)
	}
	for _, bad := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if _, ok, err := pg.AuditPrincipalOf(ctx, bad); ok || err != nil {
			t.Errorf("AuditPrincipalOf(%q) = ok %v, err %v; want an unknown id", bad, ok, err)
		}
	}

	if _, ok, err := pg.AuditKeyDestroyedAt(ctx, "alice"); ok || err != nil {
		t.Fatalf("destroyed before any key exists: ok %v, err %v", ok, err)
	}
	for _, q := range []string{
		`INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key) VALUES ('alice', 'audit-seal', 1, 'default', 'k', '\x00')`,
		`UPDATE principal_keys SET wrapped_key = NULL, destroyed_at = now() WHERE owner = 'alice'`,
		`INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key) VALUES ('alice', 'audit-seal', 2, 'default', 'k', '\x00')`,
		`INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key, destroyed_at) VALUES ('alice', 'cred', 1, 'default', 'k', NULL, now() + interval '1 day')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	at, ok, err := pg.AuditKeyDestroyedAt(ctx, "alice")
	if err != nil || !ok || at.After(time.Now().Add(time.Minute)) {
		t.Errorf("AuditKeyDestroyedAt = %v, %v, %v; want the audit-seal generation's destruction, not the cred key's", at, ok, err)
	}
}

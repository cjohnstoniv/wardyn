// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An API token or SSH key that exists when migration 0194 runs is ordered against the cuts once, by the
// time comparison it replaces: one created before a cut is refused after the migration, one created
// after is accepted, and a step of the clock afterwards changes neither answer.
func TestPG_LegacyTokenAndKeyAreOrderedOnceByTheMigration(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, emptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// Back to a database from before the migration: no triggers, a sequence never drawn from, 0194 unrecorded.
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	exec(`DELETE FROM schema_migrations WHERE filename = '0194_revocation_order.sql'`)

	st := store.NewPG(pool)
	sub := "auth0|legacy-order-" + uuid.NewString()
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at, seq) VALUES ($1, now() - interval '1 hour', 0)`, sub)
	type cred struct {
		name    string
		created string // relative to now: the cut is at -1h
		revoked bool
		raw, fp string // the token's secret and the key's fingerprint, to read the rows back
	}
	creds := []*cred{{name: "before", created: "70 minutes", revoked: true}, {name: "after", created: "50 minutes"}}
	for _, c := range creds {
		c.raw, c.fp = "wdn_"+uuid.NewString(), "SHA256:"+uuid.NewString()
		tok, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: sub, Role: "user", UserType: "standard", Name: c.name}, c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddSSHKey(ctx, types.SSHPublicKey{Fingerprint: c.fp, Principal: sub, Name: c.name, PublicKey: "ssh-ed25519 AAAA", Role: "member"}); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE api_tokens SET created_at = now() - $2::interval, rev_seq = 0 WHERE id = $1`, tok.ID, c.created)
		exec(`UPDATE ssh_public_keys SET created_at = now() - $2::interval, rev_seq = 0 WHERE fingerprint = $1`, c.fp, c.created)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	rev := &pgSessionRevocations{pool: pool}
	check := func(label string) {
		t.Helper()
		for _, c := range creds {
			tok, err := st.GetAPITokenByRaw(ctx, c.raw)
			if err != nil {
				t.Fatal(err)
			}
			key, err := st.GetSSHKeyByFingerprint(ctx, c.fp)
			if err != nil {
				t.Fatal(err)
			}
			if tok.RevSeq < 1 || key.RevSeq < 1 {
				t.Fatalf("%s: %s token rev_seq %d, key rev_seq %d; want both ordered", label, c.name, tok.RevSeq, key.RevSeq)
			}
			for what, issue := range map[string]oidc.Issue{"token": {At: tok.CreatedAt, Seq: tok.RevSeq}, "key": {At: key.CreatedAt, Seq: key.RevSeq}} {
				got, err := rev.SessionStatus(ctx, sub, "", issue, -1)
				if err != nil {
					t.Fatal(err)
				}
				if (got == oidc.SessionRevoked) != c.revoked {
					t.Errorf("%s: %s created %s before now, cut 60 minutes before now: status %v, want revoked=%v", label, what, c.created, got, c.revoked)
				}
			}
		}
	}
	check("after the migration")
	// A step of the clock: every created_at now reads on the other side of the cut.
	exec(`UPDATE api_tokens SET created_at = now() - interval '5 hours' WHERE name = 'after'`)
	exec(`UPDATE ssh_public_keys SET created_at = now() - interval '5 hours' WHERE name = 'after'`)
	exec(`UPDATE api_tokens SET created_at = now() + interval '5 hours' WHERE name = 'before'`)
	exec(`UPDATE ssh_public_keys SET created_at = now() + interval '5 hours' WHERE name = 'before'`)
	check("after the clock stepped")
	// A cut made now ends the one that survived.
	time.Sleep(10 * time.Millisecond)
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatal(err)
	}
	for _, c := range creds {
		c.revoked = true
	}
	check("after a later cut")
}

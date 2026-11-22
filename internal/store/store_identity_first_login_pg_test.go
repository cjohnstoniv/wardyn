// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Guarded by WARDYN_TEST_PG like its neighbours. The statements are the ones a 0.10.1 replica runs against a database
// this release has migrated: they name no first_login_at.

func firstLoginOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT first_login_at FROM principal_identities WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read first_login_at: %v", err)
	}
	return at
}

// A row an older replica writes gets its first sign-in from the database: a sign-in's insert, and the first sign-in
// on a row a SCIM write made, both set last_login_at and the instant is the first sign-in whichever replica wrote
// it; a later sign-in moves nothing, and a SCIM write alone leaves it unknown.
func TestPG_FirstLoginIsSetForRowsAnOlderReplicaWrites(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-48 * time.Hour)
	same := func(got *time.Time, want time.Time, what string) {
		t.Helper()
		if got == nil || !got.Equal(want) {
			t.Errorf("%s: first_login_at = %v, want %v", what, got, want)
		}
	}

	// A sign-in's insert.
	var signedIn uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, email_lower, created_at, last_login_at)
		VALUES ('older-a', $1, '', '', 'a@example.com', $2, $2) RETURNING id`, "https://idp.example.com", at).Scan(&signedIn); err != nil {
		t.Fatal(err)
	}
	same(firstLoginOf(t, pool, signedIn), at, "an older replica's sign-in insert")
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET last_login_at = $2 WHERE id = $1`, signedIn, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	same(firstLoginOf(t, pool, signedIn), at, "after a later sign-in")

	// A SCIM row: provisioning alone is no sign-in; the first sign-in on it, by an older replica, is.
	var scim uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, created_at)
		VALUES (NULL, $1, $2, $3, $4) RETURNING id`, idIssuer, idTenant, uuid.NewString(), at).Scan(&scim); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET scim_external_id = 'ext-1', scim_user_name = 'u@example.com' WHERE id = $1`, scim); err != nil {
		t.Fatal(err)
	}
	if got := firstLoginOf(t, pool, scim); got != nil {
		t.Errorf("a provisioned row with no sign-in has first_login_at = %v, want unknown", got)
	}
	signIn := at.Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET principal = 'older-b', last_login_at = $2 WHERE id = $1`, scim, signIn); err != nil {
		t.Fatal(err)
	}
	same(firstLoginOf(t, pool, scim), signIn, "an older replica's first sign-in on a SCIM row")
	if _, err := pool.Exec(ctx, `UPDATE principal_identities SET last_login_at = $2 WHERE id = $1`, scim, signIn.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	same(firstLoginOf(t, pool, scim), signIn, "after the next sign-in")

	// A statement that names the column is not changed (this release's own).
	var named uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, email_lower, created_at, last_login_at, first_login_at)
		VALUES ('this-c', $1, '', '', 'c@example.com', $2, $3, $2) RETURNING id`, "https://idp.example.com", at, at.Add(time.Hour)).Scan(&named); err != nil {
		t.Fatal(err)
	}
	same(firstLoginOf(t, pool, named), at, "an insert that names first_login_at")
}

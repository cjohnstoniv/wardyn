// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// 0296 makes the first_login_at column, the trigger that fills it for a 0.10.1 replica's writes, and the backfill,
// in one transaction. A database that has applied it and no later migration therefore already gives a sign-in an old
// replica writes its first date: no window lies between the column and the trigger, which a trigger left to a later
// migration would open. The backfill still dates a row a sign-in made before the migration, and leaves unknown a
// row any SCIM write touched.
func TestMigration0296_FirstLoginTriggerShipsWithTheColumn_PG(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0296")
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-72 * time.Hour)
	const entra = "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"
	const tenant = "11111111-2222-3333-4444-555555555555"

	// Rows as 0.10.1 left them: no first_login_at column yet.
	pgExec(t, pool, `INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, email_lower, created_at, last_login_at)
		VALUES ('before-a', 'https://idp.example.com', '', '', 'a@example.com', $1, $2)`, at, at.Add(time.Hour))
	signedIn := pgScalar[uuid.UUID](t, pool, `SELECT id FROM principal_identities WHERE principal = 'before-a'`)
	pgExec(t, pool, `INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, scim_external_id, scim_user_name, created_at, last_login_at)
		VALUES ('before-b', $1, $2, $3, 'ext-b', 'b@example.com', $4, $5)`, entra, tenant, uuid.NewString(), at, at.Add(2*time.Hour))
	scimUser := pgScalar[uuid.UUID](t, pool, `SELECT id FROM principal_identities WHERE principal = 'before-b'`)
	pgExec(t, pool, `INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, scim_external_id, scim_user_name, created_at)
		VALUES (NULL, $1, $2, $3, 'ext-c', 'c@example.com', $4)`, entra, tenant, uuid.NewString(), at)
	provisioned := pgScalar[uuid.UUID](t, pool, `SELECT id FROM principal_identities WHERE scim_external_id = 'ext-c'`)

	migrateBefore(t, pool, "0297") // applies 0296 and nothing after it

	if got := pgScalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename LIKE '0298%')`); got {
		t.Fatal("control: a migration after 0296 has applied, so the test proves nothing about 0296 alone")
	}
	first := func(id uuid.UUID) *time.Time {
		return pgScalar[*time.Time](t, pool, `SELECT first_login_at FROM principal_identities WHERE id = $1`, id)
	}
	if got := first(signedIn); got == nil || !got.Equal(at) {
		t.Errorf("a row a sign-in made before the migration: first_login_at = %v, want its created_at %v", got, at)
	}
	if got := first(scimUser); got != nil {
		t.Errorf("a row a SCIM write touched: first_login_at = %v, want unknown", got)
	}
	if got := first(provisioned); got != nil {
		t.Errorf("a provisioned row with no sign-in: first_login_at = %v, want unknown", got)
	}

	// A 0.10.1 replica's writes after 0296 and before any later migration: they name no first_login_at.
	signIn := at.Add(24 * time.Hour)
	pgExec(t, pool, `INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, email_lower, created_at, last_login_at)
		VALUES ('after-d', 'https://idp.example.com', '', '', 'd@example.com', $1, $1)`, signIn)
	newRow := pgScalar[uuid.UUID](t, pool, `SELECT id FROM principal_identities WHERE principal = 'after-d'`)
	if got := first(newRow); got == nil || !got.Equal(signIn) {
		t.Errorf("a sign-in an older replica inserted: first_login_at = %v, want %v", got, signIn)
	}
	pgExec(t, pool, `UPDATE principal_identities SET principal = 'after-c', last_login_at = $2 WHERE id = $1`, provisioned, signIn)
	if got := first(provisioned); got == nil || !got.Equal(signIn) {
		t.Errorf("the first sign-in an older replica stamped on a provisioned row: first_login_at = %v, want %v", got, signIn)
	}
}

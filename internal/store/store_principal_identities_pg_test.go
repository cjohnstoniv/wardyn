// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for migration 0113_principal_identities. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

const (
	idIssuer = "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"
	idTenant = "11111111-2222-3333-4444-555555555555"
)

func identityAliases(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT value_lower FROM principal_identity_aliases WHERE identity_id = $1 AND kind = 'email' AND source = 'login' ORDER BY value_lower`, id)
	if err != nil {
		t.Fatalf("aliases: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan alias: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func identityCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM principal_identities`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// An Entra user with no people row and a user of another issuer each get exactly one identity row
// with their email alias, however many times they sign in.
func TestPG_LoginIdentity_OneRowPerSignedInIdentity(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	oid := uuid.NewString()

	entra := store.LoginIdentity{Principal: "pairwise-" + oid, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "Pat@Corp.Example"}
	other := store.LoginIdentity{Principal: "dex-sub-1", Issuer: "https://dex.example", Email: "sam@corp.example"}
	for i := 0; i < 2; i++ {
		for _, in := range []store.LoginIdentity{entra, other} {
			if _, err := st.UpsertLoginIdentity(ctx, in, now.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatalf("sign-in %d of %s: %v", i, in.Principal, err)
			}
		}
	}
	if n := identityCount(t, pool); n != 2 {
		t.Fatalf("identity rows = %d, want 2", n)
	}

	got, err := st.GetIdentityByObject(ctx, idIssuer, idTenant, oid)
	if err != nil {
		t.Fatalf("by object: %v", err)
	}
	if got.Principal != entra.Principal || got.EmailLower != "pat@corp.example" || got.LastLoginAt == nil || got.DeactivatedAt != nil || got.AuthorityEpoch != 0 {
		t.Errorf("entra identity = %+v", got)
	}
	if a := identityAliases(t, pool, got.ID); !slices.Equal(a, []string{"pat@corp.example"}) {
		t.Errorf("entra aliases = %v, want [pat@corp.example]", a)
	}

	byPrincipal, err := st.IdentitiesByPrincipal(ctx, other.Principal)
	if err != nil || len(byPrincipal) != 1 {
		t.Fatalf("by principal = %v, %v; want one row", byPrincipal, err)
	}
	if o := byPrincipal[0]; o.Issuer != other.Issuer || o.TenantID != "" || o.ObjectID != "" || o.EmailLower != "sam@corp.example" {
		t.Errorf("other identity = %+v", o)
	}
	if a := identityAliases(t, pool, byPrincipal[0].ID); !slices.Equal(a, []string{"sam@corp.example"}) {
		t.Errorf("other aliases = %v, want [sam@corp.example]", a)
	}
	if _, err := st.GetIdentityByObject(ctx, other.Issuer, "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("by an empty object id: err = %v, want ErrNotFound", err)
	}
}

// A sign-in under a changed email adds an alias and keeps the old one.
func TestPG_LoginIdentity_EmailChangeKeepsOldAlias(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	in := store.LoginIdentity{Principal: "pairwise-" + oid, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "old@corp.example"}
	first, err := st.UpsertLoginIdentity(ctx, in, time.Now().UTC())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	in.Email = "new@corp.example"
	second, err := st.UpsertLoginIdentity(ctx, in, time.Now().UTC())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ID != first.ID || second.EmailLower != "new@corp.example" {
		t.Errorf("second = %+v, want the same row now showing new@corp.example", second)
	}
	if a := identityAliases(t, pool, first.ID); !slices.Equal(a, []string{"new@corp.example", "old@corp.example"}) {
		t.Errorf("aliases = %v, want both addresses", a)
	}
	// A token with no email leaves the row's email and aliases alone.
	in.Email = ""
	third, err := st.UpsertLoginIdentity(ctx, in, time.Now().UTC())
	if err != nil || third.EmailLower != "new@corp.example" {
		t.Errorf("third = %+v, %v; want the email kept", third, err)
	}
	if n := identityCount(t, pool); n != 1 {
		t.Errorf("identity rows = %d, want 1", n)
	}
}

// A bound row's issuer, tenant, object id and principal never change; a sign-in that tries only
// adds rows or is refused, and the original row reads back as it was.
func TestPG_LoginIdentity_BindingIsImmutable(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	in := store.LoginIdentity{Principal: "pairwise-" + oid, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "pat@corp.example"}
	orig, err := st.UpsertLoginIdentity(ctx, in, time.Now().UTC())
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Same object id, another principal: refused, and the row keeps its principal.
	moved := in
	moved.Principal = "another-sub"
	if _, err := st.UpsertLoginIdentity(ctx, moved, time.Now().UTC()); !errors.Is(err, store.ErrIdentityBindingMismatch) {
		t.Errorf("another principal on the object id: %v; want ErrIdentityBindingMismatch", err)
	}
	// Same principal, another object id: the principal is taken, so nothing is written.
	elsewhere := in
	elsewhere.ObjectID = uuid.NewString()
	if _, err := st.UpsertLoginIdentity(ctx, elsewhere, time.Now().UTC()); err == nil {
		t.Error("the same principal under another object id was accepted")
	}

	got, err := st.GetIdentityByObject(ctx, idIssuer, idTenant, oid)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.ID != orig.ID || got.Principal != orig.Principal || got.Issuer != orig.Issuer || got.TenantID != orig.TenantID || got.ObjectID != orig.ObjectID {
		t.Errorf("binding changed: %+v, was %+v", got, orig)
	}
	if n := identityCount(t, pool); n != 1 {
		t.Errorf("identity rows = %d, want 1", n)
	}
}

// A row written before the person's first sign-in has no principal; the first sign-in binds it
// once, and a later one cannot move it.
func TestPG_LoginIdentity_FirstSignInBindsAnUnboundRow(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	oid := uuid.NewString()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO principal_identities (issuer, tenant_id, object_id) VALUES ($1, $2, $3) RETURNING id`, idIssuer, idTenant, oid).Scan(&id); err != nil {
		t.Fatalf("seed unbound row: %v", err)
	}
	in := store.LoginIdentity{Principal: "pairwise-" + oid, Issuer: idIssuer, TenantID: idTenant, ObjectID: oid, Email: "pat@corp.example"}
	got, err := st.UpsertLoginIdentity(ctx, in, time.Now().UTC())
	if err != nil || got.ID != id || got.Principal != in.Principal {
		t.Fatalf("first sign-in: %+v, %v; want the seeded row bound to %s", got, err, in.Principal)
	}
	in.Principal = "another-sub"
	if _, err = st.UpsertLoginIdentity(ctx, in, time.Now().UTC()); !errors.Is(err, store.ErrIdentityBindingMismatch) {
		t.Errorf("second sign-in under another principal: %v; want ErrIdentityBindingMismatch", err)
	}
	if got, err = st.GetIdentityByObject(ctx, idIssuer, idTenant, oid); err != nil || got.Principal != "pairwise-"+oid {
		t.Errorf("after the refused sign-in: %+v, %v; want the first binding kept", got, err)
	}
	if n := identityCount(t, pool); n != 1 {
		t.Errorf("identity rows = %d, want 1", n)
	}
}

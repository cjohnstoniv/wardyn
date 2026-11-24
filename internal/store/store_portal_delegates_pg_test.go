// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A portal written inside a transaction that rolls back leaves no row, and one that commits leaves its
// issuer fields as given.
func TestCreateDelegateQ_RollsBackWithDecision_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	bound, err := st.BindPortalIssuerQ(ctx, pool, "https://idp.example.com", "https://internal.example.com", "uid", "test")
	if err != nil {
		t.Fatal(err)
	}
	d := types.Delegate{
		ID: uuid.New(), Name: "rolled-back", IdPClientID: "c", RegisteredBy: "test",
		Issuer: bound.Issuer, InternalIssuer: bound.InternalIssuerNorm, IssuerKey: bound.IssuerKey,
		TokenProfile: "access_token", UserType: "default", PortalManagedAdmission: true,
		MaxSubjectTokenAgeSeconds: 300, GrantTTLSeconds: 300,
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDelegateQ(ctx, tx, d, sha256Hex("wdi_a")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM delegates WHERE id = $1`, d.ID); n != 0 {
		t.Fatalf("a rolled-back registration left %d rows", n)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.CreateDelegateQ(ctx, tx, d, sha256Hex("wdi_a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got.Issuer != d.Issuer || got.IssuerKey != d.IssuerKey || got.GrantTTLSeconds != 300 || !got.PortalManagedAdmission || got.Group != "" {
		t.Fatalf("stored portal = %+v", got)
	}
	if _, err := st.CreateDelegateQ(ctx, pool, types.Delegate{ID: uuid.New(), Name: "again", IdPClientID: "c", Issuer: d.Issuer,
		InternalIssuer: d.InternalIssuer, IssuerKey: d.IssuerKey, TokenProfile: "access_token", UserType: "default",
		PortalManagedAdmission: true, MaxSubjectTokenAgeSeconds: 300, GrantTTLSeconds: 300}, sha256Hex("wdi_a")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a reused credential digest = %v, want ErrConflict", err)
	}
}

// Each credential prefix resolves only the kind of portal it belongs to: wdi_ an issuer portal, anything
// else a console portal, in both directions.
func TestDelegateLookups_PrefixBoundToKind_PG(t *testing.T) {
	st, _ := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	if got, err := st.GetIssuerDelegateByRaw(ctx, p.Credential); err != nil || got.ID != p.Delegate.ID {
		t.Fatalf("issuer lookup of an issuer portal = %v, %v", got.ID, err)
	}
	if _, err := st.GetDelegateByRaw(ctx, p.Credential); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("console lookup of an issuer portal credential = %v, want ErrNotFound", err)
	}
	consoleRaw := "wdp_" + uuid.NewString()
	if _, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "console2", IdPClientID: "c", Group: "g"}, consoleRaw); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetDelegateByRaw(ctx, consoleRaw); err != nil || got.Issuer != "" {
		t.Fatalf("console lookup of a console credential = %v, %v", got.ID, err)
	}
	if _, err := st.GetIssuerDelegateByRaw(ctx, consoleRaw); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("issuer lookup of a console credential = %v, want ErrNotFound", err)
	}
	// A secret with another prefix never reaches the issuer lane.
	if _, err := st.GetIssuerDelegateByRaw(ctx, "wdg_"+uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("issuer lookup with another prefix = %v, want ErrNotFound", err)
	}
}

// A delegated token never resolves through an issuer portal's row, by secret or by id.
func TestGetDelegatedTokenByRaw_RefusesIssuerPortalRow_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p := newIssuerPortal(t, st, "")
	now := time.Now().UTC()
	tok := types.DelegatedToken{ID: uuid.New(), DelegateID: p.Delegate.ID, Principal: "delegate:person:x:y", UserType: "default",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	raw := "wdg_" + uuid.NewString()
	if _, err := st.MintDelegatedToken(ctx, tok, raw, now); err != nil {
		t.Fatalf("mint onto the portal row: %v", err)
	}
	if _, err := st.GetDelegatedTokenByRaw(ctx, raw, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("by raw through an issuer portal = %v, want ErrNotFound", err)
	}
	if _, err := st.GetDelegatedTokenByID(ctx, tok.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("by id through an issuer portal = %v, want ErrNotFound", err)
	}

	// The same token through a console portal still resolves.
	console, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "console", IdPClientID: "c", Group: "g"}, "wdp_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ctok := tok
	ctok.ID, ctok.DelegateID = uuid.New(), console.ID
	craw := "wdg_" + uuid.NewString()
	if _, err := st.MintDelegatedToken(ctx, ctok, craw, now); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetDelegatedTokenByRaw(ctx, craw, now); err != nil || got.ID != ctok.ID {
		t.Fatalf("by raw through a console portal = %v, %v", got.ID, err)
	}
	_ = pool
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// issuerPortal is one bound issuer and a live portal registered against it.
type issuerPortal struct {
	Issuer     string
	Key        string
	Delegate   types.Delegate
	Credential string
}

// newIssuerPortal binds issuer (a fresh spelling when empty) and registers a managed-admission portal on it.
func newIssuerPortal(t *testing.T, st store.PG, issuer string) issuerPortal {
	t.Helper()
	ctx := context.Background()
	if issuer == "" {
		issuer = "https://idp-" + uuid.NewString() + ".example.com"
	}
	bound, err := st.BindPortalIssuerQ(ctx, st.Pool, issuer, "https://internal-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatalf("bind %s: %v", issuer, err)
	}
	return registerPortalOn(t, st, bound)
}

// registerPortalOn registers one more portal on an issuer that is already bound.
func registerPortalOn(t *testing.T, st store.PG, bound types.PortalIssuer) issuerPortal {
	t.Helper()
	cred := store.PortalCredentialPrefix + uuid.NewString()
	d, err := st.CreateDelegateQ(context.Background(), st.Pool, types.Delegate{
		ID: uuid.New(), Name: "portal-" + uuid.NewString(), IdPClientID: uuid.NewString(), RegisteredBy: "test",
		Issuer: bound.Issuer, InternalIssuer: bound.InternalIssuerNorm, IssuerKey: bound.IssuerKey,
		TokenProfile: "access_token", UserType: "default", PortalManagedAdmission: true,
		MaxSubjectTokenAgeSeconds: 600, GrantTTLSeconds: 600,
	}, sha256Hex(cred))
	if err != nil {
		t.Fatalf("register a portal on %s: %v", bound.Issuer, err)
	}
	return issuerPortal{Issuer: bound.Issuer, Key: bound.IssuerKey, Delegate: d, Credential: cred}
}

func isolatedStore(t *testing.T) (store.PG, *pgxpool.Pool) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	return store.NewPG(pool), pool
}

func rowCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

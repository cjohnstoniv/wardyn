// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A sponsor handed to B and back to A is a new sponsorship: a stream opened under the first one is no
// longer sponsored (sponsor_since moved past its open time), though the sponsor's name reads the same.
// Guarded by WARDYN_TEST_PG.
func TestPG_ServiceStillSponsored_ASponsorHandedAwayAndBackCutsAnEarlierStream(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	const a, b = "sub-sponsor-a", "sub-sponsor-b"
	for _, p := range []string{a, b} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://issuer.test", Email: p + "@corp.example"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-bot", Sponsor: a, UserType: types.UserTypeStandard, CreatedBy: a}); err != nil {
		t.Fatal(err)
	}
	opened := time.Now().UTC()
	if ok, err := st.ServiceStillSponsored(ctx, "ci-bot", a, opened); err != nil || !ok {
		t.Fatalf("before any change = %v, %v; want still sponsored", ok, err)
	}
	for _, to := range []string{b, a} {
		if _, err := st.UpdateServiceIdentity(ctx, "ci-bot", store.ServiceIdentityChange{Sponsor: &to, By: "admin"}); err != nil {
			t.Fatalf("hand to %s: %v", to, err)
		}
	}
	if ok, err := st.ServiceStillSponsored(ctx, "ci-bot", a, opened); err != nil || ok {
		t.Errorf("stream opened before the A to B to A hand-over = %v, %v; want it cut", ok, err)
	}
	if ok, err := st.ServiceStillSponsored(ctx, "ci-bot", a, time.Now().UTC()); err != nil || !ok {
		t.Errorf("stream opened after the hand-over = %v, %v; want still sponsored", ok, err)
	}
}

// A service identity's own write (IdentityGuard.ServiceSince) holds the identity's row FOR SHARE while it
// runs, so a retirement, which takes the row FOR UPDATE, cannot pass it; and the write is refused once the
// identity is suspended or retired or its sponsor was handed on after the sponsor_since it was admitted
// against, though the sponsor's name reads the same again. Guarded by WARDYN_TEST_PG.
func TestPG_ServiceWriteHeldToItsAdmission(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	const a, b = "sub-sponsor-a", "sub-sponsor-b"
	for _, p := range []string{a, b} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://issuer.test", Email: p + "@corp.example"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	si, err := st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-bot", Sponsor: a, UserType: types.UserTypeStandard, CreatedBy: a})
	if err != nil {
		t.Fatal(err)
	}
	g := store.IdentityGuard{Principal: si.Principal(), Epoch: -1, ServiceSince: si.SponsorSince}
	write := func() error { return st.WithIdentityShared(ctx, g, func() error { return nil }) }
	held := st.WithIdentityShared(ctx, g, func() error {
		_, err := pool.Exec(ctx, `SELECT 1 FROM service_identities WHERE name = 'ci-bot' FOR UPDATE NOWAIT`)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(held, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("taking the identity's row FOR UPDATE while its write runs = %v, want lock_not_available (55P03)", held)
	}
	change := func(ch store.ServiceIdentityChange) {
		t.Helper()
		ch.By = "admin"
		if _, err := st.UpdateServiceIdentity(ctx, "ci-bot", ch); err != nil {
			t.Fatal(err)
		}
	}
	yes, no := true, false
	change(store.ServiceIdentityChange{Suspended: &yes})
	if err := write(); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a write after a suspension = %v, want ErrIdentityDeactivated", err)
	}
	change(store.ServiceIdentityChange{Suspended: &no})
	if err := write(); err != nil {
		t.Errorf("a write after the suspension ended = %v, want it admitted", err)
	}
	for _, to := range []string{b, a} {
		change(store.ServiceIdentityChange{Sponsor: &to})
	}
	if err := write(); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a write admitted before the sponsor was handed to B and back = %v, want ErrIdentityDeactivated", err)
	}
	cur, err := st.RetireServiceIdentity(ctx, "ci-bot", "admin")
	if err != nil {
		t.Fatal(err)
	}
	g.ServiceSince = cur.After.SponsorSince
	if err := write(); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a write after the retirement = %v, want ErrIdentityDeactivated", err)
	}
}

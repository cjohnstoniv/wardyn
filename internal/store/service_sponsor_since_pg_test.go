// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

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

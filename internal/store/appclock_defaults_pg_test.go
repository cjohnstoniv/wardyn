// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// Omitted token/run admission times retain their DB-now defaults. API-token
// lifetimes use the DB clock; delegated expiry remains an app-clock instant.
func TestPG_AppClockDefaultsAndExpiryClocks(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	appNow := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	st.Now = func() time.Time { return appNow }
	var databaseAt time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseAt); err != nil {
		t.Fatal(err)
	}
	assertDefault := func(at time.Time) {
		t.Helper()
		if delta := at.Sub(databaseAt); delta < -5*time.Second || delta > 5*time.Second {
			t.Fatalf("zero admission lost DB-now default: got=%s database=%s", at, databaseAt)
		}
	}
	for _, lifetime := range []time.Duration{0, time.Hour, -time.Minute} {
		t.Run("api token "+lifetime.String(), func(t *testing.T) {
			var expires *time.Time
			if lifetime != 0 {
				at := appNow.Add(lifetime)
				expires = &at
			}
			raw := "wdn_" + uuid.NewString()
			got, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: "clock-default", Role: "user", UserType: types.UserTypeStandard, Name: "clock", ExpiresAt: expires}, raw)
			if err != nil {
				t.Fatal(err)
			}
			assertDefault(got.CreatedAt)
			if lifetime == 0 {
				if got.ExpiresAt != nil {
					t.Fatal("nil expiry became a deadline")
				}
			} else if got.ExpiresAt == nil || got.ExpiresAt.Sub(got.CreatedAt) != lifetime {
				t.Fatalf("API-token remaining lifetime changed: %+v expected=%s", got, lifetime)
			}
			_, err = st.GetAPITokenByRaw(ctx, raw)
			if lifetime < 0 {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("expired token authenticated: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, capped := range []bool{false, true} {
		run := newRun(types.RunPending)
		run.UpdatedAt = time.Time{}
		var got types.AgentRun
		var err error
		if capped {
			got, err = st.CreateRunUnderCap(ctx, run, 10)
		} else {
			got, err = st.CreateRun(ctx, run)
		}
		if err != nil {
			t.Fatal(err)
		}
		assertDefault(got.UpdatedAt)
	}
	d, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "clock-default", IdPClientID: uuid.NewString(), Group: "g"}, "wdp_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	raw := "wdg_" + uuid.NewString()
	expires := appNow.Add(time.Hour)
	delegated, err := st.MintDelegatedToken(ctx, types.DelegatedToken{ID: uuid.New(), DelegateID: d.ID, Principal: "clock-default", UserType: types.UserTypeStandard, CreatedAt: appNow, ExpiresAt: expires}, raw, appNow)
	if err != nil {
		t.Fatal(err)
	}
	if !delegated.ExpiresAt.Equal(expires) {
		t.Fatalf("delegated app-clock expiry translated: got=%s expected=%s", delegated.ExpiresAt, expires)
	}
	if _, err := st.GetDelegatedTokenByRaw(ctx, raw, expires.Add(-time.Microsecond)); err != nil {
		t.Fatalf("delegated grant expired early: %v", err)
	}
	if _, err := st.GetDelegatedTokenByRaw(ctx, raw, expires); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delegated grant outlived app-clock expiry: %v", err)
	}
}

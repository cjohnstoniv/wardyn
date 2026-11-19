// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func newCutPortal(t *testing.T, st store.PG) types.Delegate {
	t.Helper()
	d, err := st.CreateDelegate(context.Background(), types.Delegate{ID: uuid.New(), Name: "portal-" + uuid.NewString()[:8],
		IdPClientID: "client-" + uuid.NewString()[:8], Group: "g"}, "wdp_"+uuid.NewString())
	if err != nil {
		t.Fatalf("create delegate: %v", err)
	}
	return d
}

// A cut revoke stamps both columns from one clock value and counts the portal's non-terminal runs
// only; a plain revoke stamps no cut; a cut of a revoked portal stamps the cut alone; every repeat is
// ErrNotFound.
func TestPG_RevokeDelegateCut(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	d := newCutPortal(t, st)
	for _, state := range []types.RunState{types.RunRunning, types.RunPending, types.RunCompleted} {
		run := newRun(state)
		run.CreatedVia = &d.ID
		persistRun(t, ctx, pool, run)
	}
	got, n, err := st.RevokeDelegate(ctx, d.ID, now, true)
	if err != nil || got.RevokedAt == nil || got.CutInFlightAt == nil || !got.RevokedAt.Equal(*got.CutInFlightAt) {
		t.Fatalf("cut revoke = %+v err=%v, want revoked_at and cut_in_flight_at both stamped", got, err)
	}
	if n != 2 {
		t.Errorf("runs in flight = %d, want 2 (running and pending, not the completed one)", n)
	}
	if read, err := st.GetDelegateByID(ctx, d.ID); err != nil || read.CutInFlightAt == nil {
		t.Errorf("read back = %+v err=%v, want cut_in_flight_at", read, err)
	}
	for _, cut := range []bool{true, false} {
		if _, _, err := st.RevokeDelegate(ctx, d.ID, now, cut); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("repeat cut=%v: err=%v, want ErrNotFound", cut, err)
		}
	}

	plain := newCutPortal(t, st)
	got, _, err = st.RevokeDelegate(ctx, plain.ID, now, false)
	if err != nil || got.RevokedAt == nil || got.CutInFlightAt != nil {
		t.Fatalf("plain revoke = %+v err=%v, want revoked and not cut", got, err)
	}
	later := now.Add(time.Minute)
	got, _, err = st.RevokeDelegate(ctx, plain.ID, later, true)
	if err != nil || got.CutInFlightAt == nil || got.RevokedAt.Equal(*got.CutInFlightAt) {
		t.Fatalf("cut after revoke = %+v err=%v, want the cut stamped and revoked_at kept", got, err)
	}
	if _, _, err := st.RevokeDelegate(ctx, uuid.New(), now, true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown portal: err=%v, want ErrNotFound", err)
	}
}

// A revoke and a cut racing on one portal both land exactly once: whichever order the row lock
// gives, the portal ends revoked and cut, and no call other than those two succeeds.
func TestPG_RevokeDelegateConcurrentRevokeAndCut(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	for range 10 {
		d := newCutPortal(t, st)
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i, cut := range []bool{false, true, false, true} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = st.RevokeDelegate(ctx, d.ID, time.Now().UTC(), cut)
			}()
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, store.ErrNotFound):
				t.Fatalf("revoke: %v", err)
			}
		}
		read, err := st.GetDelegateByID(ctx, d.ID)
		if err != nil || read.RevokedAt == nil || read.CutInFlightAt == nil {
			t.Fatalf("after the race = %+v err=%v, want revoked and cut", read, err)
		}
		// A cut first leaves nothing for the rest (1); a plain revoke first leaves one cut (2).
		if ok != 1 && ok != 2 {
			t.Fatalf("%d calls succeeded, want 1 or 2", ok)
		}
	}
}

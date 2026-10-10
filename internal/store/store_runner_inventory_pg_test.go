// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mintToken(t *testing.T, st store.PG, owner string, created, expires time.Time) (string, types.RunnerRegistrationToken) {
	t.Helper()
	raw := "wdr_" + uuid.NewString()
	got, err := st.MintRunnerRegistrationToken(context.Background(), raw, types.RunnerRegistrationToken{
		ID: uuid.New(), Owner: owner, MintedBy: "admin", OrgURLSHA256: "org", CreatedAt: created, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	return raw, got
}

func TestPG_RunnerTokenRevokeRacesRedemptionExactlyOneWins(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	for range 20 {
		now := time.Now().UTC()
		raw, token := mintToken(t, st, "alice", now, now.Add(time.Hour))
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, token.ID)
		})
		var redeemed, revoked atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, raw, "org", now); err != nil {
				t.Errorf("consume: %v", err)
			} else if ok {
				redeemed.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := st.RevokeRunnerRegistrationToken(ctx, token.ID, now); err == nil {
				revoked.Add(1)
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("revoke: %v", err)
			}
		}()
		close(start)
		wg.Wait()
		if redeemed.Load()+revoked.Load() != 1 {
			t.Fatalf("redeemed=%d revoked=%d: a token must be spent by exactly one of them", redeemed.Load(), revoked.Load())
		}
	}
}

func TestPG_RunnerTokenRevokedCannotBeRedeemedOrRevokedAgain(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	raw, token := mintToken(t, st, "alice", now, now.Add(time.Hour))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, token.ID)
	})
	if _, err := st.RevokeRunnerRegistrationToken(ctx, token.ID, now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, raw, "org", now); err != nil || ok {
		t.Fatalf("revoked token redeemed: ok=%v err=%v", ok, err)
	}
	if _, err := st.RevokeRunnerRegistrationToken(ctx, token.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revoke = %v, want ErrNotFound", err)
	}
}

func TestPG_RunnerTokenListIsOnlyUnusedUnexpired(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	_, live := mintToken(t, st, "alice", now.Add(-time.Minute), now.Add(time.Hour))
	usedRaw, used := mintToken(t, st, "bob", now.Add(-time.Minute), now.Add(time.Hour))
	_, expired := mintToken(t, st, "cara", now.Add(-2*time.Hour), now.Add(-time.Hour))
	_, revoked := mintToken(t, st, "dan", now.Add(-time.Minute), now.Add(time.Hour))
	for _, tok := range []types.RunnerRegistrationToken{live, used, expired, revoked} {
		id := tok.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, id) })
	}
	if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, usedRaw, "org", now); err != nil || !ok {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if _, err := st.RevokeRunnerRegistrationToken(ctx, revoked.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListUnusedRunnerRegistrationTokens(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, tok := range got {
		if tok.Token != "" {
			t.Fatalf("listed token carries a raw value: %+v", tok)
		}
		switch tok.ID {
		case live.ID, used.ID, expired.ID, revoked.ID:
			ids = append(ids, tok.ID)
		}
	}
	if len(ids) != 1 || ids[0] != live.ID {
		t.Fatalf("listed %v, want only the live token %v", ids, live.ID)
	}
}

func TestPG_RunnerMintedByAndActiveRunCounts(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	r := newRunner("alice@example.com")
	r.MintedBy = "admin@example.com"
	got, err := st.CreateRunner(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	if got.MintedBy != r.MintedBy {
		t.Fatalf("minted_by = %q, want %q", got.MintedBy, r.MintedBy)
	}
	for _, state := range []types.RunState{types.RunRunning, types.RunWaiting, types.RunCompleted} {
		run := newRun(state)
		run.Placement = types.PlacementLocal
		run.PlacementFilled = true
		run.RunnerID = &r.ID
		if _, err := st.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		id := run.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	}
	counts, err := st.CountActiveRunsByRunner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[r.ID] != 2 {
		t.Fatalf("active runs = %d, want 2 (running and waiting, not completed)", counts[r.ID])
	}
}

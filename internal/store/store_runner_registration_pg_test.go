// Copyright 2025 The Wardyn Authors
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

func TestPG_RunnerRegistrationSingleUseUnderConcurrency(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	raw := "wdr_" + uuid.NewString()
	want := types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", MintedBy: "admin", OrgURLSHA256: "org", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	got, err := st.MintRunnerRegistrationToken(ctx, raw, want)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, want.ID)
	})
	if got.Token != "" || got.TokenSHA256 == raw || got.Owner != want.Owner || got.OrgURLSHA256 != want.OrgURLSHA256 {
		t.Fatal("token storage lost ownership/binding or retained plaintext")
	}
	if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, raw, "other-org", now); err != nil || ok {
		t.Fatalf("foreign org consume: %v %v", ok, err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			token, ok, err := st.ConsumeRunnerRegistrationToken(ctx, raw, "org", now)
			if err != nil {
				t.Errorf("consume: %v", err)
			}
			if ok {
				wins.Add(1)
				if token.Owner != want.Owner {
					t.Error("owner changed")
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners, want 1", wins.Load())
	}
}

func TestPG_RunnerRegistrationExpiryAndUnknownShareRefusal(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	token := types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", MintedBy: "alice", OrgURLSHA256: "org", CreatedAt: now.Add(-time.Hour), ExpiresAt: now}
	raw := "wdr_" + uuid.NewString()
	if _, err := st.MintRunnerRegistrationToken(ctx, raw, token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, token.ID)
	})
	for _, candidate := range []string{raw, "unknown"} {
		if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, candidate, "org", now); err != nil || ok {
			t.Fatalf("invalid token accepted: %v %v", ok, err)
		}
	}
}

func TestPG_RunnerClaimExpiresAtTwentyFourHoursWithoutSweep(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	r, err := st.CreateRunner(ctx, newRunner("alice"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	if _, err := pool.Exec(ctx, `UPDATE runners SET created_at=$2 WHERE id=$1`, r.ID, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimRunner(ctx, r.ID, r.Owner, r.KeyFingerprint, now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("expired unclaimed runner claim: %v, want ErrRunnerClaimMismatch", err)
	}
	got, err := st.GetRunner(ctx, r.ID)
	if err != nil || got.State != types.RunnerUnclaimed {
		t.Fatalf("refused claim mutated runner: %v", err)
	}
}

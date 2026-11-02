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

// A prewrapped row's registration check (0237): only the current, unexpired attempt publishes a verdict, a
// retry starts only from refused or expired, and a database time read AFTER the row lock decides expiry.

const prewrappedRef = "ghcr.io/acme/sdk-agent:2.3@sha256:4f1c2a7e0d9b8c6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b9be0"

func mkPrewrapped(t *testing.T, s store.PG, ref string, attempt uuid.UUID) types.BaseImageEntry {
	t.Helper()
	now := time.Now().UTC()
	b, err := s.UpsertBaseImage(context.Background(), types.BaseImageEntry{
		ID: uuid.New(), Kind: types.BaseImageKindPrewrapped, Name: "sdk-agent", Image: ref,
		CreatedAt: now, UpdatedAt: now, Check: &types.BaseImageCheck{Attempt: attempt},
	})
	if err != nil {
		t.Fatalf("upsert prewrapped: %v", err)
	}
	return b
}

// expire moves the row's deadline into the past, as a crashed or slow attempt would leave it.
func expire(t *testing.T, s store.PG, id uuid.UUID) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(),
		`UPDATE base_images SET check_deadline = clock_timestamp() - interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatalf("expire: %v", err)
	}
}

func TestPG_ImageCheck_FirstAttemptAndIdentityHit(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	a := uuid.New()
	b := mkPrewrapped(t, s, prewrappedRef, a)
	if b.Check == nil || b.Check.Status != types.BaseImageCheckChecking || b.Check.Attempt != a {
		t.Fatalf("new row check = %+v, want checking with attempt %s", b.Check, a)
	}
	if left := time.Until(b.Check.Deadline); left < store.PrewrappedCheckWindow-time.Minute || left > store.PrewrappedCheckWindow+time.Minute {
		t.Fatalf("deadline %v from now, want about %v", left, store.PrewrappedCheckWindow)
	}
	// Same identity again with another attempt: the existing row and its check come back unchanged.
	again := mkPrewrapped(t, s, prewrappedRef, uuid.New())
	if again.ID != b.ID || again.Check.Attempt != a {
		t.Fatalf("identity hit = %s attempt %s, want %s attempt %s", again.ID, again.Check.Attempt, b.ID, a)
	}
	// Other kinds carry no check.
	reg, err := s.UpsertBaseImage(ctx, types.BaseImageEntry{ID: uuid.New(), Kind: "registry", Name: "x", Image: "ghcr.io/acme/x:1",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil || reg.Check != nil {
		t.Fatalf("registry row = %+v, %v; want no check", reg.Check, err)
	}
	got, err := s.GetBaseImageByRef(ctx, types.BaseImageKindPrewrapped, prewrappedRef)
	if err != nil || got.ID != b.ID {
		t.Fatalf("GetBaseImageByRef = %s, %v; want %s", got.ID, err, b.ID)
	}
	if _, err := s.GetBaseImageByRef(ctx, types.BaseImageKindPrewrapped, "ghcr.io/acme/other@sha256:00"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBaseImageByRef(unknown) = %v, want ErrNotFound", err)
	}
}

func TestPG_ImageCheck_OnlyTheCurrentAttemptPublishes(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	a := uuid.New()
	b := mkPrewrapped(t, s, prewrappedRef, a)

	if _, err := s.FinishImageCheck(ctx, b.ID, b.Image, uuid.New(), types.BaseImageCheckReady, "", []string{"amd64"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("finish with another attempt = %v, want ErrConflict", err)
	}
	if _, err := s.FinishImageCheck(ctx, b.ID, "ghcr.io/acme/other@sha256:00", a, types.BaseImageCheckReady, "", []string{"amd64"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("finish naming another image = %v, want ErrConflict", err)
	}
	done, err := s.FinishImageCheck(ctx, b.ID, b.Image, a, types.BaseImageCheckReady, "", []string{"amd64", "arm64"})
	if err != nil || done.Check.Status != types.BaseImageCheckReady || len(done.Check.AcceptedArches) != 2 || done.Check.CheckedAt == nil {
		t.Fatalf("finish = %+v, %v; want ready on two architectures", done.Check, err)
	}
	// A second publish of the same attempt changes nothing: the row is no longer checking.
	if _, err := s.FinishImageCheck(ctx, b.ID, b.Image, a, types.BaseImageCheckRefused, "timed_out:arm64", nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second finish = %v, want ErrConflict", err)
	}
	// Ready is never retried.
	if _, err := s.ClaimImageCheck(ctx, b.ID, uuid.New()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claim on ready = %v, want ErrConflict", err)
	}
	// The CHECK refuses ready with no accepted architecture.
	if _, err := s.Pool.Exec(ctx, `UPDATE base_images SET accepted_arches='{}' WHERE id=$1`, b.ID); err == nil {
		t.Fatal("ready with no accepted architecture was stored; 0237's CHECK should refuse it")
	}
}

func TestPG_ImageCheck_RetryFromRefusedOrExpiredOnly(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	a := uuid.New()
	b := mkPrewrapped(t, s, prewrappedRef, a)

	// A live attempt cannot be replaced.
	if _, err := s.ClaimImageCheck(ctx, b.ID, uuid.New()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claim over a live attempt = %v, want ErrConflict", err)
	}
	if _, err := s.FinishImageCheck(ctx, b.ID, b.Image, a, types.BaseImageCheckRefused, "contract_missing:wardyn-rec", nil); err != nil {
		t.Fatalf("refuse: %v", err)
	}
	c := uuid.New()
	claimed, err := s.ClaimImageCheck(ctx, b.ID, c)
	if err != nil || claimed.Check.Status != types.BaseImageCheckChecking || claimed.Check.Attempt != c || claimed.Check.Reason != "" {
		t.Fatalf("claim from refused = %+v, %v; want checking with attempt %s and no reason", claimed.Check, err, c)
	}
	// The old attempt's late verdict touches nothing.
	if _, err := s.FinishImageCheck(ctx, b.ID, b.Image, a, types.BaseImageCheckReady, "", []string{"amd64"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale finish after retry = %v, want ErrConflict", err)
	}
	// An expired attempt publishes nothing, even with no retry, and can then be claimed.
	expire(t, s, b.ID)
	if _, err := s.FinishImageCheck(ctx, b.ID, b.Image, c, types.BaseImageCheckReady, "", []string{"amd64"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("finish after the deadline = %v, want ErrConflict", err)
	}
	if _, err := s.ClaimImageCheck(ctx, b.ID, uuid.New()); err != nil {
		t.Fatalf("claim after the deadline: %v", err)
	}
	// A registry row is never claimed.
	reg, _ := s.UpsertBaseImage(ctx, types.BaseImageEntry{ID: uuid.New(), Kind: "registry", Name: "x", Image: "ghcr.io/acme/x:1",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if _, err := s.ClaimImageCheck(ctx, reg.ID, uuid.New()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claim on a registry row = %v, want ErrConflict", err)
	}
}

// TestPG_ImageCheck_ConcurrentRetriesStartOne: two "Check again" calls on an expired row; exactly one claims.
func TestPG_ImageCheck_ConcurrentRetriesStartOne(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	b := mkPrewrapped(t, s, prewrappedRef, uuid.New())
	expire(t, s, b.ID)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.ClaimImageCheck(ctx, b.ID, uuid.New())
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, store.ErrConflict):
			t.Fatalf("claim: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent claims succeeded, want exactly 1", won)
	}
}

// TestPG_ImageCheck_LockWaitPastTheDeadlineDecidesExpired: a verdict that waits on the row lock until after the
// deadline is refused, because expiry is decided on the database's time read after the lock is held (now()
// would be the transaction's start, before the deadline).
func TestPG_ImageCheck_LockWaitPastTheDeadlineDecidesExpired(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	a := uuid.New()
	b := mkPrewrapped(t, s, prewrappedRef, a)
	if _, err := s.Pool.Exec(ctx, `UPDATE base_images SET check_deadline = clock_timestamp() + interval '1 second' WHERE id=$1`, b.ID); err != nil {
		t.Fatal(err)
	}
	holder, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT 1 FROM base_images WHERE id=$1 FOR UPDATE`, b.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.FinishImageCheck(ctx, b.ID, b.Image, a, types.BaseImageCheckReady, "", []string{"amd64"})
		result <- err
	}()
	time.Sleep(1500 * time.Millisecond) // the verdict is now blocked on the lock, and the deadline has passed
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, store.ErrConflict) {
		t.Fatalf("finish that waited past the deadline = %v, want ErrConflict", err)
	}
}

func TestPG_ImageCheck_LiveAttempts(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	live, gone := uuid.New(), uuid.New()
	mkPrewrapped(t, s, prewrappedRef, live)
	old := mkPrewrapped(t, s, "ghcr.io/acme/old@sha256:"+"0000000000000000000000000000000000000000000000000000000000000000", gone)
	expire(t, s, old.ID)
	got, err := s.LiveImageCheckAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got[live] || got[gone] || len(got) != 1 {
		t.Fatalf("live attempts = %v, want only %s", got, live)
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for the short-lived handoff rows (migration 0026). These
// close the gap the move out of process memory opened: consume-once used to be a
// map delete under a mutex, and is now a DELETE ... RETURNING whose atomicity and
// expiry predicate only a real server can prove.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset. Every case uses fresh
// ids so it is isolated inside the shared database.
// Run: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_AttachTicket_ConsumeOnce(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	now := time.Now().UTC()
	runID := uuid.New()
	tok := uuid.NewString()

	if err := st.MintAttachTicket(ctx, tok, store.AttachTicket{
		RunID: runID, ActorType: types.ActorHuman, Principal: "alice",
	}, now, now.Add(30*time.Second)); err != nil {
		t.Fatalf("mint: %v", err)
	}

	got, ok, err := st.ConsumeAttachTicket(ctx, tok, now)
	if err != nil || !ok {
		t.Fatalf("first consume: ok=%v err=%v", ok, err)
	}
	if got.RunID != runID || got.Principal != "alice" || got.ActorType != types.ActorHuman {
		t.Fatalf("ticket round-trip lost fields: %+v", got)
	}

	// Consume-once: the DELETE ... RETURNING already removed the row.
	if _, ok, err := st.ConsumeAttachTicket(ctx, tok, now); ok || err != nil {
		t.Fatalf("second consume: ok=%v err=%v, want false/nil (single-use)", ok, err)
	}
}

func TestPG_AttachTicket_Expiry(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	now := time.Now().UTC()
	tok := uuid.NewString()

	if err := st.MintAttachTicket(ctx, tok, store.AttachTicket{
		RunID: uuid.New(), ActorType: types.ActorHuman, Principal: "bob",
	}, now, now.Add(30*time.Second)); err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Presented one second past the TTL: the WHERE rejects it.
	if _, ok, err := st.ConsumeAttachTicket(ctx, tok, now.Add(31*time.Second)); ok || err != nil {
		t.Fatalf("expired consume: ok=%v err=%v, want false/nil", ok, err)
	}
	// ... and it is still unredeemable inside the window: expiry is not a
	// one-shot burn, the row is simply dead.
	if _, ok, _ := st.ConsumeAttachTicket(ctx, tok, now.Add(31*time.Second)); ok {
		t.Fatal("expired ticket redeemed on a retry")
	}
}

// TestPG_AttachTicket_MintSweepsExpired: the opportunistic TTL cleanup. A mint
// must clear already-expired rows (there is no background sweeper) without
// touching live ones — including the one it is inserting.
func TestPG_AttachTicket_MintSweepsExpired(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	now := time.Now().UTC()
	dead, live := uuid.NewString(), uuid.NewString()

	if err := st.MintAttachTicket(ctx, dead, store.AttachTicket{RunID: uuid.New(), ActorType: types.ActorHuman, Principal: "carol"},
		now, now.Add(-time.Minute)); err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if err := st.MintAttachTicket(ctx, live, store.AttachTicket{RunID: uuid.New(), ActorType: types.ActorHuman, Principal: "dave"},
		now, now.Add(30*time.Second)); err != nil {
		t.Fatalf("mint live: %v", err)
	}

	// The second mint swept the expired row. Consume it with a clock BEFORE its
	// expiry so only the sweep can explain the miss.
	if _, ok, _ := st.ConsumeAttachTicket(ctx, dead, now.Add(-2*time.Minute)); ok {
		t.Error("expired ticket survived the mint sweep")
	}
	if _, ok, err := st.ConsumeAttachTicket(ctx, live, now); !ok || err != nil {
		t.Errorf("the mint's own ticket was swept: ok=%v err=%v", ok, err)
	}
}

func TestPG_ComposeResult_TakeOnce(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	runID := uuid.New()
	body := []byte(`{"structured_output":{"run":{"agent":"claude-code"}}}`)

	if err := st.PutComposeResult(ctx, runID, body); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.TakeComposeResult(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("take: ok=%v err=%v", ok, err)
	}
	if string(got) != string(body) {
		t.Fatalf("payload round-trip: got %q, want %q", got, body)
	}
	// Delete-on-read: exactly one waiter gets it.
	if _, ok, err := st.TakeComposeResult(ctx, runID); ok || err != nil {
		t.Fatalf("second take: ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestPG_ComposeResult_RawBody: the upload is claude's RAW stdout, which on a
// failed compose is an empty body, a non-JSON wrapper, or (in a crash) binary
// junk with NUL/non-UTF-8 bytes. Storing it must not require it to parse OR to
// be valid text — payload is BYTEA precisely so a crash tail round-trips
// byte-for-byte instead of 500ing the upload (text/jsonb rejects 0x00).
func TestPG_ComposeResult_RawBody(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	for _, body := range []string{"", "claude: fatal error", `{"is_error":true}`,
		"crash\x00tail\xff\xfe not utf-8"} {
		runID := uuid.New()
		if err := st.PutComposeResult(ctx, runID, []byte(body)); err != nil {
			t.Fatalf("put %q: %v", body, err)
		}
		got, ok, err := st.TakeComposeResult(ctx, runID)
		if err != nil || !ok || string(got) != body {
			t.Errorf("raw body %q: got %q ok=%v err=%v", body, got, ok, err)
		}
	}
}

func TestPG_ComposeResult_Discard(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	runID := uuid.New()

	if err := st.PutComposeResult(ctx, runID, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.DiscardComposeResult(ctx, runID); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, ok, _ := st.TakeComposeResult(ctx, runID); ok {
		t.Error("discarded compose result was still takeable")
	}
	// Idempotent: discarding a row that is already gone is not an error.
	if err := st.DiscardComposeResult(ctx, runID); err != nil {
		t.Errorf("discard of a missing row: %v", err)
	}
}

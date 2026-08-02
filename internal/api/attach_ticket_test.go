// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memTicketStore models the attach_tickets rows in memory with the SAME contract
// the SQL has (migration 0026 / store.PG.ConsumeAttachTicket): consume deletes
// the row on ANY redemption attempt for a live token, and expiry is enforced by
// the lookup itself — the run-id binding is the CALLER's check. The SQL is proven
// against Postgres in internal/store/store_ephemeral_pg_test.go; this fake keeps
// the wrapper's rules testable without a DSN.
type memTicketStore struct {
	store.Store
	mu sync.Mutex
	m  map[string]memTicket
}

type memTicket struct {
	t   store.AttachTicket
	exp time.Time
}

func newMemTicketStore() *memTicketStore { return &memTicketStore{m: map[string]memTicket{}} }

func (s *memTicketStore) MintAttachTicket(_ context.Context, token string, t store.AttachTicket, _, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[token] = memTicket{t: t, exp: expiresAt}
	return nil
}

func (s *memTicketStore) ConsumeAttachTicket(_ context.Context, token string, now time.Time) (store.AttachTicket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mt, ok := s.m[token]
	if !ok || !mt.exp.After(now) {
		return store.AttachTicket{}, false, nil
	}
	delete(s.m, token) // single-use: burned on any redemption attempt
	return mt.t, true, nil
}

func TestAttachTicket(t *testing.T) {
	st := newMemTicketStore()
	ctx := context.Background()
	run := uuid.New()
	other := uuid.New()
	now := time.Unix(1_700_000_000, 0)

	tok, err := mintAttachTicket(ctx, st, run, types.ActorHuman, "alice", now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok == "" {
		t.Fatal("mint returned empty ticket")
	}

	// Wrong run id: rejected (and the ticket is burned on the attempt).
	if _, ok, _ := consumeAttachTicket(ctx, st, tok, other, now); ok {
		t.Fatal("ticket redeemed against the wrong run id")
	}
	if _, ok, _ := consumeAttachTicket(ctx, st, tok, run, now); ok {
		t.Fatal("ticket survived a burned redemption attempt (must be single-use)")
	}

	// Fresh ticket: redeems once, carries attribution, then is gone.
	tok2, _ := mintAttachTicket(ctx, st, run, types.ActorHuman, "bob", now)
	ta, ok, err := consumeAttachTicket(ctx, st, tok2, run, now)
	if err != nil || !ok {
		t.Fatalf("valid ticket did not redeem: ok=%v err=%v", ok, err)
	}
	if ta.principal != "bob" || ta.actorType != types.ActorHuman {
		t.Fatalf("attribution lost: got %v/%q", ta.actorType, ta.principal)
	}
	if _, ok, _ := consumeAttachTicket(ctx, st, tok2, run, now); ok {
		t.Fatal("ticket redeemed twice")
	}

	// Expiry: a ticket presented after its TTL is rejected.
	tok3, _ := mintAttachTicket(ctx, st, run, types.ActorHuman, "carol", now)
	if _, ok, _ := consumeAttachTicket(ctx, st, tok3, run, now.Add(attachTicketTTL+time.Second)); ok {
		t.Fatal("expired ticket redeemed")
	}
}

// TestAttachTicketStoreError: a store failure must NOT read as a rejected
// ticket — the caller distinguishes them (500 vs 403), so the error has to
// survive the wrapper instead of collapsing into ok=false.
func TestAttachTicketStoreError(t *testing.T) {
	ta, ok, err := consumeAttachTicket(context.Background(), errTicketStore{}, "tok", uuid.New(), time.Now())
	if err == nil {
		t.Fatal("store failure was swallowed; the caller would 403 a healthy ticket")
	}
	if ok || ta != (ticketActor{}) {
		t.Fatalf("store failure must not authenticate: ok=%v actor=%+v", ok, ta)
	}
}

type errTicketStore struct{ store.Store }

func (errTicketStore) ConsumeAttachTicket(context.Context, string, time.Time) (store.AttachTicket, bool, error) {
	return store.AttachTicket{}, false, context.DeadlineExceeded
}

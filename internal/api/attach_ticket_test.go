// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestAttachTicket(t *testing.T) {
	var tix attachTickets
	run := uuid.New()
	other := uuid.New()
	now := time.Unix(1_700_000_000, 0)

	tok, err := tix.mint(run, types.ActorHuman, "alice", now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok == "" {
		t.Fatal("mint returned empty ticket")
	}

	// Wrong run id: rejected (and the ticket is burned on the attempt).
	if _, ok := tix.consume(tok, other, now); ok {
		t.Fatal("ticket redeemed against the wrong run id")
	}
	if _, ok := tix.consume(tok, run, now); ok {
		t.Fatal("ticket survived a burned redemption attempt (must be single-use)")
	}

	// Fresh ticket: redeems once, carries attribution, then is gone.
	tok2, _ := tix.mint(run, types.ActorHuman, "bob", now)
	tk, ok := tix.consume(tok2, run, now)
	if !ok {
		t.Fatal("valid ticket did not redeem")
	}
	if tk.principal != "bob" || tk.actorType != types.ActorHuman {
		t.Fatalf("attribution lost: got %v/%q", tk.actorType, tk.principal)
	}
	if _, ok := tix.consume(tok2, run, now); ok {
		t.Fatal("ticket redeemed twice")
	}

	// Expiry: a ticket presented after its TTL is rejected.
	tok3, _ := tix.mint(run, types.ActorHuman, "carol", now)
	if _, ok := tix.consume(tok3, run, now.Add(attachTicketTTL+time.Second)); ok {
		t.Fatal("expired ticket redeemed")
	}
}

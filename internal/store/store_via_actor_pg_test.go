// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A delegated ticket's agent survives the store: the ticket lane replays it onto the rows it writes.
// A ticket with no agent reads back with none.
func TestPG_AttachTicket_ReplaysTheActor(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	actor := types.DelegationActor{Sub: "agent-sub", Iss: "https://idp.example"}
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New(), Actor: &actor}

	tok := uuid.NewString()
	if err := st.MintAttachTicket(ctx, tok, store.AttachTicket{RunID: uuid.New(), ActorType: types.ActorHuman, Principal: "alice", Via: &via},
		now, now.Add(30*time.Second)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, ok, err := st.ConsumeAttachTicket(ctx, tok, now)
	if err != nil || !ok || got.Via == nil {
		t.Fatalf("consume: %+v ok=%v err=%v", got, ok, err)
	}
	if got.Via.Delegate != via.Delegate || got.Via.Grant != via.Grant || got.Via.Actor == nil || *got.Via.Actor != actor {
		t.Fatalf("via = %+v, want %+v with the actor", got.Via, via)
	}

	plain := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	tok = uuid.NewString()
	if err := st.MintAttachTicket(ctx, tok, store.AttachTicket{RunID: uuid.New(), ActorType: types.ActorHuman, Principal: "alice", Via: &plain},
		now, now.Add(30*time.Second)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, ok, err = st.ConsumeAttachTicket(ctx, tok, now)
	if err != nil || !ok || got.Via == nil || got.Via.Actor != nil {
		t.Fatalf("a ticket with no agent read back %+v ok=%v err=%v, want the portal and no actor", got.Via, ok, err)
	}
}

// CreateRun writes created_via_actor and every read of the run returns it; a run with none reads nil.
func TestPG_CreateRun_PersistsCreatedViaActor(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	portal := uuid.New()
	actor := types.DelegationActor{Sub: "agent-sub", Iss: "https://idp.example"}

	withActor := newRun(types.RunPending)
	withActor.CreatedVia, withActor.CreatedViaActor = &portal, &actor
	created := persistRun(t, ctx, pool, withActor)
	if created.CreatedViaActor == nil || *created.CreatedViaActor != actor {
		t.Fatalf("CreateRun returned created_via_actor %+v, want %+v", created.CreatedViaActor, actor)
	}
	got, err := st.GetRun(ctx, withActor.ID)
	if err != nil || got.CreatedViaActor == nil || *got.CreatedViaActor != actor {
		t.Fatalf("GetRun created_via_actor = %+v err=%v, want %+v", got.CreatedViaActor, err, actor)
	}

	without := newRun(types.RunPending)
	without.CreatedVia = &portal
	persistRun(t, ctx, pool, without)
	got, err = st.GetRun(ctx, without.ID)
	if err != nil || got.CreatedViaActor != nil {
		t.Fatalf("a run with no agent read created_via_actor %+v err=%v, want nil", got.CreatedViaActor, err)
	}
}

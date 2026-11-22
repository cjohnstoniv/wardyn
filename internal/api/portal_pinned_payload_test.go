// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func unsealedPin(t *testing.T, actorType types.ActorType, actor string) []byte {
	t.Helper()
	raw, err := json.Marshal(pinnedEnvelope{ActorType: actorType, Actor: actor, Data: json.RawMessage(`{"k":"v"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// With a key service configured, a human actor's pinned payload is always sealed, so an unsealed one is refused
// (a database writer's plaintext actor is never chained as an audit event); a system actor's stays unsealed, and
// with no key service any actor's is.
func TestOpenPinned_UnsealedHumanRefusedWithKeyService(t *testing.T) {
	step := pinnedRow{jobID: uuid.New(), kind: store.JobKindSuspend, eventID: uuid.New()}
	srv := newHarness(t).srv

	if p, st, err := srv.openPinned(t.Context(), step, unsealedPin(t, types.ActorHuman, "alice")); err != nil || st != pinOpen || p.Actor != "alice" {
		t.Fatalf("no key service, human actor: actor %q state %v err %v, want it open as written", p.Actor, st, err)
	}

	srv.cfg.SubjectKeys = subjectkey.New(nil, subjectkey.Resolver{})
	if p, st, err := srv.openPinned(t.Context(), step, unsealedPin(t, types.ActorHuman, "mallory")); err == nil || st != pinAbsent || p.Actor != "" {
		t.Errorf("key service, unsealed human actor: actor %q state %v err %v, want it refused", p.Actor, st, err)
	}
	if p, st, err := srv.openPinned(t.Context(), step, unsealedPin(t, types.ActorSystem, portalLifecycleActor)); err != nil || st != pinOpen || p.Actor != portalLifecycleActor {
		t.Errorf("key service, system actor: actor %q state %v err %v, want it open as written", p.Actor, st, err)
	}
}

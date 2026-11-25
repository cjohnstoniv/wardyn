// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: a pinned payload sealed for one step's row opens for that row alone. The same sealed bytes moved onto
// another job row, another kind or another pinned event are refused, as a database writer copying a payload between a
// person's steps would do.
func TestPinnedPayload_SealedForOneStepDoesNotOpenForAnother_PG(t *testing.T) {
	l := newPortalLife(t)
	withSubjectKeys(t, l)
	srv := l.h.srv
	rowA := pinnedRow{jobID: uuid.New(), kind: store.JobKindSuspend, eventID: uuid.New()}
	raw, err := srv.sealPinned(t.Context(), rowA, pinnedPayload{ActorType: types.ActorHuman, Actor: "first-admin", Data: []byte(`{"k":"v"}`)})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if p, st, err := srv.openPinned(t.Context(), rowA, raw); err != nil || st != pinOpen || p.Actor != "first-admin" {
		t.Fatalf("control: the row it was sealed for: actor %q state %v err %v, want it open", p.Actor, st, err)
	}
	for name, other := range map[string]pinnedRow{
		"another job row":      {jobID: uuid.New(), kind: rowA.kind, eventID: rowA.eventID},
		"another kind":         {jobID: rowA.jobID, kind: store.JobKindPurge, eventID: rowA.eventID},
		"another pinned event": {jobID: rowA.jobID, kind: rowA.kind, eventID: uuid.New()},
	} {
		if p, st, err := srv.openPinned(t.Context(), other, raw); err == nil || st != pinAbsent || p.Actor != "" {
			t.Errorf("%s: actor %q state %v err %v, want it refused", name, p.Actor, st, err)
		}
	}
}

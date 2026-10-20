// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_TakeoverTakesHandsBackAndRefusesTheRest pins the human_takeover CAS (0208): the first
// take over moves PENDING to APPROVED with a holder, a second person replaces the holder (and is
// told whom), the holder taking over again is refused, only the current holder hands back (the
// replaced one is refused and leaves the row open), hand back stamps once, and nothing moves a
// handed-back row or a row of another kind.
func TestPG_TakeoverTakesHandsBackAndRefusesTheRest(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	st := approvalStore{pg}
	runID := uuid.New()
	if _, err := pg.CreateRun(ctx, types.AgentRun{
		ID: runID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), CreatedBy: "test-user",
		Agent: "claude-code", Repo: "test/repo", Task: "t", ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://test/agent-run/" + runID.String(), RunnerTarget: "docker",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	ap, err := approval.RequestApproval(ctx, st, types.ApprovalRequest{RunID: runID, Kind: types.ApprovalHumanTakeover,
		RequestedScope: json.RawMessage(`{"reason":"the sign-in sends a code to a phone"}`)})
	if err != nil || ap.TakeoverHolder != "" || ap.HandedBackAt != nil {
		t.Fatalf("raise: %+v, %v", ap, err)
	}
	if _, err := pg.HandBackApproval(ctx, ap.ID, "developer", time.Now()); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("hand back on a PENDING row: %v, want ErrAlreadyDecided", err)
	}
	got, prev, err := pg.TakeOverApproval(ctx, ap.ID, "developer")
	if err != nil || prev != "" || got.State != types.ApprovalApproved || got.TakeoverHolder != "developer" ||
		got.DecidedBy != "developer" || got.DecidedAt == nil {
		t.Fatalf("first take over: %+v prev=%q %v", got, prev, err)
	}
	if _, _, err := pg.TakeOverApproval(ctx, ap.ID, "developer"); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("the holder taking over again: %v, want ErrAlreadyDecided", err)
	}
	got, prev, err = pg.TakeOverApproval(ctx, ap.ID, "priya")
	if err != nil || prev != "developer" || got.TakeoverHolder != "priya" || got.DecidedBy != "developer" {
		t.Fatalf("second person: %+v prev=%q %v; want priya holding, developer still the decider", got, prev, err)
	}
	// developer's hand back lands after priya took over: it must not close priya's takeover.
	if _, err := pg.HandBackApproval(ctx, ap.ID, "developer", time.Now()); !errors.Is(err, store.ErrTakeoverNotHolder) {
		t.Fatalf("hand back by the replaced holder: %v, want ErrTakeoverNotHolder", err)
	}
	if read, err := pg.GetApproval(ctx, ap.ID); err != nil || read.HandedBackAt != nil || read.TakeoverHolder != "priya" {
		t.Fatalf("after the replaced holder's hand back: %+v, %v; want priya still holding", read, err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	back, err := pg.HandBackApproval(ctx, ap.ID, "priya", at)
	if err != nil || back.HandedBackAt == nil || !back.HandedBackAt.Equal(at) {
		t.Fatalf("hand back: %+v, %v", back, err)
	}
	if _, err := pg.HandBackApproval(ctx, ap.ID, "priya", at); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("second hand back: %v, want ErrAlreadyDecided", err)
	}
	if _, _, err := pg.TakeOverApproval(ctx, ap.ID, "someone"); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("take over after hand back: %v, want ErrAlreadyDecided", err)
	}
	read, err := pg.GetApproval(ctx, ap.ID)
	if err != nil || read.TakeoverHolder != "priya" || read.HandedBackAt == nil {
		t.Fatalf("read back: %+v, %v", read, err)
	}
	tool, err := approval.RequestApproval(ctx, st, types.ApprovalRequest{RunID: runID, Kind: types.ApprovalToolCall,
		RequestedScope: json.RawMessage(`{"tool":"Bash","cmd":"ls"}`)})
	if err != nil {
		t.Fatalf("raise tool_call: %v", err)
	}
	if _, _, err := pg.TakeOverApproval(ctx, tool.ID, "developer"); !errors.Is(err, store.ErrAlreadyDecided) {
		t.Fatalf("take over a tool_call: %v, want ErrAlreadyDecided", err)
	}
	if _, _, err := pg.TakeOverApproval(ctx, uuid.New(), "developer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent row: %v, want ErrNotFound", err)
	}
	if _, err := pg.HandBackApproval(ctx, uuid.New(), "developer", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hand back an absent row: %v, want ErrNotFound", err)
	}
}

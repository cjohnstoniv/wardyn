// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_DecideApprovalRefusesAPastExpiry: a PENDING row whose read-time expires_at has passed is no
// longer a question the run has open, so a decision is refused (ErrAlreadyDecided) and the row is left
// for the sweeper; the sweeper's own EXPIRED transition, and a row still inside its wait, are unchanged.
func TestPG_DecideApprovalRefusesAPastExpiry(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, func() types.AgentRun {
		r := newRun(types.RunRunning)
		r.WaitBudgetSec = 60
		return r
	}())
	n := 0
	decide := func(state types.ApprovalState, requestedAgo time.Duration) error {
		req := newApproval(run.ID, time.Now().UTC().Add(-requestedAgo))
		req.RequestedScope = json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))
		n++
		ap, err := st.CreateApproval(ctx, req)
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		by := "u"
		if state == types.ApprovalExpired {
			by = "system"
		}
		_, err = st.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: state, DecidedBy: by})
		return err
	}

	for _, state := range []types.ApprovalState{types.ApprovalApproved, types.ApprovalDenied} {
		if err := decide(state, time.Hour); !errors.Is(err, store.ErrAlreadyDecided) {
			t.Errorf("%s past its expiry: err = %v, want ErrAlreadyDecided", state, err)
		}
		if err := decide(state, time.Second); err != nil {
			t.Errorf("%s inside its wait: err = %v, want nil", state, err)
		}
	}
	if err := decide(types.ApprovalExpired, time.Hour); err != nil {
		t.Errorf("the sweeper's EXPIRED past the expiry: err = %v, want nil", err)
	}
	if _, err := st.DecideApproval(ctx, uuid.New(), types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "u"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
}

// The run-end cascade is a system CANCELLED write: it must land on a row whose run's end has already
// passed, or the row stays PENDING for a run that is over. A person's decision past that end stays
// refused, and so does a person's CANCELLED: only the system actor takes the exemption.
func TestPG_CancelForRunCancelsPastTheRunsEnd(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, func() types.AgentRun {
		r := newRun(types.RunRunning)
		end := time.Now().UTC().Add(-time.Minute)
		r.EndsAt = &end
		return r
	}())
	ap, err := st.CreateApproval(ctx, newApproval(run.ID, time.Now().UTC().Add(-time.Hour)))
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	for _, state := range []types.ApprovalState{types.ApprovalApproved, types.ApprovalCancelled} {
		_, err := approval.Decide(ctx, approvalStore{st}, ap.ID, types.ActorHuman, types.ApprovalDecision{State: state, DecidedBy: "u"})
		if !errors.Is(err, approval.ErrAlreadyDecided) {
			t.Fatalf("a person's %s past the run's end: err = %v, want ErrAlreadyDecided", state, err)
		}
	}

	byKind, err := approval.CancelForRun(ctx, approvalStore{st}, run.ID, "run ended")
	if err != nil {
		t.Fatalf("CancelForRun: %v", err)
	}
	if got := approval.TallyKey(ap); byKind[got] != 1 {
		t.Errorf("byKind = %v, want %s counted once", byKind, got)
	}
	got, err := st.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if got.State != types.ApprovalCancelled {
		t.Errorf("state = %s, want CANCELLED", got.State)
	}
}

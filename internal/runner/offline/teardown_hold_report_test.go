// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// heldRow is run id's row of one full state inventory.
func heldRow(t *testing.T, m *Manager, id uuid.UUID) runnerwire.RunState {
	t.Helper()
	var got *runnerwire.RunState
	if err := m.StatePages(t.Context(), func(page runnerwire.StatePage) error {
		for _, row := range page.Runs {
			if row.RunID == id {
				got = &row
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("no state row for run %s", id)
	}
	return *got
}

// The state page tells the org that the runner holds a run for a kill it could not see finish, and,
// once that kill finished (by the session's retry or by a later kill), that it cleared and when. The
// cleared report stays on every later page; a new hold of the same run reports open again.
func TestStatePageReportsATeardownHoldAndItsClearing(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	m.Offline(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	peer, receipts := receiptPeer(ctx, t)
	unconfirmed := &runner.TeardownUnconfirmedError{Err: errors.New("docker: the macOS VM helper is still running")}

	if h := heldRow(t, m, id).TeardownHold; h != nil {
		t.Fatalf("a run never held reports %+v", h)
	}
	sub.killErr = unconfirmed
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{pendingContainment(m, id, types.RunnerActionKill)}); err != nil {
		t.Fatal(err)
	}
	if got := <-receipts; got.Outcome != types.RunnerActionRefused {
		t.Fatalf("receipt %+v, want the held kill refused", got)
	}
	open := heldRow(t, m, id).TeardownHold
	if open == nil || open.Kind != types.RunnerActionKill || open.Since.IsZero() || open.ClearedAt != nil {
		t.Fatalf("held run reports %+v, want an open kill hold", open)
	}

	// A later kill that finishes clears the hold (settleApplied), with the hold's start kept.
	sub.killErr = nil
	if _, err := m.apply(ctx, pendingContainment(m, id, types.RunnerActionKill)); err != nil {
		t.Fatal(err)
	}
	cleared := heldRow(t, m, id).TeardownHold
	if cleared == nil || cleared.Kind != types.RunnerActionKill || !cleared.Since.Equal(open.Since) || cleared.ClearedAt == nil || cleared.ClearedAt.Before(open.Since) {
		t.Fatalf("after the kill finished the run reports %+v, want the hold cleared", cleared)
	}
	if again := heldRow(t, m, id).TeardownHold; again == nil || again.ClearedAt == nil || !again.ClearedAt.Equal(*cleared.ClearedAt) {
		t.Fatalf("a later page reports %+v, want the same cleared hold", again)
	}

	// Held again, then cleared by the next session's retry.
	sub.killErr = unconfirmed
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{pendingContainment(m, id, types.RunnerActionKill)}); err != nil {
		t.Fatal(err)
	}
	<-receipts
	if h := heldRow(t, m, id).TeardownHold; h == nil || h.ClearedAt != nil {
		t.Fatalf("held again the run reports %+v, want the hold open", h)
	}
	sub.killErr = nil
	if err := m.Pending(ctx, peer, nil); err != nil {
		t.Fatal(err)
	}
	if h := heldRow(t, m, id).TeardownHold; h == nil || h.ClearedAt == nil {
		t.Fatalf("after the retry finished the kill the run reports %+v, want the hold cleared", h)
	}
}

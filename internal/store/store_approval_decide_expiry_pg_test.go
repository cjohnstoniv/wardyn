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
		_, err = st.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: state, DecidedBy: "u"})
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

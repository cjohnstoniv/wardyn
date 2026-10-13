// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// spyRower is a queryRower that is not a transaction (a pool is one).
type spyRower struct{ calls int }

func (s *spyRower) QueryRow(context.Context, string, ...any) pgx.Row {
	s.calls++
	return errRow{err: errors.New("spy row")}
}

// DR-0097: an intent is written only on the run's own transaction. Handed a
// pool-like writer, both the person lock and the insert refuse before any SQL.
func TestLaunchIntentRefusesANonTransactionalWriter(t *testing.T) {
	run := types.AgentRun{CreatedBy: "alice", LaunchIntent: json.RawMessage(`{}`)}
	q := &spyRower{}
	if _, err := lockLaunchIntent(context.Background(), q, run); !errors.Is(err, errLaunchIntentOutsideTx) {
		t.Errorf("lock on a non-transaction = %v, want errLaunchIntentOutsideTx", err)
	}
	if _, err := insertLaunchIntent(context.Background(), q, run, run.LaunchIntent); !errors.Is(err, errLaunchIntentOutsideTx) {
		t.Errorf("insert on a non-transaction = %v, want errLaunchIntentOutsideTx", err)
	}
	if q.calls != 0 {
		t.Errorf("a non-transactional writer ran %d statement(s)", q.calls)
	}
	// A run with no intent takes nothing, wherever it is written.
	if keep, err := lockLaunchIntent(context.Background(), q, types.AgentRun{}); keep || err != nil || q.calls != 0 {
		t.Errorf("no intent: keep %v err %v calls %d", keep, err, q.calls)
	}
}

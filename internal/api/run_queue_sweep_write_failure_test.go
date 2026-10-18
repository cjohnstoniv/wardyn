// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queueWriteStore serves the queue sweep one row of each kind and fails the one write that failing names.
type queueWriteStore struct {
	queueListStore
	run     types.AgentRun
	failing string
}

func (q queueWriteStore) write(name string) error {
	if q.failing == name {
		return errors.New("write " + name + " failed")
	}
	return nil
}

func (q queueWriteStore) ExpiredQueueEntries(context.Context, time.Time, int) ([]store.RunQueueEntry, error) {
	return []store.RunQueueEntry{{RunID: q.run.ID}}, nil
}

func (q queueWriteStore) ExitQueuedRun(context.Context, uuid.UUID, types.RunState, string, string) (bool, error) {
	return q.failing == "", q.write("end")
}

func (q queueWriteStore) StalePromotedRuns(context.Context, time.Duration) ([]types.AgentRun, error) {
	return []types.AgentRun{q.run}, nil
}

func (q queueWriteStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return false, q.write("fail")
}

// GetRun is the re-read after a transition that did not apply: still PENDING when the write itself
// failed, otherwise already ended by someone else, which is no failure.
func (q queueWriteStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	run := q.run
	if q.failing != "fail" {
		run.State = types.RunKilled
	}
	return run, nil
}

func (q queueWriteStore) QueuePoolIDs(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{uuid.New()}, nil
}

func (q queueWriteStore) PromoteRunQueue(context.Context, store.PromotePass, store.QueueAdmit) ([]store.PromotedRun, []store.QueueHeld, error) {
	return nil, nil, q.write("promote")
}

// A waiting run the sweep cannot end, a stale promoted run it cannot fail and a pool it cannot promote
// are each returned, so the sweeper's health shows them; a sweep whose writes all work returns nil.
func TestSweepRunQueueReturnsAFailedWrite(t *testing.T) {
	for _, tc := range []struct{ failing, want string }{
		{"", ""},
		{"end", "write end failed"},
		{"fail", "the transition did not apply"},
		{"promote", "write promote failed"},
	} {
		t.Run("failing "+tc.failing, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.Store = queueWriteStore{run: types.AgentRun{ID: uuid.New(), State: types.RunPending}, failing: tc.failing}
			err := h.srv.SweepRunQueue(context.Background())
			if tc.failing == "" {
				if err != nil {
					t.Fatalf("SweepRunQueue = %v, want nil when every write works", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SweepRunQueue = %v, want an error carrying %q", err, tc.want)
			}
		})
	}
}

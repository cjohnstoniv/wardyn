// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A person's queued runs cancelled while one of them times out in the queue, on a real Postgres.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queueCancelRace runs a hook at the two points the batch reads between listing a person's waiting runs and
// claiming each one's kill.
type queueCancelRace struct {
	store.PG
	afterList func()
	afterRead func(uuid.UUID)
}

func (s *queueCancelRace) QueueOwnerEntries(ctx context.Context, owner string) ([]store.RunQueueEntry, error) {
	out, err := s.PG.QueueOwnerEntries(ctx, owner)
	if hook := s.afterList; hook != nil {
		s.afterList = nil
		hook()
	}
	return out, err
}

func (s *queueCancelRace) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	out, err := s.PG.GetRun(ctx, id)
	if hook := s.afterRead; hook != nil {
		s.afterRead = nil
		hook(id)
	}
	return out, err
}

// A waiting run that times out after the batch listed it, whether before it read the run or between that read
// and the kill's claim, keeps its FAILED end: no kill row, no teardown, and it is not counted as cancelled.
// Catches a batch that kills from the state it read, which turns FAILED into KILLED and counts a run the
// timeout had already ended.
func TestPG_QueueCancelOfAPersonLeavesARunThatTimedOutFirst(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(race *queueCancelRace, expire func(uuid.UUID), id uuid.UUID)
	}{
		{"before the run is read", func(race *queueCancelRace, expire func(uuid.UUID), id uuid.UUID) {
			race.afterList = func() { expire(id) }
		}},
		{"between the read and the claim", func(race *queueCancelRace, expire func(uuid.UUID), id uuid.UUID) {
			race.afterRead = expire
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newQueueEnv(t)
			pool := e.cappedPool("Busy", 1)
			e.occupy(pool)
			waiting := e.queuedLaunch(e.person, pool, "")
			race := &queueCancelRace{PG: e.pg}
			expire := func(id uuid.UUID) {
				applied, err := e.srv.exitQueued(ctx, id, types.RunFailed, statusDetailQueuePrefix+string(types.RunEndQueueTimeout), "the wait ran out")
				if err != nil || !applied {
					t.Errorf("expire the waiting run = %v, %v; want it applied", applied, err)
				}
			}
			tc.setup(race, expire, waiting.ID)
			prev := e.srv.cfg.Store
			e.srv.cfg.Store = race
			defer func() { e.srv.cfg.Store = prev }()

			n, err := e.srv.cancelQueuedRunsOf(ctx, e.personP, queueCancelSessionRevoked)
			if err != nil {
				t.Fatalf("cancelQueuedRunsOf: %v", err)
			}
			got := e.run(waiting.ID)
			if n != 0 || got.State != types.RunFailed || got.StatusDetail != statusDetailQueuePrefix+string(types.RunEndQueueTimeout) {
				t.Errorf("cancelled %d; the run is %s %q; want 0 cancelled and the timeout's FAILED end left as it was", n, got.State, got.StatusDetail)
			}
			if rows := e.audits(waiting.ID, "run.kill"); len(rows) != 0 {
				t.Errorf("%d run.kill rows for a run the timeout had ended; want none", len(rows))
			}
		})
	}
}

// A person's run still waiting is cancelled and counted once, alongside the run the timeout took.
func TestPG_QueueCancelOfAPersonStillKillsTheRunsStillWaiting(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	first := e.queuedLaunch(e.person, pool, "")
	second := e.queuedLaunch(e.person, pool, "")
	race := &queueCancelRace{PG: e.pg}
	race.afterList = func() {
		if applied, err := e.srv.exitQueued(ctx, first.ID, types.RunFailed, statusDetailQueuePrefix+string(types.RunEndQueueTimeout), "the wait ran out"); err != nil || !applied {
			t.Errorf("expire the first run = %v, %v", applied, err)
		}
	}
	prev := e.srv.cfg.Store
	e.srv.cfg.Store = race
	defer func() { e.srv.cfg.Store = prev }()
	n, err := e.srv.cancelQueuedRunsOf(ctx, e.personP, queueCancelSessionRevoked)
	if err != nil || n != 1 {
		t.Fatalf("cancelQueuedRunsOf = %d, %v; want exactly the run still waiting", n, err)
	}
	if e.run(first.ID).State != types.RunFailed || e.run(second.ID).State != types.RunKilled || e.queueRows(second.ID) != 0 {
		t.Errorf("first is %s, second is %s with %d queue rows; want FAILED, KILLED, none", e.run(first.ID).State, e.run(second.ID).State, e.queueRows(second.ID))
	}
}
